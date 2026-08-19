package steps

import (
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/prbody"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// maxContractCommits bounds the commit list handed to a formatter. A branch
// with more commits than this has a delta no PR body summarizes commit by
// commit, and the truncation is logged rather than silent.
const maxContractCommits = 200

// ContractInput is everything BuildContract needs, sourced either from a live
// step context or from stored run records.
type ContractInput struct {
	Run         *db.Run
	Repo        *db.Repo
	Steps       []*db.StepResult
	Rounds      map[string][]*db.StepRound
	Invocations []db.AgentInvocation
	Commits     []prbody.Commit

	// Intent is already sanitized. Callers must pass cleanedUserIntent's
	// output, never raw run intent: an inferred intent carries whatever the
	// transcript carried.
	Intent              string
	IntentSource        string
	IntentAuthoritative bool
	// Note is the author's --pr-note, trimmed and otherwise verbatim.
	Note string

	Title       string
	Summary     string
	WhatChanged string
	Branch      string
	BaseBranch  string
	BaseSHA     string
	Provider    string
	BodyLimit   int

	UserTestingInstructions []string
	UserTestingAttested     bool
}

// BuildContract assembles the hooks.pr_body payload.
//
// Everything here is raw material: per-step records rather than a rendered
// pipeline table, the three stored risk fields rather than a risk line, the
// test step's own evidence rather than a Testing section. The formatter owns
// every layout decision, which is the entire point of the hook.
func BuildContract(in ContractInput) *prbody.Contract {
	contract := &prbody.Contract{
		Version:         prbody.Version,
		Branch:          in.Branch,
		BaseBranch:      in.BaseBranch,
		BaseSHA:         in.BaseSHA,
		Provider:        in.Provider,
		BodyLimit:       in.BodyLimit,
		Title:           in.Title,
		Commits:         in.Commits,
		RefreshStrategy: string(types.RefreshStrategy("").OrDefault()),
	}
	if in.Run != nil {
		contract.RunID = in.Run.ID
		contract.HeadSHA = in.Run.HeadSHA
		contract.RefreshStrategy = string(in.Run.RefreshStrategy.OrDefault())
		if in.Run.Metadata != nil {
			contract.Metadata = *in.Run.Metadata
		}
	}
	if in.Repo != nil {
		contract.Repo = prbody.Repo{
			Root:          in.Repo.WorkingPath,
			UpstreamURL:   in.Repo.UpstreamURL,
			DefaultBranch: in.Repo.DefaultBranch,
		}
	}

	// Notes and Risk are always present; see the Sections doc comment for why
	// their absence has to be stated rather than implied.
	contract.Sections.Notes = prbody.NotesSection{Trusted: true}
	if note := strings.TrimSpace(in.Note); note != "" {
		contract.Sections.Notes.Text = note
		contract.Sections.Notes.Supplied = true
	}

	if summary := strings.TrimSpace(in.Summary); summary != "" {
		contract.Sections.Summary = &prbody.TextSection{Text: summary}
	}

	if trimmed := strings.TrimSpace(in.WhatChanged); trimmed != "" {
		contract.Sections.WhatChanged = &prbody.TextSection{Text: trimmed}
	}

	contract.Sections.Risk = contractRisk(in.Steps, in.Rounds)
	contract.Sections.StaticTests = contractStaticTests(in.Steps, in.Rounds)
	contract.Sections.ReviewEvidence = contractReviewEvidence(in.Steps, in.Rounds)
	contract.Sections.UserTesting = &prbody.UserTestingSection{
		Instructions: append([]string(nil), in.UserTestingInstructions...),
		Attested:     in.UserTestingAttested,
	}
	contract.Sections.Pipeline = contractPipeline(in.Steps, in.Rounds, in.Invocations, in.Run, strings.TrimSpace(in.Intent), in.IntentSource, in.IntentAuthoritative)
	return contract
}

// RunRecords is the stored per-step state a PR body reads. The built-in
// Pipeline section and the formatter contract are two renderings of the same
// records, so a body loads them once and hands them to both.
type RunRecords struct {
	Steps       []*db.StepResult
	Rounds      map[string][]*db.StepRound
	Invocations []db.AgentInvocation
}

// LoadRunRecords loads the stored per-step state a contract needs. A query
// failure degrades that part of the contract rather than the whole body.
func LoadRunRecords(d *db.DB, runID string) RunRecords {
	steps, err := d.GetStepsByRun(runID)
	if err != nil {
		slog.Warn("failed to query step results for pr body contract", "error", err)
		return RunRecords{}
	}
	rounds := make(map[string][]*db.StepRound, len(steps))
	for _, sr := range steps {
		r, err := d.GetRoundsByStep(sr.ID)
		if err != nil {
			slog.Warn("failed to query rounds for pr body contract", "step", sr.StepName, "error", err)
			continue
		}
		rounds[sr.ID] = r
	}
	invocations, err := d.GetAgentInvocationsByRun(runID)
	if err != nil {
		slog.Warn("failed to query agent invocations for pr body contract", "error", err)
	}
	return RunRecords{Steps: steps, Rounds: rounds, Invocations: invocations}
}

// buildPRBodyContract assembles the contract for a live run.
func buildPRBodyContract(sctx *pipeline.StepContext, records RunRecords, summary, whatChanged, title string, scope prBodyScope) *prbody.Contract {
	return BuildContract(ContractInput{
		Run:                 sctx.Run,
		Repo:                sctx.Repo,
		Steps:               records.Steps,
		Rounds:              records.Rounds,
		Invocations:         records.Invocations,
		Commits:             contractCommits(sctx, scope.baseSHA),
		Intent:              cleanedUserIntent(sctx),
		IntentSource:        sctx.IntentSource,
		IntentAuthoritative: intentSourceIsAuthoritative(sctx),
		Note:                cleanedPRNote(sctx),
		Title:               title,
		Summary:             summary,
		WhatChanged:         whatChanged,
		Branch:              scope.branch,
		BaseBranch:          scope.baseBranch,
		BaseSHA:             scope.baseSHA,
		Provider:            scope.provider,
		BodyLimit:           scope.bodyLimit,
	})
}

func contractCommits(sctx *pipeline.StepContext, baseSHA string) []prbody.Commit {
	if strings.TrimSpace(baseSHA) == "" || strings.TrimSpace(sctx.Run.HeadSHA) == "" {
		return nil
	}
	out, err := git.Run(sctx.Ctx, sctx.WorkDir, "log", "--format=%H%x00%s", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		slog.Warn("failed to read branch commits for pr body contract", "error", err)
		return nil
	}
	var commits []prbody.Commit
	for _, line := range strings.Split(out, "\n") {
		sha, subject, ok := strings.Cut(strings.TrimSpace(line), "\x00")
		if !ok || sha == "" {
			continue
		}
		commits = append(commits, prbody.Commit{SHA: sha, Subject: subject})
		if len(commits) == maxContractCommits {
			slog.Warn("pr body contract commit list truncated", "limit", maxContractCommits)
			break
		}
	}
	return commits
}

// contractRisk reads the review step's assessment. It mirrors extractRiskLine's
// sourcing - final step findings, falling back to the last round's - but keeps
// all three fields instead of collapsing them into one line.
func contractRisk(steps []*db.StepResult, rounds map[string][]*db.StepRound) prbody.RiskSection {
	for _, sr := range steps {
		if sr.StepName != types.StepReview {
			continue
		}
		findings := finalStepFindings(sr, rounds[sr.ID])
		if findings == nil || findings.RiskLevel == "" {
			return prbody.RiskSection{}
		}
		return prbody.RiskSection{
			Level:     findings.RiskLevel,
			Rationale: findings.RiskRationale,
			Scope:     findings.RiskScope,
			Reported:  true,
		}
	}
	return prbody.RiskSection{}
}

func contractTesting(steps []*db.StepResult, rounds map[string][]*db.StepRound) *prbody.TestingSection {
	for _, sr := range steps {
		if sr.StepName != types.StepTest {
			continue
		}
		findings := finalStepFindings(sr, rounds[sr.ID])
		if findings == nil {
			return nil
		}
		section := &prbody.TestingSection{
			Summary: strings.TrimSpace(findings.TestingSummary),
			Tested:  findings.Tested,
		}
		for _, artifact := range findings.Artifacts {
			section.Artifacts = append(section.Artifacts, prbody.Artifact{
				Kind:  artifact.Kind,
				Label: artifact.Label,
				Path:  artifact.Path,
				URL:   artifact.URL,
			})
		}
		if section.Summary == "" && len(section.Tested) == 0 && len(section.Artifacts) == 0 {
			return nil
		}
		return section
	}
	return nil
}

func contractStaticTests(steps []*db.StepResult, rounds map[string][]*db.StepRound) *prbody.StaticTestsSection {
	for _, sr := range steps {
		if sr.StepName != types.StepTest {
			continue
		}
		section := &prbody.StaticTestsSection{}
		if findings := finalStepFindings(sr, rounds[sr.ID]); findings != nil {
			section.Summary = strings.TrimSpace(findings.TestingSummary)
			section.Reported = append(section.Reported, findings.Tested...)
			for _, artifact := range findings.Artifacts {
				section.Artifacts = append(section.Artifacts, prbody.Artifact{
					Kind: artifact.Kind, Label: artifact.Label, Path: artifact.Path, URL: artifact.URL,
				})
			}
		}
		if evidence, err := sr.Evidence(); err == nil {
			for _, command := range evidence.Commands {
				section.Commands = append(section.Commands, prbody.PipelineCommand{
					Round: command.Round, Sequence: command.Sequence, Command: command.Command,
					Outcome: command.Outcome, ExitCode: command.ExitCode,
				})
			}
		}
		if section.Summary == "" && len(section.Commands) == 0 && len(section.Reported) == 0 && len(section.Artifacts) == 0 {
			return nil
		}
		return section
	}
	return nil
}

func contractReviewEvidence(steps []*db.StepResult, rounds map[string][]*db.StepRound) *prbody.ReviewEvidenceSection {
	for _, sr := range steps {
		if sr.StepName != types.StepReview {
			continue
		}
		section := &prbody.ReviewEvidenceSection{
			Status:   string(sr.Status),
			Rounds:   len(rounds[sr.ID]),
			Findings: contractStepFindings(sr, rounds[sr.ID]),
		}
		if section.Rounds == 0 {
			section.Rounds = 1
		}
		if evidence, err := sr.Evidence(); err == nil {
			section.Evidence = append(section.Evidence, evidence.Evidence...)
		}
		return section
	}
	return nil
}

// finalStepFindings returns a step's authoritative findings: the step row's
// own, or the last round's when the step row has none.
func finalStepFindings(sr *db.StepResult, stepRounds []*db.StepRound) *types.Findings {
	if sr.FindingsJSON != nil {
		f, err := types.ParseFindingsJSON(*sr.FindingsJSON)
		if err != nil {
			// An unreadable step row is not a reason to fall back to a
			// staler round; report nothing rather than something wrong.
			return nil
		}
		return &f
	}
	for i := len(stepRounds) - 1; i >= 0; i-- {
		if stepRounds[i].FindingsJSON == nil {
			continue
		}
		if f, err := types.ParseFindingsJSON(*stepRounds[i].FindingsJSON); err == nil {
			return &f
		}
	}
	return nil
}

func contractPipeline(steps []*db.StepResult, rounds map[string][]*db.StepRound, invocations []db.AgentInvocation, run *db.Run, intentText, intentSource string, intentProvided bool) *prbody.PipelineSection {
	if len(steps) == 0 {
		return nil
	}
	var strategy types.RefreshStrategy
	if run != nil {
		strategy = run.RefreshStrategy
	}

	byStep := make(map[string][]prbody.AgentRun, len(invocations))
	for _, invocation := range invocations {
		totalInput, uncachedInput := agent.CanonicalInputMeters(invocation.Agent, invocation.DeltaInputTokens, invocation.DeltaCacheReadTokens, invocation.DeltaCacheCreationTokens)
		agentRun := prbody.AgentRun{
			Round:               invocation.Round,
			Purpose:             invocation.Purpose,
			Agent:               invocation.Agent,
			Model:               invocation.Model,
			InvocationMode:      string(invocation.InvocationMode),
			NestedReported:      invocation.AgentObservationsReported,
			NestedCount:         invocation.NestedAgentCount,
			StartedAt:           invocation.StartedAt,
			DurationMS:          invocation.DurationMS,
			InputTokens:         totalInput,
			OutputTokens:        invocation.DeltaOutputTokens,
			UncachedInputTokens: uncachedInput,
			CacheReadTokens:     invocation.DeltaCacheReadTokens,
			CacheWriteTokens:    invocation.DeltaCacheCreationTokens,
			ReportedCostUSD:     invocation.ReportedCostUSD,
		}
		if invocation.ModelProvider != nil {
			agentRun.Provider = *invocation.ModelProvider
			agentRun.Vendor = *invocation.ModelProvider
		}
		for _, observation := range invocation.AgentObservations {
			agentRun.Nested = append(agentRun.Nested, prbody.NestedAgent{
				Identity:       observation.Identity,
				InvocationMode: string(observation.InvocationMode),
			})
		}
		byStep[invocation.StepName] = append(byStep[invocation.StepName], agentRun)
	}

	section := &prbody.PipelineSection{
		Attribution: prbody.Attribution{Name: prBodyAttributionName, URL: prBodyAttributionURL},
	}
	if run != nil {
		for _, source := range run.ConfigSources {
			section.ConfigSources = append(section.ConfigSources, prbody.ConfigSource{
				Kind:   source.Kind,
				Digest: source.Digest,
			})
		}
	}
	// Every recorded step is emitted, including pr and ci. The built-in body
	// hides those two because printing "PR: running" inside the PR body it is
	// currently writing reads badly - a layout judgement, and layout is exactly
	// what this contract delegates. A formatter that wants the same omission can
	// make it; one that reconstructs a run's history needs the rows.
	for _, sr := range steps {
		step := prbody.PipelineStep{
			Name:       string(sr.StepName),
			Label:      sr.StepName.DisplayName(strategy),
			Order:      sr.StepOrder,
			Status:     string(sr.Status),
			SkipSource: sr.SkipSource,
			ExitCode:   sr.ExitCode,
			DurationMS: sr.DurationMS,
			Rounds:     len(rounds[sr.ID]),
			Findings:   contractStepFindings(sr, rounds[sr.ID]),
			Agents:     byStep[string(sr.StepName)],
		}
		storedEvidence, err := sr.Evidence()
		if err == nil {
			for _, command := range storedEvidence.Commands {
				if strings.TrimSpace(command.Command) != "" {
					step.Commands = append(step.Commands, prbody.PipelineCommand{
						Round: command.Round, Sequence: command.Sequence, Command: command.Command,
						Outcome: command.Outcome, ExitCode: command.ExitCode,
					})
				}
			}
			step.Evidence = append(step.Evidence, storedEvidence.Evidence...)
			step.Explanation = storedEvidence.Explanation
		}
		if findings := finalStepFindings(sr, rounds[sr.ID]); findings != nil && sr.StepName == types.StepTest {
			step.Evidence = append(step.Evidence, findings.Tested...)
		}
		if sr.StepName == types.StepIntent {
			if storedEvidence.Intent != nil {
				step.Intent = contractStoredIntent(storedEvidence.Intent)
			} else {
				step.Intent = contractIntentResult(sr, intentText, intentSource, intentProvided)
			}
		}
		if step.Explanation == "" && step.Intent == nil && len(step.Commands) == 0 && len(step.Evidence) == 0 && (sr.Status == types.StepStatusCompleted || sr.Status == types.StepStatusSkipped) {
			step.Explanation = contractStepExplanation(sr, len(step.Agents) > 0)
		}
		if step.Rounds == 0 {
			// A step that ran without recording rounds still ran once.
			step.Rounds = 1
		}
		section.Steps = append(section.Steps, step)
	}
	if len(section.Steps) == 0 {
		return nil
	}
	return section
}

func contractStoredIntent(stored *db.IntentEvidence) *prbody.IntentResult {
	result := &prbody.IntentResult{
		Text:     cleanedUserIntent(&pipeline.StepContext{UserIntent: stored.Text}),
		Source:   stored.Source,
		Provided: stored.Provided,
	}
	if stored.Reason != nil {
		result.Reason = &prbody.IntentReason{Code: stored.Reason.Code, Message: stored.Reason.Message}
	}
	return result
}

// contractStepExplanation is the last resort for a step that recorded nothing.
// Skip provenance is persisted at each skip site, so this must not claim a
// cause it cannot know.
func contractStepExplanation(step *db.StepResult, agentRan bool) string {
	if step.Status == types.StepStatusSkipped {
		return "Step was skipped and recorded no further detail."
	}
	if agentRan {
		return "Step completed through the recorded agent invocation."
	}
	return "Step completed successfully; no shell command or additional evidence was recorded."
}

func contractIntentResult(step *db.StepResult, text, source string, provided bool) *prbody.IntentResult {
	if text != "" {
		return &prbody.IntentResult{Text: text, Source: source, Provided: provided}
	}
	code := "no_matching_transcript"
	message := "No matching agent transcript supplied an intent."
	if step != nil && step.Status == types.StepStatusSkipped {
		code = "step_skipped"
		message = "Intent extraction was skipped."
	}
	return &prbody.IntentResult{Reason: &prbody.IntentReason{Code: code, Message: message}}
}

func contractStepFindings(sr *db.StepResult, stepRounds []*db.StepRound) prbody.StepFindings {
	findings := finalStepFindings(sr, stepRounds)
	if findings == nil || len(findings.Items) == 0 {
		return prbody.StepFindings{}
	}
	out := prbody.StepFindings{Total: len(findings.Items), BySeverity: map[string]int{}}
	for _, item := range findings.Items {
		severity := strings.TrimSpace(item.Severity)
		if severity == "" {
			severity = "unspecified"
		}
		out.BySeverity[severity]++
	}
	return out
}
