package config

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadRepo_StepModelsCarryExplicitVendorIdentity(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte(`
intent:
  model: {name: claude-fable-5, vendor: anthropic}
refresh:
  model: {name: gpt-5.6-sol, vendor: openai}
review:
  agent: claude
  model: {name: claude-opus-5, vendor: anthropic}
  adversary_agent: codex
  adversary_model: {name: gpt-5.6-sol, vendor: openai}
test:
  model: {name: claude-fable-5, vendor: anthropic}
document:
  model: {name: claude-fable-5, vendor: anthropic}
lint:
  model: {name: claude-fable-5, vendor: anthropic}
pr:
  model: {name: claude-fable-5, vendor: anthropic}
ci:
  model: {name: claude-fable-5, vendor: anthropic}
`))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes() error = %v", err)
	}

	want := map[types.StepName]ModelRoute{
		types.StepIntent:   {Name: "claude-fable-5", Vendor: "anthropic"},
		types.StepRefresh:  {Name: "gpt-5.6-sol", Vendor: "openai"},
		types.StepReview:   {Name: "claude-opus-5", Vendor: "anthropic"},
		types.StepTest:     {Name: "claude-fable-5", Vendor: "anthropic"},
		types.StepDocument: {Name: "claude-fable-5", Vendor: "anthropic"},
		types.StepLint:     {Name: "claude-fable-5", Vendor: "anthropic"},
		types.StepPR:       {Name: "claude-fable-5", Vendor: "anthropic"},
		types.StepCI:       {Name: "claude-fable-5", Vendor: "anthropic"},
	}
	if got := cfg.ConfiguredStepModels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ConfiguredStepModels() = %#v, want %#v", got, want)
	}
	if got := cfg.Review.AdversaryAgents; !reflect.DeepEqual(got, []types.AgentName{types.AgentCodex}) {
		t.Fatalf("review adversary agents = %v, want [codex]", got)
	}
	if got := cfg.Review.AdversaryModel; got != (ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}) {
		t.Fatalf("review adversary model = %#v", got)
	}
}

func TestLoadRepo_StepModelSchemaIsStrict(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "scalar", yaml: "review:\n  model: claude-opus-5\n", want: "mapping"},
		{name: "missing name", yaml: "review:\n  model: {vendor: anthropic}\n", want: "name"},
		{name: "missing vendor", yaml: "review:\n  model: {name: claude-opus-5}\n", want: "vendor"},
		{name: "unknown model field", yaml: "review:\n  model: {name: claude-opus-5, vendor: anthropic, provider: bedrock}\n", want: "provider"},
		{name: "unknown review field", yaml: "review:\n  adversary: codex\n", want: "adversary"},
		{name: "non-canonical vendor", yaml: "review:\n  model: {name: gpt-5.6-sol, vendor: OpenAI}\n", want: "lowercase"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadRepoFromBytes([]byte(tt.yaml))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.want)) {
				t.Fatalf("LoadRepoFromBytes() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestMerge_StepModelOverridesGlobalWithoutAffectingOtherRoutes(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(`
agent: auto
review:
  model: {name: claude-fable-5, vendor: anthropic}
test:
  model: {name: claude-fable-5, vendor: anthropic}
`))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepoFromBytes([]byte(`
review:
  model: {name: gpt-5.6-sol, vendor: openai}
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(global, repo)
	if got := cfg.ConfiguredModelForStep(types.StepReview); got != (ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}) {
		t.Fatalf("review model = %#v", got)
	}
	if got := cfg.ConfiguredModelForStep(types.StepTest); got != (ModelRoute{Name: "claude-fable-5", Vendor: "anthropic"}) {
		t.Fatalf("test model = %#v", got)
	}
	if got := cfg.ConfiguredModelForStep(types.StepPush); got != (ModelRoute{}) {
		t.Fatalf("push model = %#v, want empty", got)
	}
}

