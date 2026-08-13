package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	gitutil "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutorReusesPreparedCommandPlanningWorkspace(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, nil, dir, baseSHA, headSHA, config.Commands{})
	counterPath := filepath.Join(t.TempDir(), "post-worktree-count.txt")
	hook := fmt.Sprintf("echo prepared >> %q; echo prepared > planner-prepared.txt", counterPath)
	if runtime.GOOS == "windows" {
		hook = fmt.Sprintf("echo prepared>>%q & echo prepared>planner-prepared.txt", counterPath)
	}

	plannerDirs := make([]string, 0, 2)
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerDirs = append(plannerDirs, opts.CWD)
			if _, err := os.Stat(filepath.Join(opts.CWD, "planner-prepared.txt")); err != nil {
				return nil, fmt.Errorf("planner workspace was not prepared: %w", err)
			}
			plannerHead := gitCmd(t, opts.CWD, "rev-parse", "HEAD")
			pipelineHead := gitCmd(t, dir, "rev-parse", "HEAD")
			if plannerHead != pipelineHead {
				return nil, fmt.Errorf("planner HEAD = %s, want pipeline HEAD %s", plannerHead, pipelineHead)
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	root := t.TempDir()
	plannerPath := paths.WithRoot(root).WorktreeDir(sctx.Repo.ID, sctx.Run.ID+"-command-plan")
	cfg := &config.Config{Agent: types.AgentClaude, Hooks: config.Hooks{PostWorktree: hook}}
	executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(root), cfg, ag, []pipeline.Step{
		&commandPlanningProbeStep{name: types.StepBuild, after: func(context *pipeline.StepContext) error {
			if err := os.WriteFile(filepath.Join(context.WorkDir, "after-build.txt"), []byte("new head\n"), 0o644); err != nil {
				return err
			}
			if _, err := gitutil.Run(context.Ctx, context.WorkDir, "add", "after-build.txt"); err != nil {
				return err
			}
			_, err := gitutil.Run(context.Ctx, context.WorkDir, "commit", "-m", "advance pipeline head")
			return err
		}},
		&commandPlanningProbeStep{name: types.StepLint},
	}, nil)

	if err := executor.Execute(sctx.Ctx, sctx.Run, sctx.Repo, dir); err != nil {
		t.Fatal(err)
	}
	if len(plannerDirs) != 2 || plannerDirs[0] != plannerPath || plannerDirs[1] != plannerPath {
		t.Fatalf("planner workspaces = %v, want shared managed path %q", plannerDirs, plannerPath)
	}
	counter, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(counter), "prepared"); got != 1 {
		t.Fatalf("post-worktree hook executions = %d, want 1", got)
	}
	if _, err := os.Stat(plannerPath); !os.IsNotExist(err) {
		t.Fatalf("planner workspace survived executor cleanup: %v", err)
	}
}

type commandPlanningProbeStep struct {
	name  types.StepName
	after func(*pipeline.StepContext) error
}

func (s *commandPlanningProbeStep) Name() types.StepName { return s.name }

func (s *commandPlanningProbeStep) Execute(context *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if _, err := planPipelineCommand(context, s.name, "Select command."); err != nil {
		return nil, err
	}
	if s.after != nil {
		if err := s.after(context); err != nil {
			return nil, err
		}
	}
	return &pipeline.StepOutcome{}, nil
}

