package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/conventional"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/effectiveconfig"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/intent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/prbody"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PRStep creates or updates a pull request via the provider CLI or API.
type PRStep struct{}

type prContent struct {
	Title       string `json:"title"`
	Summary     string `json:"summary,omitempty"`
	WhatChanged string `json:"what_changed,omitempty"`
	// Body is the fully assembled provider payload. It is also accepted from
	// older test/fallback producers while the structured agent schema uses the
	// three fields above.
	Body string `json:"body,omitempty"`
	// OwnedPatches is publication material produced by hooks.pr_body. It is
	// never part of the drafting agent's JSON schema.
	OwnedPatches              prbody.PatchSet `json:"-"`
	EffectiveConfigDisclosure string          `json:"-"`
}

var prContentSchema = json.RawMessage(`{
	"type": "object",
		"properties": {
			"title": {"type": "string", "description": "Conventional commit PR title, e.g. fix(scope): short description"},
			"summary": {"type": "string", "description": "Self-contained GitHub-flavored Markdown summary with no section heading. Format code identifiers with backticks."},
			"what_changed": {"type": "string", "description": "GitHub-flavored Markdown describing concrete branch changes, with no section heading."}
		},
		"required": ["title", "summary", "what_changed"]
	}`)

const (
	githubPullRequestBodyHardLimitChars = 65536
	// Count bytes, not runes, so multi-byte markdown stays within the shared
	// publication ceiling with room for provider-side formatting drift.
	maxPullRequestBodyBytes      = scm.MaxPRBodyBytes
	ownedSectionEnvelopeBudget   = 512
	maxBuiltInPRContentBytes     = maxPullRequestBodyBytes - ownedSectionEnvelopeBudget
	minLatestPipelineUpdateBytes = 256
)

type pipelineUpdateGroup struct {
	header string
	units  []string
	footer string
}

func (s *PRStep) Name() types.StepName { return types.StepPR }

func (s *PRStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	ctx := sctx.Ctx

	branch := sctx.Run.Branch
	if strings.HasPrefix(branch, "refs/heads/") {
		branch = strings.TrimPrefix(branch, "refs/heads/")
	}
	if branch == sctx.Repo.DefaultBranch {
		sctx.Log(fmt.Sprintf("skipping PR creation on default branch %s", branch))
		return &pipeline.StepOutcome{Skipped: true, SkipReason: fmt.Sprintf("No pull request was opened because the run is on the default branch %s.", branch)}, nil
	}
	provider := scm.DetectProviderContext(ctx, sctx.Repo.UpstreamURL)
	host, skipReason := buildHost(sctx, provider)
	if host == nil {
		sctx.Log(fmt.Sprintf("skipping PR creation: %s", skipReason))
		return &pipeline.StepOutcome{Skipped: true, SkipReason: "No pull request was opened: " + skipReason + "."}, nil
	}
	if err := host.Available(ctx); err != nil {
		sctx.Log(fmt.Sprintf("skipping PR creation: %v", err))
		return &pipeline.StepOutcome{Skipped: true, SkipReason: fmt.Sprintf("No pull request was opened because the provider host was unavailable: %v.", err)}, nil
	}
	if err := requirePRBodyReadRevision(host); err != nil {
		return nil, fmt.Errorf("provider cannot safely publish PR bodies: %w", err)
	}

	defaultBranch := strings.TrimSpace(sctx.Repo.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	baseBranch := refreshBaseBranch(sctx, defaultBranch)

	sctx.Log(fmt.Sprintf("checking for existing pull request on branch %s...", branch))
	existing, err := host.FindPR(ctx, branch, "")
	if err != nil {
		return nil, err
	}
	if existing != nil {
		sctx.Log(fmt.Sprintf("pull request already exists: %s", describePR(existing)))
		updated := existing
		retargeting := strings.TrimSpace(existing.Base) != "" && strings.TrimSpace(existing.Base) != baseBranch
		baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, baseBranch)
		content, buildErr := s.buildPRContent(sctx, branch, baseBranch, baseSHA, provider, scm.MaxPRBodyChars(provider), existing)
		if buildErr != nil {
			return nil, buildErr
		}
		patches, err := ownedPatchesForPRContent(content, scm.MaxPRBodyChars(provider))
		if err != nil {
			return nil, err
		}
		limits := prBodyValidationLimits(scm.MaxPRBodyChars(provider), content.EffectiveConfigDisclosure)
		bodyUpdate, err := prepareOwnedPRBodyUpdate(ctx, host, updated, patches, limits)
		if err != nil {
			if content.EffectiveConfigDisclosure != "" && errors.Is(err, prbody.ErrOversize) {
				return nil, effectiveConfigBodyOverflowError(len(content.EffectiveConfigDisclosure), len(bodyUpdate.candidate))
			}
			return nil, fmt.Errorf("update pull request body: %w", err)
		}
		baseUpdate := ""
		if retargeting {
			baseUpdate = baseBranch
		}
		if err := publishOwnedPRBodyUpdate(ctx, host, updated, bodyUpdate, baseUpdate); err != nil {
			if retargeting {
				return nil, fmt.Errorf("retarget pull request to %s and update owned body: %w", baseBranch, err)
			}
			return nil, fmt.Errorf("update pull request body: %w", err)
		}
		sctx.Log("updated owned pull request sections and verified the hosted body")
		if updated != nil && updated.URL != "" {
			if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, updated.URL); err != nil {
				slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", updated.URL, "err", err)
			}
			return &pipeline.StepOutcome{PRURL: updated.URL}, nil
		}
		return &pipeline.StepOutcome{}, nil
	}

	// Resolve the complete owned branch delta for the initial marked body.
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, baseBranch)
	content, err := s.buildPRContent(sctx, branch, baseBranch, baseSHA, provider, scm.MaxPRBodyChars(provider), nil)
	if err != nil {
		return nil, err
	}

	patches, err := ownedPatchesForPRContent(content, scm.MaxPRBodyChars(provider))
	if err != nil {
		return nil, err
	}
	body, err := prbody.NewOwnedDocument(patches)
	if err != nil {
		return nil, fmt.Errorf("build owned pull request body: %w", err)
	}
	limits := prBodyValidationLimits(scm.MaxPRBodyChars(provider), content.EffectiveConfigDisclosure)
	if err := prbody.ValidateOwnedDocument(body, limits); err != nil {
		return nil, fmt.Errorf("validate pull request body: %w", err)
	}

	sctx.Log("creating pull request...")
	created, err := host.CreatePR(ctx, branch, baseBranch, scm.PRContent{Title: content.Title, Body: body})
	if err != nil {
		return nil, err
	}
	if created == nil || (strings.TrimSpace(created.URL) == "" && strings.TrimSpace(created.Number) == "") {
		return nil, errors.New("provider created a pull request without an identity; hosted body cannot be verified")
	}
	if err := verifyCreatedPRBody(ctx, host, created, body, limits); err != nil {
		return nil, fmt.Errorf("verify created pull request body: %w", err)
	}
	sctx.Log("verified created pull request body")
	if strings.TrimSpace(created.URL) == "" {
		return &pipeline.StepOutcome{}, nil
	}
	sctx.Log(fmt.Sprintf("created pull request: %s", created.URL))
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, created.URL); err != nil {
		slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", created.URL, "err", err)
	}
	return &pipeline.StepOutcome{PRURL: created.URL}, nil
}

