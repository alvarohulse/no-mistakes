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
