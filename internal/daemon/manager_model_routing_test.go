package daemon

import (
	"context"
	"io/fs"
	"os/exec"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestNewPipelineAgents_CarriesFixerAndReviewCandidateModelIdentity(t *testing.T) {
	cfg := &config.Config{
		Agent:  types.AgentClaude,
		Agents: []types.AgentName{types.AgentClaude},
		StepAgents: map[types.StepName][]types.AgentName{
			types.StepBuild:  {types.AgentCodex},
			types.StepReview: {types.AgentCodex},
		},
		StepModels: map[types.StepName]config.ModelRoute{
			types.StepBuild:  {Name: "gpt-5.6-sol", Vendor: "openai"},
			types.StepReview: {Name: "gpt-5.6-sol", Vendor: "openai"},
		},
		ReviewCandidates: []config.ReviewCandidate{
			{Agent: types.AgentClaude, Model: config.ModelRoute{Name: "claude-opus-5", Vendor: "anthropic"}},
		},
	}
	routes, err := newPipelineAgents(context.Background(), cfg, fakeLookPath)
	if err != nil {
		t.Fatalf("newPipelineAgents() error = %v", err)
	}
	defer routes.Close()

	if got := agent.ConfiguredModel(routes.AgentForStep(types.StepBuild)); got != (agent.ModelIdentity{Name: "gpt-5.6-sol", Vendor: "openai"}) {
		t.Fatalf("build model = %#v", got)
	}
	if len(routes.routes.ReviewCandidates) != 1 {
		t.Fatalf("review candidate routes = %d, want 1", len(routes.routes.ReviewCandidates))
	}
	if got := agent.ConfiguredModel(routes.routes.ReviewCandidates[0]); got != (agent.ModelIdentity{Name: "claude-opus-5", Vendor: "anthropic"}) {
		t.Fatalf("review candidate model = %#v", got)
	}
	if got := agent.ConfiguredModel(routes.AgentForStep(types.StepTest)); got != (agent.ModelIdentity{}) {
		t.Fatalf("unconfigured test model = %#v, want empty", got)
	}
}

func TestNewPipelineAgents_CarriesACPModelIdentity(t *testing.T) {
	cfg := &config.Config{
		Agent:  types.AgentClaude,
		Agents: []types.AgentName{types.AgentClaude},
		StepAgents: map[types.StepName][]types.AgentName{
			types.StepReview: {"acp:cursor"},
		},
		StepModels: map[types.StepName]config.ModelRoute{
			types.StepReview: {Name: "claude-opus-5", Vendor: "anthropic"},
		},
		AgentArgsOverride: map[string][]string{
			"cursor": {"--model", "claude-sonnet-5"},
		},
	}
	routes, err := newPipelineAgents(context.Background(), cfg, fakeLookPath)
	if err != nil {
		t.Fatalf("newPipelineAgents() error = %v", err)
	}
	defer routes.Close()

	want := agent.ModelIdentity{Name: "claude-opus-5", Vendor: "anthropic"}
	if got := agent.ConfiguredModel(routes.AgentForStep(types.StepReview)); got != want {
		t.Fatalf("review model = %#v, want %#v", got, want)
	}
}

func TestNewPipelineAgents_AutoSelectsNativeCursorForCrossVendorModel(t *testing.T) {
	cfg := &config.Config{
		Agent:  types.AgentCodex,
		Agents: []types.AgentName{types.AgentCodex},
		StepAgents: map[types.StepName][]types.AgentName{
			types.StepReview: {types.AgentAuto},
		},
		StepModels: map[types.StepName]config.ModelRoute{
			types.StepReview: {Name: "claude-opus-5", Vendor: "anthropic"},
		},
	}
	routes, err := newPipelineAgents(context.Background(), cfg, func(bin string) (string, error) {
		switch bin {
		case "codex", "acpx", "cursor-agent":
			return "/usr/bin/" + bin, nil
		default:
			return "", &exec.Error{Name: bin, Err: fs.ErrNotExist}
		}
	})
	if err != nil {
		t.Fatalf("newPipelineAgents() error = %v", err)
	}
	defer routes.Close()

	if got := routes.AgentForStep(types.StepReview).Name(); got != "cursor" {
		t.Fatalf("review agent = %q, want cursor", got)
	}
	want := agent.ModelIdentity{Name: "claude-opus-5", Vendor: "anthropic"}
	if got := agent.ConfiguredModel(routes.AgentForStep(types.StepReview)); got != want {
		t.Fatalf("review model = %#v, want %#v", got, want)
	}
}