func ownedPatchesForPRContent(content prContent, bodyLimit int) (prbody.PatchSet, error) {
	if len(content.OwnedPatches.Sections) > 0 {
		return content.OwnedPatches, nil
	}
	body := content.Body
	disclosure := content.EffectiveConfigDisclosure
	if disclosure != "" {
		var err error
		body, err = fitEffectiveConfigDisclosure(body, disclosure, bodyLimit)
		if err != nil {
			return prbody.PatchSet{}, err
		}
	}
	empty, err := prbody.NewOwnedDocument(prbody.PatchSet{
		Version:  prbody.PatchVersion,
		Sections: []prbody.SectionPatch{{ID: "generated", Content: ""}},
	})
	if err == nil {
		if bodyLimit > 0 {
			budget := bodyLimit - scm.PRBodyLen(empty)
			if budget < 0 {
				budget = 0
			}
			body = scm.ClampPRBody(body, budget)
		}
		budget := maxPullRequestBodyBytes - len(empty)
		if budget < 0 {
			budget = 0
		}
		if len(body) > budget {
			body = truncateTextAtLineBoundary(body, budget, essentialPRBodyTruncationMarker())
		}
	}
	return prbody.PatchSet{
		Version:  prbody.PatchVersion,
		Sections: []prbody.SectionPatch{{ID: "generated", Content: body}},
	}, nil
}

func prBodyValidationLimits(bodyLimit int, disclosure string) prbody.ValidationLimits {
	limits := prbody.ValidationLimits{
		MaxBytes:     maxPullRequestBodyBytes,
		MaxUnits:     bodyLimit,
		MeasureUnits: scm.PRBodyLen,
	}
	if disclosure != "" {
		limits.SecretExemptions = []prbody.SecretExemption{{SectionID: "generated", ExactText: disclosure}}
	}
	return limits
}

func fitEffectiveConfigDisclosure(body, disclosure string, bodyLimit int) (string, error) {
	disclosureOnly := prbody.PatchSet{
		Version:  prbody.PatchVersion,
		Sections: []prbody.SectionPatch{{ID: "generated", Content: disclosure}},
	}
	fixedDocument, err := prbody.NewOwnedDocument(disclosureOnly)
	if err != nil {
		return "", fmt.Errorf("build complete effective configuration disclosure: %w", err)
	}
	if len(fixedDocument) > maxPullRequestBodyBytes || (bodyLimit > 0 && scm.PRBodyLen(fixedDocument) > bodyLimit) {
		return "", effectiveConfigBodyOverflowError(len(disclosure), len(fixedDocument))
	}

	separator := ""
	if strings.TrimSpace(body) != "" {
		separator = "\n\n"
	}
	section := body + separator + disclosure
	candidate, err := prbody.NewOwnedDocument(prbody.PatchSet{
		Version:  prbody.PatchVersion,
		Sections: []prbody.SectionPatch{{ID: "generated", Content: section}},
	})
	if err != nil {
		return "", fmt.Errorf("build pull request body with complete effective configuration: %w", err)
	}
	if err := prbody.ValidateOwnedDocument(candidate, prBodyValidationLimits(bodyLimit, disclosure)); err != nil {
		if errors.Is(err, prbody.ErrOversize) {
			return "", effectiveConfigBodyOverflowError(len(disclosure), len(candidate))
		}
		return "", fmt.Errorf("validate pull request body with complete effective configuration: %w", err)
	}
	return section, nil
}

func effectiveConfigBodyOverflowError(disclosureSize, combinedSize int) error {
	if disclosureSize == combinedSize {
		return fmt.Errorf("complete effective configuration cannot fit in the GitHub PR body: %d bytes exceeds the %d-byte publication limit (GitHub host limit: %d characters)", disclosureSize, maxPullRequestBodyBytes, githubPullRequestBodyHardLimitChars)
	}
	return fmt.Errorf("complete effective configuration cannot fit in the GitHub PR body: combined body is %d bytes, including %d bytes of disclosure, exceeding the %d-byte publication limit (GitHub host limit: %d characters)", combinedSize, disclosureSize, maxPullRequestBodyBytes, githubPullRequestBodyHardLimitChars)
}

func describePR(pr *scm.PR) string {
	if pr == nil {
		return ""
	}
	if pr.URL != "" {
		return pr.URL
	}
	if pr.Number != "" {
		return "#" + pr.Number
	}
	return ""
}

