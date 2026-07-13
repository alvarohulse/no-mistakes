//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiRunPRNoteFileJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
	ctx := context.Background()
	upstreamURL := "https://github.com/parent-owner/no-mistakes.git"
	forkURL := "https://github.com/fork-owner/no-mistakes.git"
	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatalf("mkdir fork: %v", err)
	}
	if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, upstreamURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", upstreamURL); err != nil {
		t.Fatalf("set GitHub origin: %v\n%s", err, out)
	}

	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-pr-note.log")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "parent-owner/no-mistakes")
	if out, err := h.Run("init", "--fork-url", forkURL); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	branch := "feature/axi-pr-note-file"
	h.CommitChange(branch, "pr-note.txt", "change\n", "add PR note journey change")
	worktree := h.AddWorktree(branch)
	note := "## Notes\n\nRelease operators: preserve this wording verbatim.\n\n## Testing\n\n- This heading belongs to the author note."
	noteFile := filepath.Join(t.TempDir(), "pr-note.md")
	if err := os.WriteFile(noteFile, []byte(note), 0o644); err != nil {
		t.Fatalf("write PR note: %v", err)
	}

	out, err := h.RunInDir(worktree, "axi", "run", "--yes", "--intent", axiIntent, "--pr-note-file", noteFile)
	if err != nil {
		t.Fatalf("axi run --pr-note-file: %v\n%s", err, out)
	}
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	if got := readPersistedPRNote(t, h.NMHome, run.ID); got != note {
		t.Fatalf("persisted PR note = %q, want %q", got, note)
	}

	var body string
	for _, invocation := range readGHStubInvocations(t, ghLog) {
		if len(invocation.Args) >= 2 && invocation.Args[0] == "pr" && invocation.Args[1] == "create" {
			body = invocation.Body
			break
		}
	}
	if body == "" {
		t.Fatal("created PR body was not captured")
	}
	if !strings.Contains(body, note) {
		t.Fatalf("created PR body did not preserve the author note verbatim:\n%s", body)
	}
	if strings.Count(body, "## Notes") != 1 {
		t.Fatalf("created PR body wrapped the Notes heading more than once:\n%s", body)
	}
}

func readPersistedPRNote(t *testing.T, nmHome, runID string) string {
	t.Helper()
	database, err := db.Open(paths.WithRoot(nmHome).DB())
	if err != nil {
		t.Fatalf("open e2e db: %v", err)
	}
	defer database.Close()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatalf("get run %s: %v", runID, err)
	}
	if run == nil || run.PRNote == nil {
		t.Fatalf("run %s has no persisted PR note", runID)
	}
	return *run.PRNote
}
