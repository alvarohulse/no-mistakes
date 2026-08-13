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
	plannerDir, err := sctx.CommandPlanning.Prepare(sctx.Ctx)
	if err != nil {
		return "", fmt.Errorf("prepare %s command planning workspace: %w", step, err)
	}
	plannerBefore, err := inspectCommandPlanningFingerprint(sctx.Ctx, plannerDir)
	if err != nil {
		return "", errors.Join(
			fmt.Errorf("inspect command planning workspace before %s planning: %w", step, err),
			sctx.CommandPlanning.Discard(),
		)
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
	// The integrity check runs before the agent's own error is reported: a
	// planner that both wrote to the worktree and failed must surface as a
	// violated read-only pass, not as a plain agent failure that hides it.
	integrityCtx, cancelIntegrity := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelIntegrity()
	plannerAfter, inspectErr := inspectCommandPlanningFingerprint(integrityCtx, plannerDir)
	if inspectErr != nil {
		integrityErr := errors.Join(
			fmt.Errorf("%s command planner violated the read-only pass; integrity inspection failed: %w", step, inspectErr),
			sctx.CommandPlanning.Discard(),
		)
		if err != nil {
			integrityErr = errors.Join(integrityErr, fmt.Errorf("agent plan %s command: %w", step, err))
		}
		return "", integrityErr
	}
	if !plannerBefore.equal(plannerAfter) {
		integrityErr := errors.Join(
			fmt.Errorf("%s command planner modified its workspace during a read-only pass", step),
			sctx.CommandPlanning.Discard(),
		)
		if err != nil {
			integrityErr = errors.Join(integrityErr, fmt.Errorf("agent plan %s command: %w", step, err))
		}
		return "", integrityErr
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