func (s *PRStep) buildPRContent(sctx *pipeline.StepContext, branch, baseBranch, baseSHA string, provider scm.Provider, bodyLimit int, existing *scm.PR) (prContent, error) {
	ctx := sctx.Ctx
	renderBodyLimit := bodyLimit
	if renderBodyLimit > 0 {
		renderBodyLimit -= ownedSectionEnvelopeBudget
		if renderBodyLimit < 0 {
			renderBodyLimit = 0
		}
		if renderBodyLimit > maxBuiltInPRContentBytes {
			renderBodyLimit = maxBuiltInPRContentBytes
		}
	}
	scope := prBodyScope{
		branch:     branch,
		baseBranch: baseBranch,
		baseSHA:    baseSHA,
		headSHA:    sctx.Run.HeadSHA,
		provider:   string(provider),
		bodyLimit:  bodyLimit,
	}
	// One load feeds both renderings: the built-in Pipeline markdown below and,
	// when a formatter is configured, the contract that replaces it.
	records := LoadRunRecords(sctx.DB, sctx.Run.ID)
	pipelineMD, riskLine, testingMD := s.buildPipelineSection(sctx, records)
	persisted, err := sctx.DB.GetRunNarrative(sctx.Run.ID)
	if err != nil {
		return prContent{}, fmt.Errorf("load run PR narrative: %w", err)
	}
	if persisted != nil {
		scope.baseSHA = persisted.BaseSHA
		scope.headSHA = persisted.HeadSHA
		return renderRunNarrative(sctx, provider, records, scope, *persisted, riskLine, testingMD, pipelineMD, renderBodyLimit, bodyLimit)
	}

	diffStat, _ := git.Run(ctx, sctx.WorkDir, "diff", "--stat", baseSHA+".."+sctx.Run.HeadSHA)
	finalDiff, err := git.Run(ctx, sctx.WorkDir, "diff", "--name-status", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return prContent{}, fmt.Errorf("read final branch diff: %w", err)
	}
	priorInvocations, err := sctx.DB.GetAgentInvocationsByRun(sctx.Run.ID)
	if err != nil {
		return prContent{}, fmt.Errorf("load PR drafting invocations: %w", err)
	}
	priorInvocationIDs := make(map[string]struct{}, len(priorInvocations))
	for _, invocation := range priorInvocations {
		priorInvocationIDs[invocation.ID] = struct{}{}
	}

	prompt := fmt.Sprintf(`Draft a pull request title, self-contained summary, and What Changed content for the full branch delta.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- base branch: %s

Rules:
- Cover the full branch delta, not just the latest commit.
- Title must use conventional commit format: "type(scope): description" or "type: description". Valid types: feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert. Scope is optional. Do not capitalize the type. Do not use the raw branch name.
%s
- When including a scope, it MUST be a real package/module name that exists in the codebase (for example, a directory under internal/, cmd/, or the equivalent top-level grouping for this project), identified by inspecting the changed paths. Pick the primary module affected by the change, not a secondary or incidental one.
- Keep the scope at a coarse level, not too granular: a codebase typically has fewer than 10 distinct scopes in use across its history. Prefer a broad module name (e.g. "daemon", "pipeline", "cli") over a narrow file or sub-feature name. If you cannot confidently identify a real primary module, omit the scope and use "type: description".
- Summary: self-contained GitHub-flavored Markdown explaining why the change exists and how the resulting behavior works. Use the supplied intent as context, but synthesize it with the final diff instead of copying it. Format code identifiers, symbols, files, commands, and configuration keys with backticks. Do not include a Summary heading.
- What changed: 1-3 concise GitHub-flavored Markdown bullets describing the concrete code or behavior changes in this branch, not the motivation. Do not include a What Changed heading.
- Do not include Intent, Notes, Risk Assessment, Testing, or Pipeline sections; those are recorded separately.
- Derive every summary and what_changed claim from the final diff. Inspect it directly when the paths and statuses below do not provide enough detail.
- Do not invent tests or behavior.

Diff stat:
%s

Final diff paths and statuses:
%s%s%s%s%s`, branch, baseSHA, sctx.Run.HeadSHA, baseBranch, conventional.ReleaseTypeRule, diffStat, finalDiff, userIntentPromptSection(sctx), prNotePromptSection(sctx), executionContextPromptSection(), configuredPromptSection(sctx, s.Name()))

	prompt += prBodyBudgetPromptSection(bodyLimit)

	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: prContentSchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    string(types.StepPR),
	})
	var content prContent
	validDraft := false
	source := db.NarrativeSourceAgent
	var draftingInvocationID *string
	if err != nil {
		slog.Warn("agent failed for PR content, using fallback", "error", err)
		sctx.Log("PR drafting failed; using the deterministic fallback")
		content = fallbackPRContent(sctx, finalDiff, riskLine, testingMD, pipelineMD, renderBodyLimit)
		source = db.NarrativeSourceFallback
	} else if result.Output != nil {
		if err := json.Unmarshal(result.Output, &content); err == nil {
			content.Title = strings.TrimSpace(content.Title)
			content.Summary = stripLeadingSectionHeading(strings.TrimSpace(content.Summary), "Summary")
			content.WhatChanged = stripLeadingSectionHeading(strings.TrimSpace(content.WhatChanged), "What Changed")
			legacyBody := stripGeneratedSections(unwrapNestedPRBody(strings.TrimSpace(content.Body)))
			if content.WhatChanged == "" {
				if whatChanged, ok := leadingSectionContent(legacyBody, "What Changed"); ok {
					content.WhatChanged = whatChanged
				}
			}
			structuredContent := content.Summary != "" && content.WhatChanged != ""
			if content.Title != "" && structuredContent {
				originalTitle := content.Title
				content.Title = conventional.TightenTitle(content.Title)
				if content.Title != originalTitle {
					slog.Warn("tightened agent PR title type", "from", originalTitle, "to", content.Title)
				}
				draftingInvocationID, err = latestPRDraftingInvocationID(sctx.DB, sctx.Run.ID, priorInvocationIDs)
				if err != nil {
					return prContent{}, err
				}
				if draftingInvocationID == nil {
					slog.Warn("PR drafting invocation was not recorded, using fallback")
					sctx.Log("PR drafting invocation was not recorded; using the deterministic fallback")
					content = prContent{}
				} else {
					validDraft = true
				}
			} else {
				content = prContent{}
			}
		}
	}
	if !validDraft {
		content = fallbackPRContent(sctx, finalDiff, riskLine, testingMD, pipelineMD, renderBodyLimit)
		source = db.NarrativeSourceFallback
		draftingInvocationID = nil
	}

	titleMode := db.NarrativeTitleModeFallback
	if source == db.NarrativeSourceAgent {
		titleMode = db.NarrativeTitleModeAgent
	}
	if existing != nil {
		titleMode = db.NarrativeTitleModePreserved
	}
	titleText := intent.RedactSecrets(content.Title)
	summary := intent.RedactSecrets(content.Summary)
	whatChanged := intent.RedactSecrets(content.WhatChanged)
	narrative := db.RunNarrative{
		RunID: sctx.Run.ID, Source: source, DraftingInvocationID: draftingInvocationID,
		DraftedAt: time.Now().Unix(), BaseSHA: baseSHA, HeadSHA: sctx.Run.HeadSHA,
		TitleMode: titleMode, TitleText: titleText, Summary: summary, WhatChanged: whatChanged,
	}
	if err := sctx.DB.InsertRunNarrative(narrative); err != nil {
		return prContent{}, fmt.Errorf("persist run PR narrative: %w", err)
	}
	return renderRunNarrative(sctx, provider, records, scope, narrative, riskLine, testingMD, pipelineMD, renderBodyLimit, bodyLimit)
}

func latestPRDraftingInvocationID(database *db.DB, runID string, priorIDs map[string]struct{}) (*string, error) {
	invocations, err := database.GetAgentInvocationsByRun(runID)
	if err != nil {
		return nil, fmt.Errorf("load completed PR drafting invocation: %w", err)
	}
	for i := len(invocations) - 1; i >= 0; i-- {
		invocation := invocations[i]
		if _, existed := priorIDs[invocation.ID]; existed || invocation.StepName != string(types.StepPR) || invocation.Purpose != string(types.StepPR) || invocation.ExitStatus != "ok" {
			continue
		}
		id := invocation.ID
		return &id, nil
	}
	return nil, nil
}

func renderRunNarrative(sctx *pipeline.StepContext, provider scm.Provider, records RunRecords, scope prBodyScope, narrative db.RunNarrative, riskLine, testingMD, pipelineMD string, renderBodyLimit, bodyLimit int) (prContent, error) {
	content := prContent{
		Title: narrative.TitleText, Summary: narrative.Summary, WhatChanged: narrative.WhatChanged,
	}
	body := buildPRNarrative(content.Summary, content.WhatChanged)
	if renderBodyLimit > 0 {
		content.Body = assemblePRBody(sctx, body, riskLine, testingMD, pipelineMD, renderBodyLimit)
	} else {
		content.Body = buildPRBody(body, riskLine, testingMD, pipelineMD, sctx)
	}
	content = applyPRBodyHook(sctx, records, content, content.WhatChanged, scope)
	return finalizePRContent(sctx, provider, redactOutboundPRContent(content, bodyLimit))
}