func TestMerge_ReviewAdversaryOverridesGlobalFieldByField(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(`
review:
  model: {name: claude-opus-5, vendor: anthropic}
  adversary_agent: codex
  adversary_model: {name: gpt-5.6-sol, vendor: openai}
`))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("model inherits agent", func(t *testing.T) {
		repo, err := LoadRepoFromBytes([]byte(`
review:
  adversary_model: {name: gemini-3.5-pro, vendor: google}
`))
		if err != nil {
			t.Fatal(err)
		}
		cfg := Merge(global, repo)
		if got := cfg.ReviewAdversaryAgents; !reflect.DeepEqual(got, []types.AgentName{types.AgentCodex}) {
			t.Fatalf("adversary agents = %v, want [codex]", got)
		}
		if got := cfg.ReviewAdversaryModel; got != (ModelRoute{Name: "gemini-3.5-pro", Vendor: "google"}) {
			t.Fatalf("adversary model = %#v", got)
		}
	})

	t.Run("agent inherits model", func(t *testing.T) {
		repo, err := LoadRepoFromBytes([]byte(`
review:
  adversary_agent: pi
`))
		if err != nil {
			t.Fatal(err)
		}
		cfg := Merge(global, repo)
		if got := cfg.ReviewAdversaryAgents; !reflect.DeepEqual(got, []types.AgentName{types.AgentPi}) {
			t.Fatalf("adversary agents = %v, want [pi]", got)
		}
		if got := cfg.ReviewAdversaryModel; got != (ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}) {
			t.Fatalf("adversary model = %#v", got)
		}
	})
}

func TestOverlayRepoConfig_ModelOverridePreservesSiblingRouteFields(t *testing.T) {
	committed, err := LoadRepoFromBytes([]byte(`
review:
  agent: claude
  model: {name: claude-fable-5, vendor: anthropic}
`))
	if err != nil {
		t.Fatal(err)
	}
	machine, err := LoadRepoFromBytes([]byte(`
review:
  model: {name: claude-opus-5, vendor: anthropic}
`))
	if err != nil {
		t.Fatal(err)
	}
	got := OverlayRepoConfig(committed, machine)
	if got.Review.Agent != types.AgentClaude || !reflect.DeepEqual(got.Review.Agents, []types.AgentName{types.AgentClaude}) {
		t.Fatalf("review agent = %q/%v, want committed claude", got.Review.Agent, got.Review.Agents)
	}
	if got.Review.Model != (ModelRoute{Name: "claude-opus-5", Vendor: "anthropic"}) {
		t.Fatalf("review model = %#v", got.Review.Model)
	}
}

func TestEffectiveRepoConfig_StepModelsAndAdversaryAreTrustedSelectors(t *testing.T) {
	pushed := &RepoConfig{
		Review: ReviewRaw{
			StepAgentRaw:    StepAgentRaw{Agent: "acp:hostile", Agents: []types.AgentName{"acp:hostile"}, Model: ModelRoute{Name: "hostile", Vendor: "hostile"}},
			AdversaryAgent:  "acp:hostile-two",
			AdversaryAgents: []types.AgentName{"acp:hostile-two"},
			AdversaryModel:  ModelRoute{Name: "hostile-two", Vendor: "hostile-two"},
		},
	}
	trusted := &RepoConfig{
		Review: ReviewRaw{
			StepAgentRaw:    StepAgentRaw{Agent: types.AgentClaude, Agents: []types.AgentName{types.AgentClaude}, Model: ModelRoute{Name: "claude-opus-5", Vendor: "anthropic"}},
			AdversaryAgent:  types.AgentCodex,
			AdversaryAgents: []types.AgentName{types.AgentCodex},
			AdversaryModel:  ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"},
		},
	}

	got := EffectiveRepoConfig(pushed, trusted, false)
	if !reflect.DeepEqual(got.Review, trusted.Review) {
		t.Fatalf("effective review route = %#v, want trusted %#v", got.Review, trusted.Review)
	}
	if optIn := EffectiveRepoConfig(pushed, trusted, true); !reflect.DeepEqual(optIn.Review, pushed.Review) {
		t.Fatalf("opt-in review route = %#v, want pushed %#v", optIn.Review, pushed.Review)
	}
	if noTrusted := EffectiveRepoConfig(pushed, nil, false); !reflect.DeepEqual(noTrusted.Review, ReviewRaw{}) {
		t.Fatalf("review route without trusted config = %#v, want empty", noTrusted.Review)
	}
}

