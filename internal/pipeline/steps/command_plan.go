package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
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
	sourceSnapshot, err := captureCommandPlanningSnapshot(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return "", fmt.Errorf("inspect pipeline worktree before %s planning: %w", step, err)
	}
	plannerDir, err := sctx.CommandPlanning.Prepare(sctx.Ctx)
	if err != nil {
		return "", fmt.Errorf("prepare %s command planning workspace: %w", step, err)
	}

	plannerSnapshot, err := captureCommandPlanningSnapshot(sctx.Ctx, plannerDir)
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
	afterPlanner, plannerSnapshotErr := captureCommandPlanningSnapshot(sctx.Ctx, plannerDir)
	if plannerSnapshotErr != nil {
		return "", fmt.Errorf("inspect detached worktree after %s planning: %w", step, plannerSnapshotErr)
	}
	afterSource, sourceSnapshotErr := captureCommandPlanningSnapshot(sctx.Ctx, sctx.WorkDir)
	if sourceSnapshotErr != nil {
		return "", fmt.Errorf("inspect pipeline worktree after %s planning: %w", step, sourceSnapshotErr)
	}
	sourceMutated := !sourceSnapshot.equal(afterSource)
	if sourceMutated {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if restoreErr := sourceSnapshot.restore(cleanupCtx, sctx.WorkDir); restoreErr != nil {
			return "", errors.Join(fmt.Errorf("%s command planner modified the pipeline worktree during a read-only pass", step), restoreErr)
		}
	}
	plannerMutated := !plannerSnapshot.equal(afterPlanner)
	if plannerMutated {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if restoreErr := plannerSnapshot.restore(cleanupCtx, plannerDir); restoreErr != nil {
			return "", errors.Join(fmt.Errorf("%s command planner modified the planning worktree during a read-only pass", step), restoreErr)
		}
	}
	if sourceMutated || plannerMutated {
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