func finalizePRContent(sctx *pipeline.StepContext, provider scm.Provider, content prContent) (prContent, error) {
	if provider != scm.ProviderGitHub || sctx == nil || sctx.Config == nil || !sctx.Config.EffectiveConfig.Publish || len(content.OwnedPatches.Sections) > 0 {
		return content, nil
	}
	disclosure, err := buildEffectiveConfigDisclosure(sctx)
	if err != nil {
		return prContent{}, err
	}
	content.EffectiveConfigDisclosure = disclosure
	return content, nil
}

func buildEffectiveConfigDisclosure(sctx *pipeline.StepContext) (string, error) {
	if sctx.Paths == nil || sctx.Run == nil || sctx.Run.ResolvedPolicyDigest == nil || sctx.Run.NoMistakesVersion == nil || sctx.Run.NoMistakesBuildSHA == nil {
		return "", fmt.Errorf("publish effective configuration: run artifact identity is incomplete")
	}
	artifact, err := effectiveconfig.ReadWithBinary(
		sctx.Paths,
		sctx.Run.ID,
		*sctx.Run.ResolvedPolicyDigest,
		*sctx.Run.NoMistakesVersion,
		*sctx.Run.NoMistakesBuildSHA,
	)
	if err != nil {
		return "", fmt.Errorf("publish effective configuration: %w", err)
	}
	yamlBytes, err := effectiveconfig.CommentFreeYAML(artifact.YAML)
	if err != nil {
		return "", err
	}
	fence := effectiveConfigMarkdownFence(yamlBytes)
	return fmt.Sprintf("### Effective Configuration\n\n- Run ID: `%s`\n- `policy_digest`: `%s`\n- `yaml_sha256`: `%s`\n\n%syaml\n%s%s",
		sctx.Run.ID,
		artifact.Metadata.PolicyDigest,
		artifact.Metadata.YAMLSHA256,
		fence,
		yamlBytes,
		fence,
	), nil
}

func effectiveConfigMarkdownFence(yamlBytes []byte) string {
	longest := 0
	current := 0
	for _, b := range yamlBytes {
		if b == '`' {
			current++
			if current > longest {
				longest = current
			}
			continue
		}
		current = 0
	}
	if longest < 3 {
		longest = 2
	}
	return strings.Repeat("`", longest+1)
}

func stripLeadingSectionHeading(text, heading string) string {
	content, ok := leadingSectionContent(text, heading)
	if !ok {
		return strings.TrimSpace(text)
	}
	return content
}

func leadingSectionContent(text, heading string) (string, bool) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) == 0 {
		return "", false
	}
	first := strings.TrimSpace(lines[0])
	hashes := 0
	for hashes < len(first) && hashes < 7 && first[hashes] == '#' {
		hashes++
	}
	if hashes == 0 || hashes > 6 || hashes == len(first) || (first[hashes] != ' ' && first[hashes] != '\t') {
		return "", false
	}
	name := strings.TrimSpace(first[hashes:])
	name = strings.TrimSpace(strings.TrimRight(name, "#"))
	name = strings.TrimRight(name, ":.!? ")
	if !strings.EqualFold(name, heading) {
		return "", false
	}
	return strings.TrimSpace(strings.Join(lines[1:], "\n")), true
}

func buildPRNarrative(summary, whatChanged string) string {
	var sections []string
	if summary = strings.TrimSpace(summary); summary != "" {
		sections = append(sections, "## Summary\n\n"+summary)
	}
	if whatChanged = strings.TrimSpace(whatChanged); whatChanged != "" {
		sections = append(sections, "## What Changed\n\n"+whatChanged)
	}
	return strings.Join(sections, "\n\n")
}

// redactOutboundPRContent is the last mutation of the pipeline-drafted title
// and built-in body before owned markers are generated. Built-in evidence can
// contain credentials, so it is redacted and re-bounded here. Formatter-owned
// patches are validated and rejected for possible secrets before this point;
// they are never redacted or truncated after their section hashes are chosen.
func redactOutboundPRContent(content prContent, bodyLimit int) prContent {
	content.Title = intent.RedactSecrets(content.Title)
	body := intent.RedactSecrets(content.Body)
	if bodyLimit > 0 {
		body = scm.ClampPRBody(body, bodyLimit)
	}
	if len(body) > maxPullRequestBodyBytes {
		body = truncateTextAtLineBoundary(body, maxPullRequestBodyBytes, essentialPRBodyTruncationMarker())
	}
	content.Body = body
	return content
}

func (s *PRStep) buildPipelineSection(sctx *pipeline.StepContext, records RunRecords) (string, string, string) {
	summary, riskLine := buildPipelineSummary(records.Steps, records.Rounds, records.Invocations, sctx.Run.RefreshStrategy, sctx.Run.HeadSHA)
	testingMD := BuildTestingSummaryForPR(records.Steps, records.Rounds, sctx.Repo.UpstreamURL, sctx.Run.HeadSHA, sctx.WorkDir, testEvidenceDir(sctx))
	configSources := configSourcesSummary(sctx.Run.ConfigSources)
	if summary == "" || configSources == "" {
		return summary, riskLine, testingMD
	}
	summary = strings.Replace(summary, noMistakesPRSignature+"\n\n", noMistakesPRSignature+"\n\n"+configSources, 1)
	return summary, riskLine, testingMD
}

// unwrapNestedPRBody detects when the agent returned the body as a
// serialized prContent JSON string and extracts the real markdown body.
func unwrapNestedPRBody(body string) string {
	if len(body) == 0 || body[0] != '{' {
		return body
	}
	var nested prContent
	if err := json.Unmarshal([]byte(body), &nested); err != nil {
		return body
	}
	if strings.TrimSpace(nested.Body) != "" {
		slog.Warn("agent returned nested JSON in PR body, unwrapping")
		return strings.TrimSpace(nested.Body)
	}
	return body
}

// prBodyBudgetPromptSection tells the drafting agent about a host's PR-body
// character cap so it keeps its narrative short. Notes, Risk, Testing, and
// Pipeline sections are appended deterministically, so the
// agent only controls a slice of the budget; this nudge keeps that slice small.
// Returns "" when the provider has no practical limit (bodyLimit <= 0).
func prBodyBudgetPromptSection(bodyLimit int) string {
	if bodyLimit <= 0 {
		return ""
	}
	return fmt.Sprintf("\n\n- This repository's host caps the entire PR description at %d characters. Notes, Risk Assessment, Testing, and Pipeline sections are appended automatically. Keep Summary and What Changed concise.", bodyLimit)
}

