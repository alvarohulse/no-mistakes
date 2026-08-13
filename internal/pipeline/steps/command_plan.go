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

var commandPlanSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string"}
  },
  "required": ["command"]
}`)

type commandPlan struct {
	Command *string `json:"command"`
}

func planPipelineCommand(sctx *pipeline.StepContext, step types.StepName, task string) (string, error) {
	beforeHead, err := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("inspect HEAD before %s planning: %w", step, err)
	}
	beforeStatus, err := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("inspect worktree before %s planning: %w", step, err)
	}
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, effectiveBaseBranch(sctx))
	result, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
		Prompt: fmt.Sprintf(`Select the exact shell command the %s pipeline step should execute.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Task:
%s

Rules:
- This is a read-only command-planning pass.
- Inspect repository configuration and changed files only as needed.
- Do not run the selected command.
- Do not modify any file.
- Return JSON containing only a single "command" string.
- Return an empty command only when no meaningful %s command exists.%s%s`, step.DisplayName(sctx.Run.RefreshStrategy), sctx.Run.Branch, baseSHA, sctx.Run.HeadSHA, task, step, userIntentPromptSection(sctx), configuredPromptSection(sctx, step)),
		CWD:        sctx.WorkDir,
		JSONSchema: commandPlanSchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    string(step) + "-plan",
	})
	if err != nil {
		return "", fmt.Errorf("agent plan %s command: %w", step, err)
	}
	afterHead, headErr := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", "HEAD")
	if headErr != nil {
		return "", fmt.Errorf("inspect HEAD after %s planning: %w", step, headErr)
	}
	afterStatus, statusErr := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain")
	if statusErr != nil {
		return "", fmt.Errorf("inspect worktree after %s planning: %w", step, statusErr)
	}
	if beforeHead != afterHead || beforeStatus != afterStatus {
		return "", fmt.Errorf("%s command planner modified the worktree during a read-only pass", step)
	}
	var plan commandPlan
	if len(result.Output) == 0 || json.Unmarshal(result.Output, &plan) != nil || plan.Command == nil {
		return "", fmt.Errorf("%s command planner returned an invalid structured result", step)
	}
	return strings.TrimSpace(*plan.Command), nil
}
