package daemon

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestNewPipelineAgents_CarriesPrimaryAndAdversaryModelIdentity(t *testing.T) {
	cfg := &config.Config{
		Agent:  types.AgentClaude,
		Agents: []types.AgentName{types.AgentClaude},
		StepAgents: map[types.StepName][]types.AgentName{
			types.StepReview: {types.AgentCodex},
		},
		StepModels: map[types.StepName]config.ModelRoute{
			types.StepReview: {Name: "gpt-5.6-sol", Vendor: "openai"},
		},
		ReviewAdversaryAgents: []types.AgentName{types.AgentClaude},
		ReviewAdversaryModel:  config.ModelRoute{Name: "claude-opus-5", Vendor: "anthropic"},
	}
	routes, err := newPipelineAgents(context.Background(), cfg, fakeLookPath)
	if err != nil {
		t.Fatalf("newPipelineAgents() error = %v", err)
	}
	defer routes.Close()

	if got := agent.ConfiguredModel(routes.AgentForStep(types.StepReview)); got != (agent.ModelIdentity{Name: "gpt-5.6-sol", Vendor: "openai"}) {
		t.Fatalf("primary review model = %#v", got)
	}
	if got := agent.ConfiguredModel(routes.routes.AdversaryForReview()); got != (agent.ModelIdentity{Name: "claude-opus-5", Vendor: "anthropic"}) {
		t.Fatalf("adversary review model = %#v", got)
	}
	if got := agent.ConfiguredModel(routes.AgentForStep(types.StepTest)); got != (agent.ModelIdentity{}) {
		t.Fatalf("unconfigured test model = %#v, want empty", got)
	}
}