// assemblePRBody composes the final PR body from its sections and keeps it
// within bodyLimit (0 = unlimited), measured the way the host counts it.
//
// The shedding order is worst-content-first. Testing goes first: it is the
// only section that embeds artifact and log file contents and is therefore
// effectively unbounded, so an Azure DevOps PR sheds log dumps while keeping
// its Summary, Notes, What Changed, Risk, and Pipeline narrative. The agent
// attribution table goes next, as a complete unit rather than a ragged
// remnant. Only then does Pipeline evidence shrink, and it shrinks
// structurally - whole update rounds omitted oldest-first at <details>
// boundaries, newest evidence retained - because a blind tail cut through a
// collapsible block both hides the most recent evidence and leaves broken
// markup. ClampPRBody stays the last-resort backstop for a body whose
// non-Pipeline sections alone (e.g. an unusually long Summary) still overrun.
func assemblePRBody(sctx *pipeline.StepContext, narrative, riskLine, testingMD, pipelineMD string, bodyLimit int) string {
	narrative = prependNotesSection(narrative, sctx)
	sections := appendGeneratedSections(narrative, riskLine, testingMD, pipelineMD)
	full := sections
	if bodyLimit <= 0 || scm.PRBodyLen(full) <= bodyLimit {
		return full
	}
	if testingMD != "" {
		sections = appendGeneratedSections(narrative, riskLine, "", pipelineMD)
		core := sections
		if scm.PRBodyLen(core) <= bodyLimit {
			return core
		}
	}
	withoutTelemetry := pipelineSectionWithoutAgentTelemetry(pipelineMD)
	if withoutTelemetry != pipelineMD {
		sections = appendGeneratedSections(narrative, riskLine, "", withoutTelemetry)
		core := sections
		if scm.PRBodyLen(core) <= bodyLimit {
			return core
		}
	}
	if fitted := fitPipelineWithinPRBodyLimit(narrative, riskLine, withoutTelemetry, bodyLimit); fitted != "" {
		return fitted
	}
	return scm.ClampPRBody(sections, bodyLimit)
}

// fitPipelineWithinPRBodyLimit re-renders the Pipeline section with older
// update rounds omitted until the whole body fits the host's cap, keeping the
// Summary/Notes/What Changed narrative, Risk, and the newest pipeline evidence intact.
//
// The structured omission helpers budget in bytes while the host counts
// PRBodyLen units. UTF-8 never encodes a rune in fewer bytes than UTF-16
// counts units for it, so spending the unit budget as a byte budget is
// conservative: it may shed one round more than strictly necessary, never one
// fewer. Returns "" when no structured rendering fits, leaving the caller's
// blunt-clamp backstop in charge.
func fitPipelineWithinPRBodyLimit(narrative, riskLine, pipelineMD string, bodyLimit int) string {
	if pipelineMD == "" || bodyLimit <= 0 {
		return ""
	}
	prefix := appendGeneratedSections(narrative, riskLine, "", "")
	budget := bodyLimit - scm.PRBodyLen(prefix)
	if prefix != "" {
		budget -= scm.PRBodyLen("\n\n")
	}
	if budget <= 0 && strings.Contains(pipelineMD, pipelineAttestationCommentPrefix) {
		candidate := appendGeneratedSectionsToCleanBodyWithinLimit(stripGeneratedSections(narrative), riskLine, "", pipelineMD, bodyLimit)
		if scm.PRBodyLen(candidate) <= bodyLimit && strings.Contains(candidate, "## Pipeline") {
			return candidate
		}
	}
	if budget <= 0 {
		return ""
	}
	truncated := truncatePipelineSection(pipelineMD, budget)
	if truncated == "" {
		return ""
	}
	candidate := appendGeneratedSections(narrative, riskLine, "", truncated)
	if scm.PRBodyLen(candidate) > bodyLimit {
		return ""
	}
	return candidate
}

// appendGeneratedSections appends deterministic sections after the agent's body
// and applies the PR body length guard.
func appendGeneratedSections(body, riskLine, testingMD, pipelineMD string) string {
	body = stripGeneratedSections(body)
	return appendGeneratedSectionsToCleanBody(body, riskLine, testingMD, pipelineMD)
}

func buildPRBody(body, riskLine, testingMD, pipelineMD string, sctx *pipeline.StepContext) string {
	body = stripGeneratedSections(body)
	body = prependNotesSection(body, sctx)
	return appendGeneratedSectionsToCleanBody(body, riskLine, testingMD, pipelineMD)
}

func appendGeneratedSectionsToCleanBody(body, riskLine, testingMD, pipelineMD string) string {
	return appendGeneratedSectionsToCleanBodyWithinLimit(body, riskLine, testingMD, pipelineMD, maxBuiltInPRContentBytes)
}

func appendGeneratedSectionsToCleanBodyWithinLimit(body, riskLine, testingMD, pipelineMD string, maxBytes int) string {
	generatedSections := generatedEssentialSections(riskLine, testingMD)
	prefix := body + generatedSections
	if pipelineMD == "" {
		return essentialPRBodyWithinBudget(body, generatedSections, maxBytes)
	}

	separator := ""
	if prefix != "" {
		separator = "\n\n"
	}
	if len(prefix+separator+pipelineMD) <= maxBytes {
		return prefix + separator + pipelineMD
	}

	prefix = essentialPRBodyWithinPipelineBudget(body, generatedSections, pipelineMD, maxBytes)
	return appendPipelineSectionWithinLimit(prefix, pipelineMD, maxBytes)
}

func generatedEssentialSections(riskLine, testingMD string) string {
	var b strings.Builder
	if riskLine != "" {
		b.WriteString("\n\n## Risk Assessment\n\n")
		b.WriteString(riskLine)
	}
	if testingMD != "" {
		b.WriteString("\n\n")
		b.WriteString(testingMD)
	}
	return b.String()
}

func essentialPRBodyWithinLimit(body, generatedSections string) string {
	return essentialPRBodyWithinBudget(body, generatedSections, maxPullRequestBodyBytes)
}

func essentialPRBodyWithinPipelineBudget(body, generatedSections, pipelineMD string, maxBytes int) string {
	minPipeline := minimumPipelineRetainingLatestUpdate(pipelineMD)
	if minPipeline == "" {
		minPipeline = minimumPipelineOmissionSection(pipelineMD)
		if minPipeline == "" {
			return essentialPRBodyWithinBudget(body, generatedSections, maxBytes)
		}
	}

	prefixBudget := maxBytes - len(minPipeline)
	if body != "" || generatedSections != "" {
		prefixBudget -= len("\n\n")
	}
	if prefixBudget <= 0 {
		return essentialPRBodyWithinBudget(body, generatedSections, maxBytes)
	}
	return essentialPRBodyWithinBudget(body, generatedSections, prefixBudget)
}

func essentialPRBodyWithinBudget(body, generatedSections string, maxBytes int) string {
	full := body + generatedSections
	if len(full) <= maxBytes {
		return full
	}
	if generatedSections == "" {
		return truncateTextAtLineBoundary(body, maxBytes, essentialPRBodyTruncationMarker())
	}

	bodyBudget := maxBytes - len(generatedSections)
	if bodyBudget <= 0 {
		return truncateTextAtLineBoundary(generatedSections, maxBytes, essentialPRBodyTruncationMarker())
	}
	return truncatePRBodySections(body, bodyBudget, essentialPRBodyTruncationMarker()) + generatedSections
}