func TestResolveAgent_ModelNarrowsAutoAndRefusesACP(t *testing.T) {
	t.Run("auto probes only compatible backends", func(t *testing.T) {
		var probed []string
		cfg := &Config{
			Agent:  types.AgentCodex,
			Agents: []types.AgentName{types.AgentCodex},
			StepAgents: map[types.StepName][]types.AgentName{
				types.StepReview: {types.AgentAuto},
			},
			StepModels: map[types.StepName]ModelRoute{
				types.StepReview: {Name: "gpt-5.6-sol", Vendor: "openai"},
			},
		}
		err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
			probed = append(probed, bin)
			if bin == "codex" {
				return "/usr/bin/codex", nil
			}
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		})
		if err != nil {
			t.Fatalf("ResolveAgent() error = %v", err)
		}
		if got := cfg.StepAgents[types.StepReview]; !reflect.DeepEqual(got, []types.AgentName{types.AgentCodex}) {
			t.Fatalf("review agents = %v, want [codex]", got)
		}
		for _, bin := range probed {
			if bin == "claude" {
				t.Fatalf("auto probed incompatible claude backend: %v", probed)
			}
		}
	})

	t.Run("model plus ACP fails before launch", func(t *testing.T) {
		cfg := &Config{
			Agent:  types.AgentClaude,
			Agents: []types.AgentName{types.AgentClaude},
			StepAgents: map[types.StepName][]types.AgentName{
				types.StepReview: {types.AgentCursor},
			},
			StepModels: map[types.StepName]ModelRoute{
				types.StepReview: {Name: "claude-opus-5", Vendor: "anthropic"},
			},
		}
		err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
			return "/fake/bin/" + bin, nil
		})
		if err == nil {
			t.Fatal("ResolveAgent() accepted a model on ACP")
		}
		for _, want := range []string{"ACP", "model", "not supported"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should contain %q, got %v", want, err)
			}
		}
	})

	t.Run("auto fails loudly when no compatible backend is runnable", func(t *testing.T) {
		cfg := &Config{
			Agent:  types.AgentCodex,
			Agents: []types.AgentName{types.AgentCodex},
			StepAgents: map[types.StepName][]types.AgentName{
				types.StepReview: {types.AgentAuto},
			},
			StepModels: map[types.StepName]ModelRoute{
				types.StepReview: {Name: "gemini-3.5-pro", Vendor: "google"},
			},
		}
		err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
			if bin == "codex" {
				return "/usr/bin/codex", nil
			}
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		})
		if err == nil || !strings.Contains(err.Error(), "gemini-3.5-pro") || !strings.Contains(err.Error(), "google") {
			t.Fatalf("ResolveAgent() error = %v, want named-model failure", err)
		}
	})

	t.Run("explicit opencode requires provider-qualified model", func(t *testing.T) {
		cfg := &Config{
			Agent:  types.AgentCodex,
			Agents: []types.AgentName{types.AgentCodex},
			StepAgents: map[types.StepName][]types.AgentName{
				types.StepReview: {types.AgentOpenCode},
			},
			StepModels: map[types.StepName]ModelRoute{
				types.StepReview: {Name: "claude-opus-5", Vendor: "anthropic"},
			},
		}
		err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
			return "/fake/bin/" + bin, nil
		})
		if err == nil || !strings.Contains(err.Error(), "provider/model") {
			t.Fatalf("ResolveAgent() error = %v, want provider/model refusal", err)
		}
	})

	t.Run("auto skips opencode for unqualified model", func(t *testing.T) {
		var probed []string
		cfg := &Config{
			Agent:  types.AgentPi,
			Agents: []types.AgentName{types.AgentPi},
			StepAgents: map[types.StepName][]types.AgentName{
				types.StepReview: {types.AgentAuto},
			},
			StepModels: map[types.StepName]ModelRoute{
				types.StepReview: {Name: "gemini-3.5-pro", Vendor: "google"},
			},
		}
		err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
			probed = append(probed, bin)
			if bin == "opencode" || bin == "pi" {
				return "/usr/bin/" + bin, nil
			}
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		})
		if err != nil {
			t.Fatalf("ResolveAgent() error = %v", err)
		}
		if got := cfg.StepAgents[types.StepReview]; !reflect.DeepEqual(got, []types.AgentName{types.AgentPi}) {
			t.Fatalf("review agents = %v, want [pi]", got)
		}
		for _, bin := range probed {
			if bin == "opencode" {
				t.Fatalf("auto probed incompatible opencode backend: %v", probed)
			}
		}
	})

	t.Run("auto routes provider-qualified models to opencode", func(t *testing.T) {
		tests := []struct {
			name        string
			model       ModelRoute
			nativeAgent types.AgentName
			nativeBin   string
		}{
			{name: "anthropic", model: ModelRoute{Name: "anthropic/claude-opus-5", Vendor: "anthropic"}, nativeAgent: types.AgentClaude, nativeBin: "claude"},
			{name: "openai", model: ModelRoute{Name: "openai/gpt-5.6-sol", Vendor: "openai"}, nativeAgent: types.AgentCodex, nativeBin: "codex"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				cfg := &Config{
					Agent:  tt.nativeAgent,
					Agents: []types.AgentName{tt.nativeAgent},
					StepAgents: map[types.StepName][]types.AgentName{
						types.StepReview: {types.AgentAuto},
					},
					StepModels: map[types.StepName]ModelRoute{
						types.StepReview: tt.model,
					},
				}
				err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
					if bin == tt.nativeBin || bin == "opencode" {
						return "/usr/bin/" + bin, nil
					}
					return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
				})
				if err != nil {
					t.Fatalf("ResolveAgent() error = %v", err)
				}
				if got := cfg.StepAgents[types.StepReview]; !reflect.DeepEqual(got, []types.AgentName{types.AgentOpenCode}) {
					t.Fatalf("review agents = %v, want [opencode]", got)
				}
			})
		}
	})

	t.Run("explicit rovodev model fails before launch", func(t *testing.T) {
		cfg := &Config{
			Agent:  types.AgentCodex,
			Agents: []types.AgentName{types.AgentCodex},
			StepAgents: map[types.StepName][]types.AgentName{
				types.StepReview: {types.AgentRovoDev},
			},
			StepModels: map[types.StepName]ModelRoute{
				types.StepReview: {Name: "claude-opus-5", Vendor: "anthropic"},
			},
		}
		err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
			return "/fake/bin/" + bin, nil
		})
		if err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("ResolveAgent() error = %v, want unsupported Rovo Dev model refusal", err)
		}
	})
}

func TestResolveAgent_ReviewAdversaryMustBeCrossVendor(t *testing.T) {
	cfg := &Config{
		Agent:                 types.AgentClaude,
		Agents:                []types.AgentName{types.AgentClaude},
		StepModels:            map[types.StepName]ModelRoute{types.StepReview: {Name: "claude-opus-5", Vendor: "anthropic"}},
		ReviewAdversaryAgents: []types.AgentName{types.AgentClaude},
		ReviewAdversaryModel:  ModelRoute{Name: "claude-fable-5", Vendor: "anthropic"},
	}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		return "/fake/bin/" + bin, nil
	})
	if err == nil || !strings.Contains(err.Error(), "cross-vendor") {
		t.Fatalf("ResolveAgent() error = %v, want cross-vendor refusal", err)
	}
}
