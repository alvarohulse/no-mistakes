package config

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadGlobal_ManagedReviewCandidates(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(`
managed: true
review:
  agent: cursor
  model: {name: gpt-5.6-luna, vendor: openai}
  candidates:
    - agent: claude
      model: {name: claude-opus-5, vendor: anthropic}
    - agent: cursor
      model: {name: grok-4.6, vendor: xai}
      optional: true
`))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes() error = %v", err)
	}
	if !cfg.Managed {
		t.Fatal("managed mode was not preserved")
	}
	want := []ReviewCandidate{
		{Agent: types.AgentClaude, Model: ModelRoute{Name: "claude-opus-5", Vendor: "anthropic"}},
		{Agent: types.AgentCursor, Model: ModelRoute{Name: "grok-4.6", Vendor: "xai"}, Optional: true},
	}
	if got := cfg.Review.Candidates; !reflect.DeepEqual(got, want) {
		t.Fatalf("review candidates = %#v, want %#v", got, want)
	}
}

func TestLoadGlobal_RejectsDuplicateReviewCandidates(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte(`
review:
  candidates:
    - agent: claude
      model: {name: claude-opus-5, vendor: anthropic}
    - agent: claude
      model: {name: claude-opus-5, vendor: anthropic}
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("LoadGlobalFromBytes() error = %v, want duplicate-candidate refusal", err)
	}
}

func TestLoadRepo_ReviewCandidateSchemaIsStrict(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "missing agent",
			yaml: "review:\n  candidates:\n    - model: {name: claude-opus-5, vendor: anthropic}\n",
			want: "agent",
		},
		{
			name: "missing model",
			yaml: "review:\n  candidates:\n    - agent: claude\n",
			want: "model",
		},
		{
			name: "automatic harness",
			yaml: "review:\n  candidates:\n    - agent: auto\n      model: {name: claude-opus-5, vendor: anthropic}\n",
			want: "explicit",
		},
		{
			name: "duplicate",
			yaml: "review:\n  candidates:\n    - agent: claude\n      model: {name: claude-opus-5, vendor: anthropic}\n    - agent: claude\n      model: {name: claude-opus-5, vendor: anthropic}\n      optional: true\n",
			want: "duplicate",
		},
		{
			name: "unknown field",
			yaml: "review:\n  candidates:\n    - agent: claude\n      model: {name: claude-opus-5, vendor: anthropic}\n      weight: 2\n",
			want: "weight",
		},
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

func TestResolveAgent_ReviewCandidatesRespectRequiredAndOptionalAvailability(t *testing.T) {
	t.Run("drops unavailable optional candidate", func(t *testing.T) {
		cfg := completeManagedConfig()
		cfg.ReviewCandidates = []ReviewCandidate{
			{Agent: types.AgentCodex, Model: ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}},
			{Agent: types.AgentCursor, Model: ModelRoute{Name: "grok-4.6", Vendor: "xai"}, Optional: true},
		}
		if err := cfg.ResolveAgent(context.Background(), codexOnlyLookPath); err != nil {
			t.Fatalf("ResolveAgent() error = %v", err)
		}
		want := []ReviewCandidate{{Agent: types.AgentCodex, Model: ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}}}
		if !reflect.DeepEqual(cfg.ReviewCandidates, want) {
			t.Fatalf("resolved candidates = %#v, want %#v", cfg.ReviewCandidates, want)
		}
	})

	t.Run("drops optional candidate whose model catalog omits the model", func(t *testing.T) {
		originalProbe := loadReviewCandidateModelCatalog
		loadReviewCandidateModelCatalog = func(_ context.Context, name types.AgentName, _ string) (map[string]bool, error) {
			if name == types.AgentCursor {
				return map[string]bool{"gpt-5.6-luna-medium": true}, nil
			}
			return nil, nil
		}
		t.Cleanup(func() { loadReviewCandidateModelCatalog = originalProbe })

		cfg := completeManagedConfig()
		cfg.ReviewCandidates = []ReviewCandidate{
			{Agent: types.AgentCodex, Model: ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}},
			{Agent: types.AgentCursor, Model: ModelRoute{Name: "grok-4.6", Vendor: "xai"}, Optional: true},
		}
		lookPath := func(bin string) (string, error) { return "/usr/bin/" + bin, nil }
		if err := cfg.ResolveAgent(context.Background(), lookPath); err != nil {
			t.Fatalf("ResolveAgent() error = %v", err)
		}
		want := []ReviewCandidate{{Agent: types.AgentCodex, Model: ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}}}
		if !reflect.DeepEqual(cfg.ReviewCandidates, want) {
			t.Fatalf("resolved candidates = %#v, want %#v", cfg.ReviewCandidates, want)
		}
	})

	t.Run("catalog probe failure is not treated as optional absence", func(t *testing.T) {
		originalProbe := loadReviewCandidateModelCatalog
		loadReviewCandidateModelCatalog = func(context.Context, types.AgentName, string) (map[string]bool, error) {
			return nil, exec.ErrNotFound
		}
		t.Cleanup(func() { loadReviewCandidateModelCatalog = originalProbe })

		cfg := completeManagedConfig()
		cfg.ReviewCandidates = []ReviewCandidate{
			{Agent: types.AgentCodex, Model: ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}},
			{Agent: types.AgentCursor, Model: ModelRoute{Name: "grok-4.6", Vendor: "xai"}, Optional: true},
		}
		err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) { return "/usr/bin/" + bin, nil })
		if err == nil || !strings.Contains(err.Error(), "probe review candidate") {
			t.Fatalf("ResolveAgent() error = %v, want catalog probe failure", err)
		}
	})

	t.Run("fails unavailable required candidate", func(t *testing.T) {
		cfg := completeManagedConfig()
		cfg.ReviewCandidates = []ReviewCandidate{{Agent: types.AgentCursor, Model: ModelRoute{Name: "grok-4.6", Vendor: "xai"}}}
		err := cfg.ResolveAgent(context.Background(), codexOnlyLookPath)
		if err == nil || !strings.Contains(err.Error(), "required review candidate") || !strings.Contains(err.Error(), "cursor") {
			t.Fatalf("ResolveAgent() error = %v, want required-candidate refusal", err)
		}
	})

	t.Run("fails empty usable pool", func(t *testing.T) {
		cfg := completeManagedConfig()
		cfg.ReviewCandidates = []ReviewCandidate{{Agent: types.AgentCursor, Model: ModelRoute{Name: "grok-4.6", Vendor: "xai"}, Optional: true}}
		err := cfg.ResolveAgent(context.Background(), codexOnlyLookPath)
		if err == nil || !strings.Contains(err.Error(), "no usable review candidates") {
			t.Fatalf("ResolveAgent() error = %v, want empty-pool refusal", err)
		}
	})
}

func TestResolveAgent_ManagedModeRequiresEveryModelRoute(t *testing.T) {
	cfg := completeManagedConfig()
	delete(cfg.StepModels, types.StepCI)
	err := cfg.ResolveAgent(context.Background(), codexOnlyLookPath)
	if err == nil || !strings.Contains(err.Error(), "managed") || !strings.Contains(err.Error(), "ci") || !strings.Contains(err.Error(), "model") {
		t.Fatalf("ResolveAgent() error = %v, want missing CI model refusal", err)
	}

	cfg = completeManagedConfig()
	delete(cfg.StepAgents, types.StepDocument)
	err = cfg.ResolveAgent(context.Background(), codexOnlyLookPath)
	if err == nil || !strings.Contains(err.Error(), "managed") || !strings.Contains(err.Error(), "document") || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("ResolveAgent() error = %v, want missing Document agent refusal", err)
	}
}

func TestValidateManagedStepPlan_RejectsUnclassifiedExecutableStep(t *testing.T) {
	cfg := completeManagedConfig()
	err := cfg.ValidateManagedStepPlan([]types.StepName{types.StepReview, "new-model-step"})
	if err == nil || !strings.Contains(err.Error(), "classification") || !strings.Contains(err.Error(), "new-model-step") {
		t.Fatalf("ValidateManagedStepPlan() error = %v, want unclassified-step refusal", err)
	}
}

func completeManagedConfig() *Config {
	stepAgents := make(map[types.StepName][]types.AgentName)
	stepModels := make(map[types.StepName]ModelRoute)
	for _, step := range types.AllSteps() {
		if step == types.StepPush {
			continue
		}
		stepAgents[step] = []types.AgentName{types.AgentCodex}
		stepModels[step] = ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}
	}
	return &Config{
		Managed:          true,
		Agent:            types.AgentCodex,
		Agents:           []types.AgentName{types.AgentCodex},
		StepAgents:       stepAgents,
		StepModels:       stepModels,
		ReviewCandidates: []ReviewCandidate{{Agent: types.AgentCodex, Model: ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}}},
	}
}

func codexOnlyLookPath(bin string) (string, error) {
	if bin == "codex" {
		return "/usr/bin/codex", nil
	}
	return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
}
