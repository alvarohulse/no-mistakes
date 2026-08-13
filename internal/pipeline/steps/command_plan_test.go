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
	"github.com/kunchenguid/no-mistakes/internal/worktreehook"
)

func TestExecutorReusesCleanCommandPlanningWorkspace(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, nil, dir, baseSHA, headSHA, config.Commands{})
	counterPath := filepath.Join(t.TempDir(), "post-worktree-count.txt")
	hook := fmt.Sprintf("echo prepared >> %q; echo prepared > planner-prepared.txt", counterPath)
	if runtime.GOOS == "windows" {
		hook = fmt.Sprintf("echo prepared>>%q & echo prepared>planner-prepared.txt", counterPath)
	}
	cfg := &config.Config{Agent: types.AgentClaude, Hooks: config.Hooks{PostWorktree: hook}}
	if err := worktreehook.Run(sctx.Ctx, dir, cfg); err != nil {
		t.Fatalf("prepare pipeline worktree: %v", err)
	}

	plannerDirs := make([]string, 0, 2)
	plannerHeads := make([]string, 0, 2)
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerDirs = append(plannerDirs, opts.CWD)
			plannerHeads = append(plannerHeads, gitCmd(t, opts.CWD, "rev-parse", "HEAD"))
			if _, err := os.Stat(filepath.Join(opts.CWD, "planner-prepared.txt")); !os.IsNotExist(err) {
				return nil, fmt.Errorf("planner copied hook-prepared state: %v", err)
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	root := t.TempDir()
	plannerPath := paths.WithRoot(root).WorktreeDir(sctx.Repo.ID, paths.CommandPlanWorktreeID(sctx.Run.ID))
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
		t.Fatalf("planner workspaces = %v, want reused managed path %q", plannerDirs, plannerPath)
	}
	if len(plannerHeads) != 2 || plannerHeads[0] == plannerHeads[1] {
		t.Fatalf("planner HEADs = %v, want refresh after pipeline commit", plannerHeads)
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

func TestPlanPipelineCommandContainsPrivateRefMutations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(context.Context, string) error
	}{
		{
			name: "branch",
			mutate: func(ctx context.Context, dir string) error {
				_, err := gitutil.Run(ctx, dir, "branch", "planner-created", "HEAD")
				return err
			},
		},
		{
			name: "switch",
			mutate: func(ctx context.Context, dir string) error {
				_, err := gitutil.Run(ctx, dir, "switch", "-c", "planner-created")
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			sourceBranch := gitCmd(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
			plannerDir := ""
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					plannerDir = opts.CWD
					if err := test.mutate(ctx, opts.CWD); err != nil {
						return nil, err
					}
					return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
				},
			}
			sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

			if _, err := planPipelineCommand(sctx, types.StepLint, "Select lint."); err == nil || !strings.Contains(err.Error(), "read-only") {
				t.Fatalf("planPipelineCommand() error = %v, want read-only violation", err)
			}
			if _, err := os.Stat(plannerDir); !os.IsNotExist(err) {
				t.Fatalf("mutated planner workspace was not discarded: %v", err)
			}
			if got := gitCmd(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != sourceBranch {
				t.Fatalf("source branch = %q, want %q", got, sourceBranch)
			}
			if _, err := gitutil.Run(context.Background(), dir, "show-ref", "--verify", "refs/heads/planner-created"); err == nil {
				t.Fatal("planner-created branch escaped into source refs")
			}
		})
	}
}

func TestPlanPipelineCommandContainsPrivateConfigAndIndexMutations(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sourceConfig := gitCmd(t, dir, "config", "--local", "--list", "-z")
	sourceIndex := gitCmd(t, dir, "ls-files", "-v", "-z")
	plannerDir := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerDir = opts.CWD
			if _, err := gitutil.Run(ctx, opts.CWD, "config", "--local", "planner.mutated", "true"); err != nil {
				return nil, err
			}
			if _, err := gitutil.Run(ctx, opts.CWD, "update-index", "--assume-unchanged", "feature.txt"); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("planPipelineCommand() error = %v, want read-only violation", err)
	}
	if _, err := os.Stat(plannerDir); !os.IsNotExist(err) {
		t.Fatalf("mutated planner workspace was not discarded: %v", err)
	}
	if got := gitCmd(t, dir, "config", "--local", "--list", "-z"); got != sourceConfig {
		t.Fatalf("source config changed:\n%s", got)
	}
	if got := gitCmd(t, dir, "ls-files", "-v", "-z"); got != sourceIndex {
		t.Fatalf("source index changed:\n%s", got)
	}
}

func TestPlanPipelineCommandDiscardsWorktreeMutation(t *testing.T) {
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

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("planPipelineCommand() error = %v, want read-only violation", err)
	}
	if _, err := os.Stat(plannerDir); !os.IsNotExist(err) {
		t.Fatalf("mutated planner workspace was not discarded: %v", err)
	}
	assertCommandPlanningFileContent(t, filepath.Join(dir, "feature.txt"), "feature code\n")
}

func TestPlanPipelineCommandCancellationUsesIndependentCleanup(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	plannerDir := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerDir = opts.CWD
			if err := os.WriteFile(filepath.Join(opts.CWD, "cancelled.txt"), []byte("mutation\n"), 0o644); err != nil {
				return nil, err
			}
			cancel()
			return nil, ctx.Err()
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Ctx = ctx

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("planPipelineCommand() error = %v, want cancellation and read-only violation", err)
	}
	if _, err := os.Stat(plannerDir); !os.IsNotExist(err) {
		t.Fatalf("cancelled planner workspace was not discarded: %v", err)
	}
}

func TestPlanPipelineCommandInspectionFailureDiscardsWorkspace(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	plannerDir := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerDir = opts.CWD
			if err := os.RemoveAll(filepath.Join(opts.CWD, ".git")); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepTest, "Select test."); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("planPipelineCommand() error = %v, want read-only inspection violation", err)
	}
	if _, err := os.Stat(plannerDir); !os.IsNotExist(err) {
		t.Fatalf("uninspectable planner workspace was not discarded: %v", err)
	}
}

func TestPlanPipelineCommandReusesCleanWorkspaceAfterAgentError(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	plannerDirs := make([]string, 0, 2)
	calls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerDirs = append(plannerDirs, opts.CWD)
			calls++
			if calls == 1 {
				return nil, errors.New("planner unavailable")
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err == nil || !strings.Contains(err.Error(), "planner unavailable") {
		t.Fatalf("first planPipelineCommand() error = %v, want agent failure", err)
	}
	if _, err := planPipelineCommand(sctx, types.StepTest, "Select test."); err != nil {
		t.Fatalf("second planPipelineCommand() error = %v", err)
	}
	if len(plannerDirs) != 2 || plannerDirs[0] != plannerDirs[1] {
		t.Fatalf("planner workspaces = %v, want clean reuse", plannerDirs)
	}
}

func TestPlanPipelineCommandPersistsPrivateCommandSeparatelyFromEvidence(t *testing.T) {
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

func assertCommandPlanningFileContent(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(content); got != want {
		t.Fatalf("%s content = %q, want %q", path, got, want)
	}
}
