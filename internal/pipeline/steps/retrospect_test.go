package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestRetrospectStep_DisabledSkips(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&RetrospectStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.Skipped {
		t.Fatalf("outcome = %#v, want skipped", outcome)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("agent calls = %d, want 0", len(ag.calls))
	}
}

func TestRetrospectStep_EnabledRecordsSummaryWithoutChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	var logs []string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"summary":"capture config tradeoff","notes":["doc step stayed focused"]}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Retrospect.Enabled = true
	sctx.Log = func(text string) { logs = append(logs, text) }

	outcome, err := (&RetrospectStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("outcome = %#v, want completed", outcome)
	}
	if outcome.FixSummary != "capture config tradeoff" {
		t.Fatalf("FixSummary = %q, want summary", outcome.FixSummary)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "Do not update documentation, source files, tests, config, git state, or any filesystem content") {
		t.Fatalf("prompt missing read-only instruction:\n%s", prompt)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("worktree changed: %q", got)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "doc step stayed focused") {
		t.Fatalf("logs = %q, want retrospective note", logs)
	}
}

func TestRetrospectStep_RejectsAgentWorktreeChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "RETRO.md"), []byte("notes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"wrote notes"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Retrospect.Enabled = true

	_, err := (&RetrospectStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected error for worktree changes")
	}
	if !strings.Contains(err.Error(), "retrospective step left worktree changes") {
		t.Fatalf("error = %v", err)
	}
}

func TestRetrospectStep_RejectsAgentCommit(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "RETRO.md"), []byte("notes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "RETRO.md")
			gitCmd(t, dir, "commit", "-m", "retrospective notes")
			return &agent.Result{Output: json.RawMessage(`{"summary":"committed notes"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Retrospect.Enabled = true

	_, err := (&RetrospectStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected error for agent-created commit")
	}
	if !strings.Contains(err.Error(), "retrospective step changed HEAD") {
		t.Fatalf("error = %v", err)
	}
}

func TestRetrospectStep_RejectsAgentEditToUntrackedFile(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	untrackedPath := filepath.Join(dir, "NOTES.md")
	if err := os.WriteFile(untrackedPath, []byte("untracked before retrospective\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(untrackedPath, []byte("untracked after retrospective\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"edited untracked file"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Retrospect.Enabled = true

	_, err := (&RetrospectStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected error for edit to untracked file")
	}
	if !strings.Contains(err.Error(), "retrospective step left worktree changes") {
		t.Fatalf("error = %v", err)
	}
}

func TestRetrospectStep_RejectsAgentFileInUntrackedDir(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	untrackedDir := filepath.Join(dir, "evidence")
	if err := os.MkdirAll(untrackedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(untrackedDir, "run.log"), []byte("evidence before retrospective\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(untrackedDir, "RETRO.md"), []byte("notes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"wrote notes in untracked dir"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Retrospect.Enabled = true

	_, err := (&RetrospectStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected error for new file inside untracked dir")
	}
	if !strings.Contains(err.Error(), "retrospective step left worktree changes") {
		t.Fatalf("error = %v", err)
	}
}

func TestRetrospectStep_AllowsUnchangedDirtyAndUntrackedState(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("dirty before retrospective\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	untrackedDir := filepath.Join(dir, "evidence")
	if err := os.MkdirAll(untrackedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(untrackedDir, "run.log"), []byte("evidence before retrospective\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statusBefore := gitStatusPorcelain(t, dir)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"summary":"no retrospective notes"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Retrospect.Enabled = true

	outcome, err := (&RetrospectStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("outcome = %#v, want completed", outcome)
	}
	if statusAfter := gitStatusPorcelain(t, dir); statusAfter != statusBefore {
		t.Fatalf("snapshot mutated worktree state:\nbefore: %q\nafter: %q", statusBefore, statusAfter)
	}
}

func TestRetrospectStep_RejectsAgentIgnoredEditInStoreInRepoEvidenceDir(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.png\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".gitignore")
	gitCmd(t, dir, "commit", "-m", "ignore pngs")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	evidenceDir := filepath.Join(dir, "evidence", "feature")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ignoredPath := filepath.Join(evidenceDir, "run.png")
	if err := os.WriteFile(ignoredPath, []byte("evidence before retrospective\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(ignoredPath, []byte("evidence edited by retrospective\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"edited ignored evidence"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Retrospect.Enabled = true
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}

	_, err := (&RetrospectStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected error for ignored edit inside in-repo evidence dir")
	}
	if !strings.Contains(err.Error(), "retrospective step left worktree changes") {
		t.Fatalf("error = %v", err)
	}
}

func TestRetrospectStep_RejectsAgentIgnoredFileInNewStoreInRepoEvidenceDir(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.png\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".gitignore")
	gitCmd(t, dir, "commit", "-m", "ignore pngs")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			evidenceDir := filepath.Join(dir, "evidence", "feature")
			if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(evidenceDir, "retro.png"), []byte("notes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"wrote ignored evidence"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Retrospect.Enabled = true
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}

	_, err := (&RetrospectStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected error for ignored file in newly created in-repo evidence dir")
	}
	if !strings.Contains(err.Error(), "retrospective step left worktree changes") {
		t.Fatalf("error = %v", err)
	}
}

func TestRetrospectStep_AllowsUntouchedIgnoredEvidenceContent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.png\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".gitignore")
	gitCmd(t, dir, "commit", "-m", "ignore pngs")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	evidenceDir := filepath.Join(dir, "evidence", "feature")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "run.png"), []byte("evidence before retrospective\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"summary":"no retrospective notes"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Retrospect.Enabled = true
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}

	outcome, err := (&RetrospectStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("outcome = %#v, want completed", outcome)
	}
}

func TestRetrospectStep_RejectsAgentEditToAlreadyDirtyFile(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	dirtyPath := filepath.Join(dir, "feature.txt")
	if err := os.WriteFile(dirtyPath, []byte("dirty before retrospective\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(dirtyPath, []byte("dirty after retrospective\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"edited dirty file"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Retrospect.Enabled = true

	_, err := (&RetrospectStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected error for edit to already-dirty file")
	}
	if !strings.Contains(err.Error(), "retrospective step left worktree changes") {
		t.Fatalf("error = %v", err)
	}
}
