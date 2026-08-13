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

func TestExecutorReusesPreparedCommandPlanningWorkspace(t *testing.T) {
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

func TestPlanPipelineCommandRestoresExistingSourceUntrackedMutation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	preparedPath := filepath.Join(dir, "prepared.txt")
	if err := os.WriteFile(preparedPath, []byte("prepared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(preparedPath, []byte("mutated\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	_, err := planPipelineCommand(sctx, types.StepBuild, "Select build.")
	if err == nil || !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("planPipelineCommand() error = %v, want source mutation refusal", err)
	}
	content, readErr := os.ReadFile(preparedPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(content) != "prepared\n" {
		t.Fatalf("prepared source content = %q, want restored content", content)
	}
}

func TestPlanPipelineCommandRestoresSourceUntrackedModeAndSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	scriptPath := filepath.Join(dir, "prepared.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(scriptPath, 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "target-a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "target-b"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "prepared-link")
	if err := os.Symlink("target-a", linkPath); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.Chmod(scriptPath, 0o600); err != nil {
				return nil, err
			}
			if err := os.Remove(linkPath); err != nil {
				return nil, err
			}
			if err := os.Symlink("target-b", linkPath); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	_, err := planPipelineCommand(sctx, types.StepBuild, "Select build.")
	if err == nil || !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("planPipelineCommand() error = %v, want source mutation refusal", err)
	}
	info, statErr := os.Stat(scriptPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if got := info.Mode().Perm(); got != 0o751 {
		t.Fatalf("prepared script mode = %o, want 751", got)
	}
	target, readErr := os.Readlink(linkPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if target != "target-a" {
		t.Fatalf("prepared symlink target = %q, want target-a", target)
	}
}

func TestPlanPipelineCommandRestoresExistingPlannerUntrackedMutation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "prepared.txt"), []byte("prepared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plannerDir := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerDir = opts.CWD
			if err := os.WriteFile(filepath.Join(opts.CWD, "prepared.txt"), []byte("mutated\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	_, err := planPipelineCommand(sctx, types.StepTest, "Select test.")
	if err == nil || !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("planPipelineCommand() error = %v, want planner mutation refusal", err)
	}
	content, readErr := os.ReadFile(filepath.Join(plannerDir, "prepared.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(content) != "prepared\n" {
		t.Fatalf("prepared planner content = %q, want restored content", content)
	}
}

func TestPlanPipelineCommandRestoresExistingTrackedMutation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sourcePath := filepath.Join(dir, "feature.txt")
	if err := os.WriteFile(sourcePath, []byte("prepared tracked\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sourcePath, 0o640); err != nil {
		t.Fatal(err)
	}
	plannerPath := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerPath = filepath.Join(opts.CWD, "feature.txt")
			if err := os.WriteFile(sourcePath, []byte("source mutation\n"), 0o600); err != nil {
				return nil, err
			}
			if err := os.WriteFile(plannerPath, []byte("planner mutation\n"), 0o600); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err == nil || !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("planPipelineCommand() error = %v, want tracked mutation refusal", err)
	}
	assertCommandPlanningFileContent(t, sourcePath, "prepared tracked\n")
	assertCommandPlanningFileContent(t, plannerPath, "prepared tracked\n")
	if info, err := os.Stat(sourcePath); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("restored source mode = %o, want 640", got)
	}
}

func TestPlanPipelineCommandRestoresExistingIgnoredMutation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("prepared-cache/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "prepared-cache")
	if err := os.Mkdir(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(cacheDir, "cache.bin")
	if err := os.WriteFile(sourcePath, []byte("prepared ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nestedGitDir := filepath.Join(cacheDir, ".git")
	if err := os.Mkdir(nestedGitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sourceMetadataPath := filepath.Join(nestedGitDir, "config")
	if err := os.WriteFile(sourceMetadataPath, []byte("prepared metadata\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(cacheDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(cacheDir, 0o700) })
	}
	plannerPath := ""
	plannerMetadataPath := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerCacheDir := filepath.Join(opts.CWD, "prepared-cache")
			plannerPath = filepath.Join(plannerCacheDir, "cache.bin")
			plannerMetadataPath = filepath.Join(opts.CWD, "prepared-cache", ".git", "config")
			if runtime.GOOS != "windows" {
				if err := os.Chmod(cacheDir, 0o700); err != nil {
					return nil, err
				}
				if err := os.Chmod(plannerCacheDir, 0o700); err != nil {
					return nil, err
				}
			}
			if err := os.WriteFile(sourcePath, []byte("source mutation\n"), 0o644); err != nil {
				return nil, err
			}
			if err := os.WriteFile(sourceMetadataPath, []byte("source metadata mutation\n"), 0o600); err != nil {
				return nil, err
			}
			if err := os.WriteFile(plannerPath, []byte("planner mutation\n"), 0o644); err != nil {
				return nil, err
			}
			if err := os.WriteFile(plannerMetadataPath, []byte("planner metadata mutation\n"), 0o600); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepTest, "Select test."); err == nil || !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("planPipelineCommand() error = %v, want ignored mutation refusal", err)
	}
	assertCommandPlanningFileContent(t, sourcePath, "prepared ignored\n")
	assertCommandPlanningFileContent(t, plannerPath, "prepared ignored\n")
	assertCommandPlanningFileContent(t, sourceMetadataPath, "prepared metadata\n")
	assertCommandPlanningFileContent(t, plannerMetadataPath, "prepared metadata\n")
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(cacheDir); err != nil {
			t.Fatal(err)
		} else if got := info.Mode().Perm(); got != 0o500 {
			t.Fatalf("restored ignored directory mode = %o, want 500", got)
		}
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

func TestPlanPipelineCommandRestoresIndexFlagMutation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	plannerDir := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerDir = opts.CWD
			if _, err := gitutil.Run(ctx, opts.CWD, "update-index", "--assume-unchanged", "feature.txt"); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err == nil || !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("planPipelineCommand() error = %v, want index flag mutation refusal", err)
	}
	if got := gitCmd(t, plannerDir, "ls-files", "-v", "feature.txt"); strings.HasPrefix(got, "h ") {
		t.Fatalf("planner assume-unchanged flag survived restore: %q", got)
	}
}

func TestPlanPipelineCommandRestoresInitializedSubmoduleMutation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	submoduleRepo := t.TempDir()
	gitCmd(t, submoduleRepo, "init")
	gitCmd(t, submoduleRepo, "config", "user.email", "test@example.com")
	gitCmd(t, submoduleRepo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(submoduleRepo, "dependency.txt"), []byte("prepared submodule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, submoduleRepo, "add", "dependency.txt")
	gitCmd(t, submoduleRepo, "commit", "-m", "initial dependency")
	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", submoduleRepo, "dependency")
	gitCmd(t, dir, "commit", "-am", "add dependency")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	sourcePath := filepath.Join(dir, "dependency", "dependency.txt")
	plannerPath := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerPath = filepath.Join(opts.CWD, "dependency", "dependency.txt")
			if _, err := gitutil.Run(ctx, opts.CWD, "status", "--porcelain"); err != nil {
				return nil, err
			}
			if err := os.WriteFile(sourcePath, []byte("source mutation\n"), 0o644); err != nil {
				return nil, err
			}
			if err := os.WriteFile(plannerPath, []byte("planner mutation\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepTest, "Select test."); err == nil || !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("planPipelineCommand() error = %v, want submodule mutation refusal", err)
	}
	assertCommandPlanningFileContent(t, sourcePath, "prepared submodule\n")
	assertCommandPlanningFileContent(t, plannerPath, "prepared submodule\n")
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

func TestPlanPipelineCommandRestoresMutationWhenAgentIsCancelled(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	preparedPath := filepath.Join(dir, "prepared.txt")
	if err := os.WriteFile(preparedPath, []byte("prepared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	plannerPath := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerPath = filepath.Join(opts.CWD, "prepared.txt")
			if err := os.WriteFile(preparedPath, []byte("source mutation\n"), 0o644); err != nil {
				return nil, err
			}
			if err := os.WriteFile(plannerPath, []byte("planner mutation\n"), 0o644); err != nil {
				return nil, err
			}
			cancel()
			return nil, ctx.Err()
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Ctx = ctx

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); !errors.Is(err, context.Canceled) {
		t.Fatalf("planPipelineCommand() error = %v, want context cancellation", err)
	}
	assertCommandPlanningFileContent(t, preparedPath, "prepared\n")
	assertCommandPlanningFileContent(t, plannerPath, "prepared\n")
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