func appendPipelineSectionWithinLimit(prefix, pipelineMD string, maxBytes int) string {
	separator := ""
	if prefix != "" {
		separator = "\n\n"
	}
	full := prefix + separator + pipelineMD
	if len(full) <= maxBytes {
		return full
	}

	pipelineBudget := maxBytes - len(prefix) - len(separator)
	if pipelineBudget <= 0 {
		return truncateTextAtLineBoundary(prefix, maxBytes, essentialPRBodyTruncationMarker())
	}

	truncatedPipeline := truncatePipelineSection(pipelineMD, pipelineBudget)
	if truncatedPipeline == "" {
		return prefix
	}
	candidate := prefix + separator + truncatedPipeline
	if len(candidate) <= maxBytes {
		return candidate
	}
	if len(prefix) <= maxBytes {
		return prefix
	}
	return truncateTextAtLineBoundary(prefix, maxBytes, essentialPRBodyTruncationMarker())
}

func truncatePipelineSection(pipelineMD string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(pipelineMD) <= maxBytes {
		return pipelineMD
	}

	header, updates := splitPipelineSectionHeader(pipelineMD)
	_, updates = splitPipelineAgentTelemetry(updates)
	withoutTelemetry := header + updates
	if len(withoutTelemetry) <= maxBytes {
		return withoutTelemetry
	}
	groups := parsePipelineUpdateGroups(updates)
	totalUnits := countPipelineUpdateUnits(groups)
	if totalUnits == 0 {
		return pipelineConfigHeaderFallback(header, 0, maxBytes)
	}

	for omitted := 1; omitted < totalUnits; omitted++ {
		candidate := renderPipelineWithOmittedUpdates(header, groups, omitted)
		if len(candidate) <= maxBytes {
			return candidate
		}
	}

	if candidate := renderPipelineWithTruncatedLatestUpdate(header, groups, maxBytes); candidate != "" {
		return candidate
	}

	return pipelineConfigHeaderFallback(header, totalUnits, maxBytes)
}

func pipelineConfigHeaderFallback(header string, omitted, maxBytes int) string {
	if omission := pipelineOmissionSectionWithinLimit(header, omitted, maxBytes); omission != "" {
		return omission
	}
	if strings.Contains(header, pipelineConfigSourcesPrefix) && len(header) <= maxBytes {
		return header
	}
	return ""
}

func minimumPipelineOmissionSection(pipelineMD string) string {
	header, updates := splitPipelineSectionHeader(pipelineMD)
	_, updates = splitPipelineAgentTelemetry(updates)
	totalUnits := countPipelineUpdateUnits(parsePipelineUpdateGroups(updates))
	return header + pipelineUpdatesOmissionMarker(totalUnits) + "\n"
}

func minimumPipelineRetainingLatestUpdate(pipelineMD string) string {
	header, updates := splitPipelineSectionHeader(pipelineMD)
	_, updates = splitPipelineAgentTelemetry(updates)
	groups := parsePipelineUpdateGroups(updates)
	totalUnits := countPipelineUpdateUnits(groups)
	if totalUnits == 0 {
		return ""
	}

	group, unit, ok := latestPipelineUpdateUnit(groups)
	if !ok {
		return ""
	}

	omitted := totalUnits - 1
	var b strings.Builder
	b.WriteString(header)
	if omitted > 0 {
		b.WriteString(pipelineUpdatesOmissionMarker(omitted))
		b.WriteString("\n\n")
	}
	b.WriteString(group.header)

	unitBudget := len(unit)
	if unitBudget > minLatestPipelineUpdateBytes {
		unitBudget = minLatestPipelineUpdateBytes + len("\n\n") + len(pipelineLatestUpdateTruncationMarker())
	}
	if group.footer != "" {
		unitBudget += len("\n\n") + len(group.footer)
	}

	return renderPipelineWithTruncatedLatestUpdate(header, groups, b.Len()+unitBudget)
}

func pipelineOmissionSectionWithinLimit(header string, omitted, maxBytes int) string {
	markerOnly := header + pipelineUpdatesOmissionMarker(omitted) + "\n"
	if len(markerOnly) <= maxBytes {
		return markerOnly
	}
	return ""
}

func splitPipelineSectionHeader(pipelineMD string) (string, string) {
	const heading = "## Pipeline\n\n"
	if !strings.HasPrefix(pipelineMD, heading) {
		return "", pipelineMD
	}

	rest := pipelineMD[len(heading):]
	introEnd := strings.Index(rest, "\n\n")
	if introEnd < 0 {
		return heading, rest
	}

	headerEnd := len(heading) + introEnd + len("\n\n")
	if strings.HasPrefix(pipelineMD[headerEnd:], pipelineConfigSourcesPrefix) {
		if configEnd := strings.Index(pipelineMD[headerEnd:], "\n\n"); configEnd >= 0 {
			headerEnd += configEnd + len("\n\n")
		}
	}
	// Generated attestations are fixed header data, not update details. Keep
	// them when older human-readable rounds are truncated.
	rest = pipelineMD[headerEnd:]
	if strings.HasPrefix(rest, pipelineAttestationCommentPrefix) {
		if end := strings.Index(rest, pipelineAttestationCommentClosingToken); end >= 0 {
			headerEnd += end + len(pipelineAttestationCommentClosingToken)
			if strings.HasPrefix(pipelineMD[headerEnd:], "\n\n") {
				headerEnd += len("\n\n")
			}
		}
	}
	return pipelineMD[:headerEnd], pipelineMD[headerEnd:]
}

func splitPipelineAgentTelemetry(updates string) (string, string) {
	if !strings.HasPrefix(updates, pipelineAgentTelemetryTableHeader) {
		return "", updates
	}
	tableEnd := strings.Index(updates, "\n\n")
	if tableEnd < 0 {
		return updates, ""
	}
	tableEnd += len("\n\n")
	return updates[:tableEnd], updates[tableEnd:]
}

func pipelineSectionWithoutAgentTelemetry(pipelineMD string) string {
	header, updates := splitPipelineSectionHeader(pipelineMD)
	telemetry, updates := splitPipelineAgentTelemetry(updates)
	if telemetry == "" {
		return pipelineMD
	}
	return header + updates
}

func parsePipelineUpdateGroups(updates string) []pipelineUpdateGroup {
	var groups []pipelineUpdateGroup
	rest := updates
	for strings.TrimSpace(rest) != "" {
		rest = strings.TrimLeft(rest, "\n")
		if strings.HasPrefix(rest, "<details>") {
			end := strings.Index(rest, "</details>")
			if end >= 0 {
				end += len("</details>")
				if end < len(rest) && rest[end] == '\n' {
					end++
				}
				groups = append(groups, parsePipelineDetailsGroup(rest[:end]))
				rest = rest[end:]
				continue
			}
		}

		nextDetails := strings.Index(rest, "\n<details>")
		raw := rest
		if nextDetails >= 0 {
			raw = rest[:nextDetails]
			rest = rest[nextDetails+1:]
		} else {
			rest = ""
		}
		units := splitPipelineUpdateUnits(raw)
		if len(units) > 0 {
			groups = append(groups, pipelineUpdateGroup{units: units})
		}
	}
	return groups
}

