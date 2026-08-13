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

func planPipelineCommand(sctx *pipeline.StepContext, step types.StepName, task string) (string, error) {
	headSHA, err := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("inspect HEAD before %s planning: %w", step, err)
	}
	sourceStatus, err := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("inspect pipeline worktree before %s planning: %w", step, err)
	}
	sourceUntracked, err := untrackedPaths(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return "", fmt.Errorf("inspect untracked pipeline files before %s planning: %w", step, err)
	}
	plannerDir, err := sctx.CommandPlanning.Prepare(sctx.Ctx)
	if err != nil {
		return "", fmt.Errorf("prepare %s command planning workspace: %w", step, err)
	}

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
	afterSourceHead, sourceHeadErr := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", "HEAD")
	if sourceHeadErr != nil {
		return "", fmt.Errorf("inspect pipeline HEAD after %s planning: %w", step, sourceHeadErr)
	}
	afterSourceStatus, sourceStatusErr := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain")
	if sourceStatusErr != nil {
		return "", fmt.Errorf("inspect pipeline worktree after %s planning: %w", step, sourceStatusErr)
	}
	sourceMutated := headSHA != afterSourceHead || sourceStatus != afterSourceStatus
	if sourceMutated {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if restoreErr := restorePlanningWorktree(cleanupCtx, sctx.WorkDir, headSHA, sourceUntracked); restoreErr != nil {
			return "", errors.Join(fmt.Errorf("%s command planner modified the pipeline worktree during a read-only pass", step), restoreErr)
		}
	}
	plannerMutated := beforeHead != afterHead || beforeStatus != afterStatus
	if plannerMutated {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if restoreErr := sctx.CommandPlanning.Restore(cleanupCtx); restoreErr != nil {
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

func untrackedPaths(ctx context.Context, workDir string) (map[string]struct{}, error) {
	output, err := git.Run(ctx, workDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	paths := make(map[string]struct{})
	for _, path := range strings.Split(output, "\x00") {
		if path != "" {
			paths[path] = struct{}{}
		}
	}
	return paths, nil
}

func restorePlanningWorktree(ctx context.Context, workDir, headSHA string, preservedUntracked map[string]struct{}) error {
	if _, err := git.Run(ctx, workDir, "reset", "--hard", headSHA); err != nil {
		return fmt.Errorf("restore planning worktree HEAD: %w", err)
	}
	currentUntracked, err := untrackedPaths(ctx, workDir)
	if err != nil {
		return fmt.Errorf("inspect planning worktree during restore: %w", err)
	}
	for path := range currentUntracked {
		if _, preserve := preservedUntracked[path]; preserve {
			continue
		}
		if err := os.RemoveAll(filepath.Join(workDir, filepath.FromSlash(path))); err != nil {
			return fmt.Errorf("remove planner-created file %s: %w", path, err)
		}
	}
	return nil
}
