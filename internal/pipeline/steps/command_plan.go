package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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

func planPipelineCommand(sctx *pipeline.StepContext, step types.StepName, task string) (_ string, retErr error) {
	headSHA, err := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("inspect HEAD before %s planning: %w", step, err)
	}
	plannerRoot, err := os.MkdirTemp("", "no-mistakes-command-plan-*")
	if err != nil {
		return "", fmt.Errorf("create %s command planning directory: %w", step, err)
	}
	plannerDir := filepath.Join(plannerRoot, "worktree")
	if err := git.WorktreeAdd(sctx.Ctx, sctx.WorkDir, plannerDir, headSHA); err != nil {
		_ = os.RemoveAll(plannerRoot)
		return "", fmt.Errorf("create detached %s command planning worktree: %w", step, err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanupErr := git.WorktreeRemove(cleanupCtx, sctx.WorkDir, plannerDir)
		removeErr := os.RemoveAll(plannerRoot)
		if cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove detached %s command planning worktree: %w", step, cleanupErr))
		}
		if removeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove %s command planning directory: %w", step, removeErr))
		}
	}()

	beforeHead, err := git.Run(sctx.Ctx, plannerDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("inspect detached HEAD before %s planning: %w", step, err)
	}
	beforeStatus, err := git.Run(sctx.Ctx, plannerDir, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("inspect detached worktree before %s planning: %w", step, err)
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
		CWD:        plannerDir,
		JSONSchema: commandPlanSchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    string(step) + "-plan",
	})
	if err != nil && sctx.Ctx.Err() != nil {
		return "", fmt.Errorf("agent plan %s command: %w", step, err)
	}
	// The integrity check runs before the agent's own error is reported: a
	// planner that both wrote to the worktree and failed must surface as a
	// violated read-only pass, not as a plain agent failure that hides it.
	afterHead, headErr := git.Run(sctx.Ctx, plannerDir, "rev-parse", "HEAD")
	if headErr != nil {
		return "", fmt.Errorf("inspect HEAD after %s planning: %w", step, headErr)
	}
	afterStatus, statusErr := git.Run(sctx.Ctx, plannerDir, "status", "--porcelain")
	if statusErr != nil {
		return "", fmt.Errorf("inspect worktree after %s planning: %w", step, statusErr)
	}
	if beforeHead != afterHead || beforeStatus != afterStatus {
		return "", fmt.Errorf("%s command planner modified the worktree during a read-only pass", step)
	}
	if err != nil {
		return "", fmt.Errorf("agent plan %s command: %w", step, err)
	}
	var plan commandPlan
	if len(result.Output) == 0 || json.Unmarshal(result.Output, &plan) != nil || plan.Command == nil {
		return "", fmt.Errorf("%s command planner returned an invalid structured result", step)
	}
	command := strings.TrimSpace(*plan.Command)
	if command != "" && sctx.DB != nil && sctx.StepResultID != "" {
		if err := sctx.DB.SetStepPlannedCommand(sctx.StepResultID, command); err != nil {
			return "", fmt.Errorf("persist private %s planned command: %w", step, err)
		}
	}
	return command, nil
}