func parsePipelineDetailsGroup(raw string) pipelineUpdateGroup {
	footerStart := strings.LastIndex(raw, "</details>")
	summaryEnd := strings.Index(raw, "</summary>")
	if footerStart < 0 || summaryEnd < 0 || summaryEnd > footerStart {
		return pipelineUpdateGroup{units: splitPipelineUpdateUnits(raw)}
	}

	contentStart := summaryEnd + len("</summary>")
	if strings.HasPrefix(raw[contentStart:], "\n\n") {
		contentStart += len("\n\n")
	} else if strings.HasPrefix(raw[contentStart:], "\n") {
		contentStart++
	}

	footerEnd := footerStart + len("</details>")
	if footerEnd < len(raw) && raw[footerEnd] == '\n' {
		footerEnd++
	}

	return pipelineUpdateGroup{
		header: raw[:contentStart],
		units:  splitPipelineUpdateUnits(raw[contentStart:footerStart]),
		footer: raw[footerStart:footerEnd],
	}
}

func splitPipelineUpdateUnits(content string) []string {
	var units []string
	var b strings.Builder
	for _, line := range strings.SplitAfter(content, "\n") {
		b.WriteString(line)
		if strings.TrimSpace(line) != "" {
			continue
		}
		if strings.TrimSpace(b.String()) == "" {
			b.Reset()
			continue
		}
		units = append(units, b.String())
		b.Reset()
	}
	if strings.TrimSpace(b.String()) != "" {
		units = append(units, b.String())
	}
	return units
}

func countPipelineUpdateUnits(groups []pipelineUpdateGroup) int {
	total := 0
	for _, group := range groups {
		total += len(group.units)
	}
	return total
}

func renderPipelineWithOmittedUpdates(header string, groups []pipelineUpdateGroup, omitted int) string {
	var b strings.Builder
	b.WriteString(header)
	if omitted > 0 {
		b.WriteString(pipelineUpdatesOmissionMarker(omitted))
		b.WriteString("\n\n")
	}

	remainingOmitted := omitted
	wroteGroup := false
	for _, group := range groups {
		if remainingOmitted >= len(group.units) {
			remainingOmitted -= len(group.units)
			continue
		}

		start := remainingOmitted
		remainingOmitted = 0
		units := group.units[start:]
		if len(units) == 0 {
			continue
		}
		if wroteGroup {
			b.WriteString("\n")
		}
		b.WriteString(group.header)
		for _, unit := range units {
			b.WriteString(unit)
		}
		if group.footer != "" {
			last := units[len(units)-1]
			if !strings.HasSuffix(last, "\n\n") {
				if !strings.HasSuffix(last, "\n") {
					b.WriteString("\n")
				}
				b.WriteString("\n")
			}
		}
		b.WriteString(group.footer)
		wroteGroup = true
	}

	return b.String()
}

func renderPipelineWithTruncatedLatestUpdate(header string, groups []pipelineUpdateGroup, maxBytes int) string {
	group, unit, ok := latestPipelineUpdateUnit(groups)
	if !ok {
		return ""
	}

	totalUnits := countPipelineUpdateUnits(groups)
	omitted := totalUnits - 1
	var b strings.Builder
	b.WriteString(header)
	if omitted > 0 {
		b.WriteString(pipelineUpdatesOmissionMarker(omitted))
		b.WriteString("\n\n")
	}
	b.WriteString(group.header)
	prefix := b.String()

	footerSeparatorBytes := 0
	if group.footer != "" {
		footerSeparatorBytes = len("\n\n")
	}
	unitBudget := maxBytes - len(prefix) - len(group.footer) - footerSeparatorBytes
	if unitBudget <= 0 {
		return ""
	}

	marker := pipelineLatestUpdateTruncationMarker()
	truncatedUnit := truncatePipelineUpdateAtLineBoundary(unit, unitBudget, marker)
	if truncatedUnit == "" {
		return ""
	}

	candidate := prefix + truncatedUnit
	if group.footer != "" {
		if !strings.HasSuffix(truncatedUnit, "\n\n") {
			if !strings.HasSuffix(truncatedUnit, "\n") {
				candidate += "\n"
			}
			candidate += "\n"
		}
		candidate += group.footer
	}
	if len(candidate) <= maxBytes {
		return candidate
	}
	return ""
}

func latestPipelineUpdateUnit(groups []pipelineUpdateGroup) (pipelineUpdateGroup, string, bool) {
	for i := len(groups) - 1; i >= 0; i-- {
		group := groups[i]
		for j := len(group.units) - 1; j >= 0; j-- {
			if strings.TrimSpace(group.units[j]) == "" {
				continue
			}
			return group, group.units[j], true
		}
	}
	return pipelineUpdateGroup{}, "", false
}

func pipelineUpdatesOmissionMarker(omitted int) string {
	rounds := "rounds"
	if omitted == 1 {
		rounds = "round"
	}
	return fmt.Sprintf("_... (%d earlier update %s omitted to keep the PR body within GitHub's %d-char limit; full history is in the run log.)_", omitted, rounds, githubPullRequestBodyHardLimitChars)
}

func pipelineLatestUpdateTruncationMarker() string {
	return fmt.Sprintf("_... (latest pipeline update truncated to keep the PR body within GitHub's %d-char limit; full history is in the run log.)_", githubPullRequestBodyHardLimitChars)
}

func truncateEssentialPRBodyIfNeeded(body string) string {
	if len(body) <= maxPullRequestBodyBytes {
		return body
	}
	return truncateTextAtLineBoundary(body, maxPullRequestBodyBytes, essentialPRBodyTruncationMarker())
}

func essentialPRBodyTruncationMarker() string {
	return fmt.Sprintf("_... (body truncated to keep the PR body within GitHub's %d-char limit.)_", githubPullRequestBodyHardLimitChars)
}

func truncatePRBodySections(body string, maxBytes int, marker string) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(body) <= maxBytes {
		return body
	}

	sections := splitPRBodySections(body)
	if len(sections) <= 1 {
		return truncateTextAtLineBoundary(body, maxBytes, marker)
	}

	for {
		joined := joinPRBodySections(sections)
		if len(joined) <= maxBytes {
			return joined
		}

		i := largestPRBodySectionIndex(sections)
		if i < 0 {
			return truncateTextAtLineBoundary(joined, maxBytes, marker)
		}
		sectionBudget := len(sections[i]) - (len(joined) - maxBytes)
		truncated := truncateTextAtLineBoundary(sections[i], sectionBudget, marker)
		if len(truncated) >= len(sections[i]) {
			return truncateTextAtLineBoundary(joined, maxBytes, marker)
		}
		sections[i] = truncated
	}
}

