package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

func TestPlanPipelineCommandRejectsControllerMetadataMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(context.Context, string) error
	}{
		{
			name: "hook",
			mutate: func(ctx context.Context, dir string) error {
				hooksDir, err := gitutil.Run(ctx, dir, "config", "--local", "--get", "core.hooksPath")
				if err != nil || hooksDir == "" {
					hooksDir = filepath.Join(dir, ".git", "hooks")
				} else if !filepath.IsAbs(hooksDir) {
					hooksDir = filepath.Join(dir, hooksDir)
				}
				if err := os.MkdirAll(hooksDir, 0o755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(hooksDir, "post-checkout"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
			},
		},
		{
			name: "ownership marker",
			mutate: func(_ context.Context, dir string) error {
				return os.WriteFile(filepath.Join(dir, ".git", "no-mistakes-command-planner"), []byte("mutated\n"), 0o644)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
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

			if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err == nil || !strings.Contains(err.Error(), "read-only") {
				t.Fatalf("planPipelineCommand() error = %v, want controller-metadata refusal", err)
			}
			if _, err := os.Stat(plannerDir); !os.IsNotExist(err) {
				t.Fatalf("planner with mutated controller metadata was not discarded: %v", err)
			}
		})
	}
}

func TestPlanPipelineCommandIsolatesAmbientGitConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook execution is platform-specific")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	hooksDir := t.TempDir()
	sentinelPath := filepath.Join(t.TempDir(), "ambient-hook-ran")
	hook := fmt.Sprintf("#!/bin/sh\nprintf triggered > %q\n", filepath.ToSlash(sentinelPath))
	if err := os.WriteFile(filepath.Join(hooksDir, "reference-transaction"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "remote.origin.url")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://example.invalid/ambient.git")
	t.Setenv("GIT_CONFIG_KEY_1", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_1", hooksDir)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			remote := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url")
			remote.Dir = opts.CWD
			remote.Env = append(os.Environ(), opts.Env...)
			if output, err := remote.Output(); err == nil {
				return nil, fmt.Errorf("planner inherited ambient remote: %s", strings.TrimSpace(string(output)))
			}
			hooks := exec.CommandContext(ctx, "git", "config", "--get", "core.hooksPath")
			hooks.Dir = opts.CWD
			hooks.Env = append(os.Environ(), opts.Env...)
			output, err := hooks.Output()
			if err != nil {
				return nil, fmt.Errorf("inspect planner hooks path: %w", err)
			}
			if got := strings.TrimSpace(string(output)); got != filepath.Join(opts.CWD, ".git", "hooks") {
				return nil, fmt.Errorf("planner hooks path = %q, want isolated repository hooks", got)
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinelPath); !os.IsNotExist(err) {
		t.Fatalf("ambient Git hook executed during planner lifecycle: %v", err)
	}
}

func TestPlanPipelineCommandRestartsPersistentAgentAroundPlannerEnvironment(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &plannerServerLifecycleAgent{}
	if _, err := ag.Run(context.Background(), agent.RunOpts{CWD: dir, Env: []string{"PRESTARTED=true"}}); err != nil {
		t.Fatal(err)
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err != nil {
		t.Fatal(err)
	}
	if ag.closeCalls != 2 {
		t.Fatalf("agent Close calls = %d, want one before and one after planning", ag.closeCalls)
	}
	if len(ag.serverStarts) != 2 {
		t.Fatalf("managed server starts = %d, want prestarted and planner servers", len(ag.serverStarts))
	}
	plannerEnv := strings.Join(ag.serverStarts[1], "\n")
	for _, want := range pipeline.CommandPlanningGitEnv() {
		if !strings.Contains(plannerEnv, want) {
			t.Fatalf("planner server environment missing %q: %v", want, ag.serverStarts[1])
		}
	}
	if ag.serverEnv != nil {
		t.Fatalf("planner server survived command planning: %v", ag.serverEnv)
	}

	if _, err := ag.Run(context.Background(), agent.RunOpts{CWD: dir, Env: []string{"AFTER=true"}}); err != nil {
		t.Fatal(err)
	}
	afterEnv := strings.Join(ag.serverStarts[2], "\n")
	if strings.Contains(afterEnv, "GIT_CONFIG_NOSYSTEM=1") {
		t.Fatalf("planner environment leaked into later managed server: %v", ag.serverStarts[2])
	}
}

func TestPlanPipelineCommandPreservesRunAndPostCloseErrors(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	runErr := errors.New("planner run failed")
	closeErr := errors.New("planner close failed")
	ag := &plannerLifecycleErrorAgent{
		runErr:    runErr,
		closeErrs: []error{nil, closeErr},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	_, err := planPipelineCommand(sctx, types.StepBuild, "Select build.")
	if !errors.Is(err, runErr) || !errors.Is(err, closeErr) {
		t.Fatalf("planPipelineCommand() error = %v, want run and post-close errors", err)
	}
	if ag.runCalls != 1 || ag.closeCalls != 2 {
		t.Fatalf("agent lifecycle = %d runs/%d closes, want 1 run/2 closes", ag.runCalls, ag.closeCalls)
	}
}

func TestPlanPipelineCommandStopsWhenPreCloseFails(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	closeErr := errors.New("pre-planner close failed")
	ag := &plannerLifecycleErrorAgent{closeErrs: []error{closeErr}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	_, err := planPipelineCommand(sctx, types.StepBuild, "Select build.")
	if !errors.Is(err, closeErr) {
		t.Fatalf("planPipelineCommand() error = %v, want pre-close error", err)
	}
	if ag.runCalls != 0 || ag.closeCalls != 1 {
		t.Fatalf("agent lifecycle = %d runs/%d closes, want 0 runs/1 close", ag.runCalls, ag.closeCalls)
	}
}

func TestPlanPipelineCommandRejectsUninitializedSubmoduleMutation(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	submoduleDir := t.TempDir()
	gitCmd(t, submoduleDir, "init")
	gitCmd(t, submoduleDir, "config", "user.email", "test@example.com")
	gitCmd(t, submoduleDir, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(submoduleDir, "dependency.txt"), []byte("dependency\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, submoduleDir, "add", "dependency.txt")
	gitCmd(t, submoduleDir, "commit", "-m", "initial dependency")
	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", submoduleDir, "dependency")
	gitCmd(t, dir, "commit", "-am", "add dependency")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	plannerDir := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerDir = opts.CWD
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, os.WriteFile(filepath.Join(opts.CWD, "dependency", "mutated.txt"), []byte("mutation\n"), 0o644)
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepTest, "Select test."); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("planPipelineCommand() error = %v, want gitlink mutation refusal", err)
	}
	if _, err := os.Stat(plannerDir); !os.IsNotExist(err) {
		t.Fatalf("planner with mutated uninitialized submodule was not discarded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dependency", "mutated.txt")); !os.IsNotExist(err) {
		t.Fatalf("planner submodule mutation reached source: %v", err)
	}
}

func TestPlanPipelineCommandRejectsNestedGitRepository(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	trackedDir := filepath.Join(dir, "nested")
	if err := os.Mkdir(trackedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trackedDir, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "nested/tracked.txt")
	gitCmd(t, dir, "commit", "-m", "add tracked directory")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	plannerDir := ""
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			plannerDir = opts.CWD
			if _, err := gitutil.Run(ctx, opts.CWD, "init", "nested"); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("planPipelineCommand() error = %v, want nested-repository refusal", err)
	}
	if _, err := os.Stat(plannerDir); !os.IsNotExist(err) {
		t.Fatalf("planner with nested Git repository was not discarded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(trackedDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("nested planner repository reached source: %v", err)
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

func TestPlanPipelineCommandRestoresDirectSourceMutation(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("planner bypass\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("planPipelineCommand() error = %v, want direct-source read-only violation", err)
	}
	assertCommandPlanningFileContent(t, filepath.Join(dir, "feature.txt"), "feature code\n")
}

func TestPlanPipelineCommandRestoresHiddenDirtySourceMutation(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "update-index", "--assume-unchanged", "feature.txt")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("prepared hidden change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	indexFlags := gitCmd(t, dir, "ls-files", "-v", "-z")
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("planner bypass\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("planPipelineCommand() error = %v, want hidden direct-source read-only violation", err)
	}
	assertCommandPlanningFileContent(t, filepath.Join(dir, "feature.txt"), "prepared hidden change\n")
	if got := gitCmd(t, dir, "ls-files", "-v", "-z"); got != indexFlags {
		t.Fatalf("source index flags = %q, want %q", got, indexFlags)
	}
}

func TestPlanPipelineCommandRestoresSmallIgnoredSourceMutations(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("prepared-output/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".gitignore")
	gitCmd(t, dir, "commit", "-m", "ignore prepared output")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	preparedDir := filepath.Join(dir, "prepared-output")
	if err := os.Mkdir(preparedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	preparedPath := filepath.Join(preparedDir, "hook-state.txt")
	if err := os.WriteFile(preparedPath, []byte("hook prepared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(preparedPath, []byte("planner rewrite\n"), 0o644); err != nil {
				return nil, err
			}
			if err := os.Chmod(preparedPath, 0o644); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(preparedDir, "planner-created.txt"), []byte("created\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("planPipelineCommand() error = %v, want ignored-state read-only violation", err)
	}
	assertCommandPlanningFileContent(t, preparedPath, "hook prepared\n")
	info, err := os.Stat(preparedPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("restored ignored file mode = %o, want 600", got)
	}
	if _, err := os.Stat(filepath.Join(preparedDir, "planner-created.txt")); !os.IsNotExist(err) {
		t.Fatalf("planner-created ignored file survived restore: %v", err)
	}
}

func TestPlanPipelineCommandRemovesCreatedIgnoredDirectory(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("planner-cache/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".gitignore")
	gitCmd(t, dir, "commit", "-m", "ignore planner cache")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	createdDir := filepath.Join(dir, "planner-cache")
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.MkdirAll(filepath.Join(createdDir, "nested"), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(createdDir, "nested", "created.txt"), []byte("created\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("planPipelineCommand() error = %v, want ignored-directory read-only violation", err)
	}
	if _, err := os.Stat(createdDir); !os.IsNotExist(err) {
		t.Fatalf("planner-created ignored directory survived restore: %v", err)
	}
}

func TestCommandPlanningIgnoredSnapshotSkipsLargeRootWithinBudget(t *testing.T) {
	dir, _, _ := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "node_modules")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 256 {
		path := filepath.Join(root, fmt.Sprintf("package-%03d.txt", i))
		if err := os.WriteFile(path, []byte("dependency\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := captureCommandPlanningIgnoredSnapshot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.roots) != 1 || snapshot.roots[0].path != "node_modules" || !snapshot.roots[0].skipped {
		t.Fatalf("large ignored snapshot = %+v, want one budget-skipped root", snapshot.roots)
	}
	if len(snapshot.roots[0].entries) != 0 {
		t.Fatalf("large ignored root retained %d partial entries", len(snapshot.roots[0].entries))
	}
}

func TestPlanPipelineCommandPreservesConcurrentSharedGitState(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if _, err := gitutil.Run(ctx, dir, "branch", "concurrent-update", "HEAD"); err != nil {
				return nil, err
			}
			if _, err := gitutil.Run(ctx, dir, "config", "--local", "concurrent.preserved", "true"); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := planPipelineCommand(sctx, types.StepBuild, "Select build."); err != nil {
		t.Fatalf("planPipelineCommand() error = %v, want concurrent shared state preserved", err)
	}
	if got := gitCmd(t, dir, "config", "--local", "--get", "concurrent.preserved"); got != "true" {
		t.Fatalf("concurrent config = %q, want true", got)
	}
	if _, err := gitutil.Run(context.Background(), dir, "show-ref", "--verify", "refs/heads/concurrent-update"); err != nil {
		t.Fatalf("concurrent ref was removed: %v", err)
	}
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

type plannerServerLifecycleAgent struct {
	serverEnv    []string
	serverStarts [][]string
	closeCalls   int
}

type plannerLifecycleErrorAgent struct {
	runErr     error
	closeErrs  []error
	runCalls   int
	closeCalls int
}

func (*plannerServerLifecycleAgent) Name() string { return "managed" }

func (a *plannerServerLifecycleAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	if a.serverEnv == nil {
		a.serverEnv = append([]string(nil), opts.Env...)
		a.serverStarts = append(a.serverStarts, append([]string(nil), opts.Env...))
	}
	return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
}

func (a *plannerServerLifecycleAgent) Close() error {
	a.closeCalls++
	a.serverEnv = nil
	return nil
}

func (*plannerLifecycleErrorAgent) Name() string { return "lifecycle-error" }

func (a *plannerLifecycleErrorAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	a.runCalls++
	return nil, a.runErr
}

func (a *plannerLifecycleErrorAgent) Close() error {
	index := a.closeCalls
	a.closeCalls++
	if index >= len(a.closeErrs) {
		return nil
	}
	return a.closeErrs[index]
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
