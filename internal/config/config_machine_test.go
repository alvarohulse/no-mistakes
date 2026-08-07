package config

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestOverlayRepoConfigOverridesSpecifiedMachineLocalFields(t *testing.T) {
	t.Parallel()
	committed, err := LoadRepoFromBytes([]byte(`
agent: claude
commands:
  test: committed-test
  lint: committed-lint
ignore_patterns: [vendor/**]
review:
  agent: codex
`))
	if err != nil {
		t.Fatal(err)
	}
	machine, err := LoadRepoFromBytes([]byte(`
repo: git@github.com:owner/project.git
agent: opencode
commands:
  test: machine-test
review:
  agent: pi
`))
	if err != nil {
		t.Fatal(err)
	}

	got := OverlayRepoConfig(committed, machine)
	if got.Agent != types.AgentOpenCode {
		t.Fatalf("agent = %q, want opencode", got.Agent)
	}
	if got.Commands.Test != "machine-test" {
		t.Fatalf("commands.test = %q, want machine-test", got.Commands.Test)
	}
	if got.Commands.Lint != "committed-lint" {
		t.Fatalf("commands.lint = %q, want inherited committed-lint", got.Commands.Lint)
	}
	if !reflect.DeepEqual(got.IgnorePatterns, []string{"vendor/**"}) {
		t.Fatalf("ignore_patterns = %v, want committed value", got.IgnorePatterns)
	}
	if agents := got.ConfiguredStepAgents()[types.StepReview]; !reflect.DeepEqual(agents, []types.AgentName{types.AgentPi}) {
		t.Fatalf("review agents = %v, want [pi]", agents)
	}
}

func TestOverlayRepoConfigHonorsExplicitEmptyMachineLocalFields(t *testing.T) {
	t.Parallel()
	committed, err := LoadRepoFromBytes([]byte(`
agent: claude
commands:
  test: committed-test
ignore_patterns: [vendor/**]
review:
  agent: codex
`))
	if err != nil {
		t.Fatal(err)
	}
	machine, err := LoadRepoFromBytes([]byte(`
repo: https://github.com/owner/project
agent: ""
commands:
  test: ""
ignore_patterns: []
review:
  agent: ""
`))
	if err != nil {
		t.Fatal(err)
	}

	got := OverlayRepoConfig(committed, machine)
	if got.Agent != "" || len(got.Agents) != 0 {
		t.Fatalf("agent route = (%q, %v), want cleared", got.Agent, got.Agents)
	}
	if got.Commands.Test != "" {
		t.Fatalf("commands.test = %q, want cleared", got.Commands.Test)
	}
	if got.IgnorePatterns == nil || len(got.IgnorePatterns) != 0 {
		t.Fatalf("ignore_patterns = %#v, want explicit empty list", got.IgnorePatterns)
	}
	if _, ok := got.ConfiguredStepAgents()[types.StepReview]; ok {
		t.Fatalf("review route was not cleared: %v", got.ConfiguredStepAgents())
	}
}

func TestOverlayRepoConfigOverridesNestedSettingsWithoutReplacingSiblings(t *testing.T) {
	t.Parallel()
	zero := 0
	one := 1
	storeInRepo := true
	committed := &RepoConfig{
		AutoFix: AutoFixRaw{Review: &one, Test: &one},
		Test: TestRaw{Evidence: EvidenceRaw{
			StoreInRepo: &storeInRepo,
			Dir:         stringPtr("committed-evidence"),
		}},
	}
	machine, err := LoadRepoFromBytes([]byte(`
repo: https://github.com/owner/project
auto_fix:
  review: 0
test:
  evidence:
    dir: machine-evidence
`))
	if err != nil {
		t.Fatal(err)
	}

	got := OverlayRepoConfig(committed, machine)
	if got.AutoFix.Review == nil || *got.AutoFix.Review != zero {
		t.Fatalf("auto_fix.review = %v, want 0", got.AutoFix.Review)
	}
	if got.AutoFix.Test == nil || *got.AutoFix.Test != one {
		t.Fatalf("auto_fix.test = %v, want inherited 1", got.AutoFix.Test)
	}
	if got.Test.Evidence.StoreInRepo == nil || !*got.Test.Evidence.StoreInRepo {
		t.Fatalf("test.evidence.store_in_repo = %v, want inherited true", got.Test.Evidence.StoreInRepo)
	}
	if got.Test.Evidence.Dir == nil || *got.Test.Evidence.Dir != "machine-evidence" {
		t.Fatalf("test.evidence.dir = %v, want machine-evidence", got.Test.Evidence.Dir)
	}
}

// TestOverlayRepoConfigOverridesPrompts proves the machine-local overlay
// (NM_REPO_CONFIG) carries prompt additions: present keys replace the
// committed value (including explicit empties), absent keys inherit it.
func TestOverlayRepoConfigOverridesPrompts(t *testing.T) {
	t.Parallel()
	committed, err := LoadRepoFromBytes([]byte(`
prompts:
  shared: committed shared
  test: committed test
  review: committed review
`))
	if err != nil {
		t.Fatal(err)
	}
	machine, err := LoadRepoFromBytes([]byte(`
repo: https://github.com/owner/project
prompts:
  test: run the scaleapi testing commands
  review: ""
`))
	if err != nil {
		t.Fatal(err)
	}

	got := OverlayRepoConfig(committed, machine)
	if got.Prompts.Test != "run the scaleapi testing commands" {
		t.Fatalf("prompts.test = %q, want machine value", got.Prompts.Test)
	}
	if got.Prompts.Shared != "committed shared" {
		t.Fatalf("prompts.shared = %q, want inherited committed value", got.Prompts.Shared)
	}
	if got.Prompts.Review != "" {
		t.Fatalf("prompts.review = %q, want explicitly cleared", got.Prompts.Review)
	}
}

func stringPtr(value string) *string { return &value }