func largestPRBodySectionIndex(sections []string) int {
	index := -1
	length := 0
	for i, section := range sections {
		if len(section) <= length {
			continue
		}
		index = i
		length = len(section)
	}
	return index
}

func splitPRBodySections(body string) []string {
	if body == "" {
		return nil
	}

	var starts []int
	for start := 0; start < len(body); {
		end := strings.IndexByte(body[start:], '\n')
		lineEnd := len(body)
		next := len(body)
		if end >= 0 {
			lineEnd = start + end
			next = lineEnd + 1
		}
		if isPRBodySectionHeading(body[start:lineEnd]) {
			starts = append(starts, start)
		}
		start = next
	}
	if len(starts) == 0 || starts[0] != 0 {
		starts = append([]int{0}, starts...)
	}

	sections := make([]string, 0, len(starts))
	for i, start := range starts {
		end := len(body)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		sections = append(sections, body[start:end])
	}
	return sections
}

func isPRBodySectionHeading(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "## ") && !strings.HasPrefix(line, "### ")
}

func joinPRBodySections(sections []string) string {
	var b strings.Builder
	for _, section := range sections {
		if section == "" {
			continue
		}
		if b.Len() > 0 {
			current := b.String()
			if !strings.HasSuffix(current, "\n") {
				b.WriteString("\n")
			}
			current = b.String()
			if !strings.HasSuffix(current, "\n\n") {
				b.WriteString("\n")
			}
			section = strings.TrimLeft(section, "\n")
		}
		b.WriteString(section)
	}
	return b.String()
}

func truncateTextAtLineBoundary(text string, maxBytes int, marker string) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	if marker != "" {
		marker = "\n\n" + marker
	}
	available := maxBytes - len(marker)
	if available <= 0 {
		if len(marker) <= maxBytes {
			return strings.TrimLeft(marker, "\n")
		}
		return ""
	}

	available = utf8BoundaryBefore(text, available)
	cut := strings.LastIndex(text[:available], "\n")
	if cut <= 0 {
		cut = available
	}
	return strings.TrimRight(text[:cut], "\n") + marker
}

func truncatePipelineUpdateAtLineBoundary(text string, maxBytes int, marker string) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	if marker != "" {
		marker = "\n\n" + marker
	}
	available := maxBytes - len(marker)
	if available <= 0 {
		if len(marker) <= maxBytes {
			return strings.TrimLeft(marker, "\n")
		}
		return ""
	}

	available = utf8BoundaryBefore(text, available)
	searchEnd := available
	if searchEnd < len(text) && text[searchEnd] == '\n' {
		searchEnd++
	}
	cut := strings.LastIndex(text[:searchEnd], "\n")
	if cut <= 0 {
		return strings.TrimRight(text[:available], "\n") + marker
	}
	return strings.TrimRight(text[:cut], "\n") + marker
}

func utf8BoundaryBefore(text string, n int) int {
	if n >= len(text) {
		return len(text)
	}
	if n <= 0 {
		return 0
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return n
}

func stripGeneratedSections(body string) string {
	if body == "" {
		return ""
	}

	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	skipping := false

	for _, raw := range lines {
		line := strings.TrimSpace(raw)

		if skipping {
			if strings.HasPrefix(line, "## ") {
				if isGeneratedSectionHeading(line) {
					continue
				}
				skipping = false
			} else {
				continue
			}
		}

		if isGeneratedSectionHeading(line) {
			skipping = true
			continue
		}

		out = append(out, raw)
	}

	return strings.TrimSpace(strings.Join(out, "\n"))
}

func isGeneratedSectionHeading(line string) bool {
	if !strings.HasPrefix(strings.TrimSpace(line), "##") {
		return false
	}

	heading := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "##"))
	heading = strings.TrimRight(heading, ":.!? ")
	heading = strings.ToLower(heading)

	switch heading {
	case "intent", "risk assessment", "testing", "tests", "pipeline":
		return true
	default:
		return false
	}
}

func prependNotesSection(body string, sctx *pipeline.StepContext) string {
	section := prNoteSectionText(sctx)
	if section == "" {
		return body
	}
	if strings.TrimSpace(body) == "" {
		return section
	}
	return section + "\n\n" + body
}

func cleanedPRNote(sctx *pipeline.StepContext) string {
	if sctx == nil {
		return ""
	}
	return strings.TrimSpace(sctx.PRNote)
}

func prNoteSectionText(sctx *pipeline.StepContext) string {
	note := cleanedPRNote(sctx)
	if note == "" {
		return ""
	}
	if noteHasOwnNotesHeading(note) {
		return note
	}
	return "## Notes\n\n" + note
}

func noteHasOwnNotesHeading(note string) bool {
	line := note
	if i := strings.IndexAny(note, "\r\n"); i >= 0 {
		line = note[:i]
	}
	line = strings.TrimSpace(line)
	hashes := 0
	for hashes < len(line) && line[hashes] == '#' {
		hashes++
	}
	if hashes != 2 {
		return false
	}
	rest := line[hashes:]
	if rest != "" && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
		return false
	}
	content := strings.TrimSpace(rest)
	if trimmed := strings.TrimRight(content, "#"); trimmed != content {
		if trimmed == "" || strings.HasSuffix(trimmed, " ") || strings.HasSuffix(trimmed, "\t") {
			content = strings.TrimSpace(trimmed)
		}
	}
	return strings.EqualFold(content, "notes")
}

func prNotePromptSection(sctx *pipeline.StepContext) string {
	note := cleanedPRNote(sctx)
	if note == "" {
		return ""
	}
	return "\n\nAuthor-provided PR notes (trusted guidance; keep the summary consistent with them and do not repeat them in What Changed):\n" +
		"-----BEGIN AUTHOR NOTES-----\n" + note + "\n-----END AUTHOR NOTES-----\n"
}

// fallbackWhatChanged is the deterministic heading-free What Changed content used when the
// drafting agent produced nothing usable.
func fallbackWhatChanged(finalDiff string) string {
	diffSummary := strings.TrimSpace(finalDiff)
	if diffSummary == "" {
		return "Final diff unavailable; no complete scope summary was generated."
	}
	return "Final changed paths and statuses:\n\n```text\n" + escapeMarkdownFence(diffSummary) + "\n```"
}

func fallbackPRContent(sctx *pipeline.StepContext, finalDiff, riskLine, testingMD, pipelineMD string, bodyLimit int) prContent {
	title := "chore: update pull request"
	summary := "Updates the branch with the final recorded changes."
	whatChanged := fallbackWhatChanged(finalDiff)
	body := buildPRNarrative(summary, whatChanged)
	if bodyLimit > 0 {
		body = assemblePRBody(sctx, body, riskLine, testingMD, pipelineMD, bodyLimit)
	} else {
		body = buildPRBody(body, riskLine, testingMD, pipelineMD, sctx)
	}
	return prContent{
		Title:       title,
		Summary:     summary,
		WhatChanged: whatChanged,
		Body:        body,
	}
}