func TestPlanPipelineCommandRejectsCommittedMutation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	plannerDir := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerDir = opts.CWD
			if err := os.WriteFile(filepath.Join(opts.CWD, "planner-mutation.txt"), []byte("changed\n"), 0o644); err != nil {
				return nil, err
			}
			if _, err := gitutil.Run(ctx, opts.CWD, "add", "planner-mutation.txt"); err != nil {
				return nil, err
			}
			if _, err := gitutil.Run(ctx, opts.CWD, "commit", "-m", "planner mutation"); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	_, err := planPipelineCommand(sctx, types.StepLint, "Select lint.")
	if err == nil || !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("planPipelineCommand() error = %v, want committed-mutation refusal", err)
	}
	if plannerDir == dir {
		t.Fatal("planner ran in the pipeline worktree")
	}
	if _, err := os.Stat(plannerDir); err != nil {
		t.Fatalf("shared planner worktree missing after restore: %v", err)
	}
	if got := gitCmd(t, plannerDir, "status", "--porcelain"); got != "" {
		t.Fatalf("shared planner worktree was not restored: %q", got)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("pipeline HEAD = %s, want %s", got, headSHA)
	}
	if _, err := os.Stat(filepath.Join(dir, "planner-mutation.txt")); !os.IsNotExist(err) {
		t.Fatalf("planner mutation reached pipeline worktree: %v", err)
	}
}

func TestPlanPipelineCommandContainsUncommittedMutations(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	plannerDir := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerDir = opts.CWD
			if err := os.WriteFile(filepath.Join(opts.CWD, "feature.txt"), []byte("rewritten\n"), 0o644); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(opts.CWD, "planner-untracked.txt"), []byte("untracked\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	_, err := planPipelineCommand(sctx, types.StepBuild, "Select build.")
	if err == nil || !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("planPipelineCommand() error = %v, want uncommitted-mutation refusal", err)
	}
	if _, err := os.Stat(plannerDir); err != nil {
		t.Fatalf("shared planner worktree missing after restore: %v", err)
	}
	if got := gitCmd(t, plannerDir, "status", "--porcelain"); got != "" {
		t.Fatalf("shared planner worktree was not restored: %q", got)
	}
	content, err := os.ReadFile(filepath.Join(dir, "feature.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "feature code\n" {
		t.Fatalf("pipeline feature.txt = %q, want original content", content)
	}
	if _, err := os.Stat(filepath.Join(dir, "planner-untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("planner untracked file reached pipeline worktree: %v", err)
	}
}

func TestPlanPipelineCommandContainsMutationWhenAgentFails(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	plannerDir := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerDir = opts.CWD
			if err := os.WriteFile(filepath.Join(opts.CWD, "planner-error.txt"), []byte("changed\n"), 0o644); err != nil {
				return nil, err
			}
			return nil, errors.New("planner failed")
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	_, err := planPipelineCommand(sctx, types.StepTest, "Select test.")
	if err == nil || !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("planPipelineCommand() error = %v, want mutation refusal before agent error", err)
	}
	if _, err := os.Stat(plannerDir); err != nil {
		t.Fatalf("shared planner worktree missing after restore: %v", err)
	}
	if got := gitCmd(t, plannerDir, "status", "--porcelain"); got != "" {
		t.Fatalf("shared planner worktree was not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "planner-error.txt")); !os.IsNotExist(err) {
		t.Fatalf("failed planner mutation reached pipeline worktree: %v", err)
	}
}

func TestPlanPipelineCommandPersistsPrivateCommandSeparatelyFromEvidence(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	command := "TOKEN='$UNEXPANDED' go test ./internal/pipeline/..."
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			output, err := json.Marshal(commandPlan{Command: &command})
			return &agent.Result{Output: output}, err
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepLint)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID

	gotCommand, err := planPipelineCommand(sctx, types.StepLint, "Select lint.")
	if err != nil {
		t.Fatal(err)
	}
	if gotCommand != command {
		t.Fatalf("planned command = %q, want %q", gotCommand, command)
	}
	step, err := sctx.DB.GetStepResult(sctx.StepResultID)
	if err != nil {
		t.Fatal(err)
	}
	if step.PlannedCommand == nil || *step.PlannedCommand != command {
		t.Fatalf("stored planned command = %v, want exact value %q", step.PlannedCommand, command)
	}
	if step.EvidenceJSON != nil && strings.Contains(*step.EvidenceJSON, command) {
		t.Fatal("private planned command leaked into public evidence")
	}
}
