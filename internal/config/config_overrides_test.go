package config

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadGlobalParsesOverridesKeyedByOwnerRepo(t *testing.T) {
	t.Parallel()
	cfg, err := LoadGlobalFromBytes([]byte(`
agent: auto
overrides:
  ScaleAPI/scaleapi:
    agent: codex
    commands:
      build: "yarn build"
      format: ""
    hooks:
      pr_body: "~/scripts/format-pr"
    prompts:
      test: |
        run the canonical suite
    ignore_patterns: ["**/dist/**"]
  other/project:
    commands:
      lint: "make lint"
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Overrides) != 2 {
		t.Fatalf("overrides = %d entries, want 2", len(cfg.Overrides))
	}
	override, ok := cfg.Overrides["scaleapi/scaleapi"]
	if !ok {
		t.Fatalf("overrides missing case-normalized scaleapi/scaleapi key: %v", cfg.Overrides)
	}
	if override.Agent != types.AgentCodex || override.Commands.Build != "yarn build" {
		t.Fatalf("override = %+v, want codex agent and yarn build", override)
	}
	if !override.Declares("commands.format") || override.Commands.Format != "" {
		t.Fatalf("explicit empty commands.format must stay declared and empty")
	}
	if !strings.Contains(override.Prompts.Test, "run the canonical suite") {
		t.Fatalf("prompts.test = %q", override.Prompts.Test)
	}
}

func TestLoadGlobalRejectsMalformedOverrideKeys(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		key  string
	}{
		{name: "single segment", key: "scaleapi"},
		{name: "three segments", key: "github.com/scaleapi/scaleapi"},
		{name: "url", key: "https://github.com/scaleapi/scaleapi"},
		{name: "scp-like remote", key: "git@github.com:scaleapi/scaleapi.git"},
		{name: "empty owner", key: "/scaleapi"},
		{name: "empty repo", key: "scaleapi/"},
		{name: "embedded whitespace", key: "scale api/scaleapi"},
		{name: "dot segment", key: "./scaleapi"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadGlobalFromBytes([]byte("overrides:\n  \"" + tt.key + "\":\n    commands:\n      lint: make lint\n"))
			if err == nil || !strings.Contains(err.Error(), "overrides key") {
				t.Fatalf("error = %v, want loud malformed-key refusal", err)
			}
		})
	}
}

func TestLoadGlobalRejectsOverrideDeclaringRepoBinding(t *testing.T) {
	t.Parallel()
	_, err := LoadGlobalFromBytes([]byte(`
overrides:
  scaleapi/scaleapi:
    repo: git@github.com:scaleapi/scaleapi.git
    commands:
      lint: make lint
`))
	if err == nil || !strings.Contains(err.Error(), "must not declare repo") {
		t.Fatalf("error = %v, want repo-binding refusal", err)
	}
}

func TestLoadGlobalRejectsNullAndCaseCollidingOverrides(t *testing.T) {
	t.Parallel()
	if _, err := LoadGlobalFromBytes([]byte("overrides:\n  scaleapi/scaleapi:\n")); err == nil || !strings.Contains(err.Error(), "must be a repo-config mapping") {
		t.Fatalf("null override error = %v, want mapping refusal", err)
	}
	_, err := LoadGlobalFromBytes([]byte(`
overrides:
  scaleapi/scaleapi:
    commands:
      lint: make lint
  Scaleapi/Scaleapi:
    commands:
      lint: make lint
`))
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("case-collision error = %v, want duplicate refusal", err)
	}
}

func TestLoadGlobalRejectsUnparseableOverride(t *testing.T) {
	t.Parallel()
	_, err := LoadGlobalFromBytes([]byte(`
overrides:
  scaleapi/scaleapi:
    commit:
      fix_message: "{{.Bogus"
`))
	if err == nil {
		t.Fatal("want loud config error for an invalid override commit template")
	}
}

func TestOverrideForRepoIdentityMatchesHostAgnosticOwnerRepo(t *testing.T) {
	t.Parallel()
	cfg, err := LoadGlobalFromBytes([]byte(`
overrides:
  scaleapi/scaleapi:
    commands:
      lint: make lint
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []string{
		"github.com/scaleapi/scaleapi",
		"gitlab.example.com/scaleapi/scaleapi",
	} {
		override, key, ok := cfg.OverrideForRepoIdentity(identity)
		if !ok || key != "scaleapi/scaleapi" || override.Commands.Lint != "make lint" {
			t.Fatalf("identity %q: override=%+v key=%q ok=%v, want match", identity, override, key, ok)
		}
	}
	for _, identity := range []string{
		"github.com/scaleapi/other",
		"github.com/group/scaleapi/scaleapi", // nested path never matches a two-segment key
		"scaleapi/scaleapi",                  // host-less strings are not identities
		"",
	} {
		if _, _, ok := cfg.OverrideForRepoIdentity(identity); ok && identity != "scaleapi/scaleapi" {
			t.Fatalf("identity %q unexpectedly matched", identity)
		}
	}
}

// TestOverrideForRepoIdentityRequiresHostPrefix pins the identity contract:
// the input is gate.RemoteIdentity output (host/owner/repo), so a bare
// owner/repo string must not match by accident of the Cut position.
func TestOverrideForRepoIdentityRequiresHostPrefix(t *testing.T) {
	t.Parallel()
	cfg, err := LoadGlobalFromBytes([]byte(`
overrides:
  scaleapi/scaleapi:
    commands:
      lint: make lint
`))
	if err != nil {
		t.Fatal(err)
	}
	// "scaleapi/scaleapi" cut at the first slash leaves path "scaleapi",
	// which matches no two-segment key.
	if _, _, ok := cfg.OverrideForRepoIdentity("scaleapi/scaleapi"); ok {
		t.Fatal("bare owner/repo input must not match: identities always carry a host prefix")
	}
}

func TestNormalizeOverrideKeyLowercases(t *testing.T) {
	t.Parallel()
	got, err := NormalizeOverrideKey("ScaleAPI/ScaleAPI")
	if err != nil || got != "scaleapi/scaleapi" {
		t.Fatalf("normalized = %q err = %v, want lowercase scaleapi/scaleapi", got, err)
	}
}
