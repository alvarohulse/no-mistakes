package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RetrospectStep records optional process notes without changing project docs.
type RetrospectStep struct{}

type retrospectiveOutput struct {
	Summary string   `json:"summary"`
	Notes   []string `json:"notes"`
}

var retrospectiveSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "description": "One concise sentence fragment summarizing the retrospective"},
		"notes": {
			"type": "array",
			"items": {"type": "string"},
			"description": "Optional concrete process notes or follow-up learnings from this run"
		}
	},
	"required": ["summary"]
}`)

func (s *RetrospectStep) Name() types.StepName { return types.StepRetrospect }

func (s *RetrospectStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if sctx == nil || sctx.Config == nil || !sctx.Config.Retrospect.Enabled {
		if sctx != nil && sctx.Log != nil {
			sctx.Log("retrospective step disabled")
		}
		return &pipeline.StepOutcome{Skipped: true}, nil
	}

	before, err := snapshotRetrospectiveWorktree(sctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot worktree before retrospective: %w", err)
	}

	prompt := fmt.Sprintf(`Write a short retrospective for this no-mistakes run.

Context:
- branch: %s
- target commit: %s
- default branch: %s

Task:
- Capture only process learnings, surprising friction, or follow-up notes useful to a future maintainer or agent.
- Keep it concise and concrete.
- Do not update documentation, source files, tests, config, git state, or any filesystem content.
- If there is nothing useful to record, return summary "no retrospective notes" and an empty notes array.

Rules:
- Return JSON with "summary" and optional "notes".
- The summary must be one concise sentence fragment.
- Notes must be actionable or evidence-backed; do not include generic praise.%s%s%s`,
		sctx.Run.Branch,
		sctx.Run.HeadSHA,
		sctx.Repo.DefaultBranch,
		executionContextPromptSection(),
		roundHistoryPromptSection(sctx),
		userIntentPromptSection(sctx),
	)

	result, runErr := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: retrospectiveSchema,
		OnChunk:    sctx.LogChunk,
	})

	after, snapErr := snapshotRetrospectiveWorktree(sctx)
	if snapErr != nil {
		return nil, fmt.Errorf("snapshot worktree after retrospective: %w", snapErr)
	}
	if after.head != before.head {
		return nil, fmt.Errorf("retrospective step changed HEAD")
	}
	if after != before {
		return nil, fmt.Errorf("retrospective step left worktree changes")
	}

	if runErr != nil {
		sctx.Log(fmt.Sprintf("retrospective skipped: %v", runErr))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}

	output := parseRetrospectiveOutput(result)
	summary := strings.TrimSpace(output.Summary)
	if summary == "" {
		summary = "no retrospective notes"
	}
	sctx.Log("retrospective: " + summary)
	for _, note := range output.Notes {
		note = strings.TrimSpace(note)
		if note != "" {
			sctx.Log("- " + note)
		}
	}

	return &pipeline.StepOutcome{FixSummary: summary}, nil
}

// retrospectiveWorktreeSnapshot fingerprints the run worktree so the step can
// verify the agent stayed read-only: head catches created or amended commits
// (which the push step would otherwise push), content catches every change
// the push step could stage - the `git add -A` surface plus the force-added
// in-repo evidence directory, so edits to already-dirty or untracked files,
// new files inside untracked directories (which keep an identical porcelain
// line), and gitignored writes under the evidence directory all count - and
// status catches index-state changes (staging) that leave content untouched.
type retrospectiveWorktreeSnapshot struct {
	head    string
	status  string
	content string
}

func snapshotRetrospectiveWorktree(sctx *pipeline.StepContext) (retrospectiveWorktreeSnapshot, error) {
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return retrospectiveWorktreeSnapshot{}, err
	}
	status, err := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil {
		return retrospectiveWorktreeSnapshot{}, err
	}
	var forceInclude []string
	if pathspec := inRepoEvidencePathspec(sctx.Ctx, sctx.WorkDir, sctx.Run.Branch, sctx.Run.ID, sctx.Config.Test.Evidence); pathspec != "" {
		forceInclude = append(forceInclude, pathspec)
	}
	content, err := git.WorktreeContentHash(sctx.Ctx, sctx.WorkDir, forceInclude...)
	if err != nil {
		return retrospectiveWorktreeSnapshot{}, err
	}
	return retrospectiveWorktreeSnapshot{head: head, status: status, content: content}, nil
}

func parseRetrospectiveOutput(result *agent.Result) retrospectiveOutput {
	if result == nil {
		return retrospectiveOutput{}
	}
	if len(result.Output) > 0 {
		var output retrospectiveOutput
		if err := json.Unmarshal(result.Output, &output); err == nil {
			return output
		}
	}
	return retrospectiveOutput{Summary: strings.TrimSpace(result.Text)}
}
