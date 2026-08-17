package config

import (
	"strings"
	"testing"
)

func TestEffectiveRepoConfigKeepsPreflightBehindTrustedCommandBoundary(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte("preflight:\n  - echo pushed\n"))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadRepoFromBytes([]byte("preflight:\n  - echo trusted\n"))
	if err != nil {
		t.Fatal(err)
	}

	got := EffectiveRepoConfig(pushed, trusted, false)
	if len(got.Preflight) != 1 || got.Preflight[0].Run != "echo trusted" {
		t.Fatalf("trusted preflight = %+v", got.Preflight)
	}
	optedIn := EffectiveRepoConfig(pushed, trusted, true)
	if len(optedIn.Preflight) != 1 || optedIn.Preflight[0].Run != "echo pushed" {
		t.Fatalf("opted-in preflight = %+v", optedIn.Preflight)
	}
	withoutTrusted := EffectiveRepoConfig(pushed, nil, false)
	if len(withoutTrusted.Preflight) != 0 {
		t.Fatalf("untrusted preflight survived without trusted config: %+v", withoutTrusted.Preflight)
	}
}

func TestMachineOverrideReplacesPreflightAsOneOrderedList(t *testing.T) {
	base, err := LoadRepoFromBytes([]byte("preflight:\n  - echo committed-one\n  - echo committed-two\n"))
	if err != nil {
		t.Fatal(err)
	}
	override, err := LoadRepoFromBytes([]byte(`
preflight:
  - run: echo machine
    runner: {executable: zsh, args: [-lc]}
`))
	if err != nil {
		t.Fatal(err)
	}

	got := OverlayRepoConfig(base, override)
	if len(got.Preflight) != 1 || got.Preflight[0].Run != "echo machine" || got.Preflight[0].Runner == nil {
		t.Fatalf("overlaid preflight = %+v", got.Preflight)
	}
	got.Preflight[0].Run = "mutated"
	if override.Preflight[0].Run != "echo machine" {
		t.Fatal("preflight overlay aliased its input")
	}
}

func TestGlobalConfigAllowsPreflightOnlyInsideRepositoryOverride(t *testing.T) {
	if _, err := LoadGlobalFromBytes([]byte("preflight:\n  - echo global\n")); err == nil || !strings.Contains(err.Error(), "field preflight not found") {
		t.Fatalf("top-level preflight error = %v", err)
	}
	global, err := LoadGlobalFromBytes([]byte("overrides:\n  owner/repo:\n    preflight:\n      - echo override\n"))
	if err != nil {
		t.Fatal(err)
	}
	override := global.Overrides["owner/repo"]
	if override == nil || len(override.Preflight) != 1 || override.Preflight[0].Run != "echo override" {
		t.Fatalf("override preflight = %+v", override)
	}
}

func TestMergeCopiesResolvedPreflightCommands(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte("preflight:\n  - echo ready\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(DefaultGlobalConfig(), repo)
	if len(cfg.Preflight) != 1 || cfg.Preflight[0].Run != "echo ready" {
		t.Fatalf("resolved preflight = %+v", cfg.Preflight)
	}
	cfg.Preflight[0].Run = "mutated"
	if repo.Preflight[0].Run != "echo ready" {
		t.Fatal("resolved preflight aliased repo config")
	}
}
