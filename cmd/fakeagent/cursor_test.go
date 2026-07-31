package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractCursorInvocationRequiresContainedStdinShape(t *testing.T) {
	got, err := extractCursorInvocation(
		[]string{"--model", "gpt-5.6-sol", "-p", "--output-format", "stream-json", "--workspace", "/tmp/clean", "--add-dir", "/repo/worktree"},
		strings.NewReader("review the repo"),
	)
	if err != nil {
		t.Fatalf("extractCursorInvocation() error = %v", err)
	}
	if got.Prompt != "review the repo" || got.Workspace != "/tmp/clean" || got.Repo != "/repo/worktree" || got.Model != "gpt-5.6-sol" {
		t.Fatalf("invocation = %+v", got)
	}
}

func TestValidateCursorContainmentKeepsInstructionsInAddedRepo(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "clean")
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".cursor", "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	for path := range map[string]bool{
		filepath.Join(repo, "AGENTS.md"):                     true,
		filepath.Join(repo, ".cursorrules"):                  true,
		filepath.Join(repo, ".cursor", "rules", "audit.mdc"): true,
	} {
		if err := os.WriteFile(path, []byte("untrusted marker"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := validateCursorContainment(cursorInvocation{Workspace: workspace, Repo: repo}, workspace, workspace); err != nil {
		t.Fatalf("validateCursorContainment() error = %v", err)
	}
}
