package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	gitutil "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
	if _, err := os.Stat(plannerDir); !os.IsNotExist(err) {
		t.Fatalf("planner worktree survived: %v", err)
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
	if _, err := os.Stat(plannerDir); !os.IsNotExist(err) {
		t.Fatalf("planner worktree survived: %v", err)
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
	if _, err := os.Stat(plannerDir); !os.IsNotExist(err) {
		t.Fatalf("planner worktree survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "planner-error.txt")); !os.IsNotExist(err) {
		t.Fatalf("failed planner mutation reached pipeline worktree: %v", err)
	}
}
