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
	sourceBefore, err := captureCommandPlanningSource(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return "", errors.Join(
			fmt.Errorf("snapshot pipeline worktree before %s command planning: %w", step, err),
			sctx.CommandPlanning.Discard(),
		)
	}
	defer sourceBefore.Close()
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, effectiveBaseBranch(sctx))
	if err := sctx.Agent.Close(); err != nil {
		return "", fmt.Errorf("reset agent before planning %s command: %w", step, err)
	}
	var result *agent.Result
	var agentRunErr error
	var agentCloseErr error
	func() {
		defer func() {
			agentCloseErr = sctx.Agent.Close()
		}()
		result, agentRunErr = sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
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
			Env:        append(append([]string(nil), sctx.Env...), pipeline.CommandPlanningGitEnv()...),
			JSONSchema: commandPlanSchema,
			OnChunk:    sctx.LogChunk,
			Purpose:    string(step) + "-plan",
		})
	}()
	var agentErr error
	if agentRunErr != nil {
		agentErr = errors.Join(agentErr, agentRunErr)
	}
	if agentCloseErr != nil {
		agentErr = errors.Join(agentErr, fmt.Errorf("close agent after %s command planning: %w", step, agentCloseErr))
	}
	// The integrity check runs before the agent's own error is reported: a
	// planner that both wrote to the worktree and failed must surface as a
	// violated read-only pass, not as a plain agent failure that hides it.
	integrityCtx, cancelIntegrity := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelIntegrity()
	plannerAfter, inspectErr := inspectCommandPlanningFingerprint(integrityCtx, plannerDir)
	sourceChanged, sourceInspectErr := sourceBefore.Changed(integrityCtx)
	sourceViolation := sourceInspectErr != nil || sourceChanged
	var sourceRestoreErr error
	if sourceViolation {
		sourceRestoreErr = sourceBefore.Restore()
	}
	if inspectErr != nil || sourceViolation {
		var integrityErr error
		if inspectErr != nil {
			integrityErr = errors.Join(integrityErr, fmt.Errorf("%s command planner violated the read-only pass; planner integrity inspection failed: %w", step, inspectErr))
		}
		if sourceInspectErr != nil {
			integrityErr = errors.Join(integrityErr, fmt.Errorf("%s command planner violated the read-only pass; pipeline worktree integrity inspection failed: %w", step, sourceInspectErr))
		} else if sourceChanged {
			integrityErr = errors.Join(integrityErr, fmt.Errorf("%s command planner modified the pipeline worktree during a read-only pass", step))
		}
		if sourceRestoreErr != nil {
			integrityErr = errors.Join(integrityErr, fmt.Errorf("restore pipeline worktree after %s command planning: %w", step, sourceRestoreErr))
		}
		integrityErr = errors.Join(integrityErr, sctx.CommandPlanning.Discard())
		if agentErr != nil {
			integrityErr = errors.Join(integrityErr, fmt.Errorf("agent plan %s command: %w", step, agentErr))
		}
		return "", integrityErr
	}
	if !plannerBefore.equal(plannerAfter) {
		integrityErr := errors.Join(
			fmt.Errorf("%s command planner modified its workspace during a read-only pass", step),
			sctx.CommandPlanning.Discard(),
		)
		if agentErr != nil {
			integrityErr = errors.Join(integrityErr, fmt.Errorf("agent plan %s command: %w", step, agentErr))
		}
		return "", integrityErr
	}
	if agentErr != nil {
		return "", fmt.Errorf("agent plan %s command: %w", step, agentErr)
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
