package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestEvalReplayConfigRoundTripsOnlyRequiredReviewInputs(t *testing.T) {
	cfg := &Config{
		AgentPathOverride:       map[string]string{"claude": "/private/machine-agent"},
		AgentArgsOverride:       map[string][]string{"claude": {"--token", "machine-secret"}},
		Commands:                Commands{Test: "echo shell-secret"},
		Hooks:                   Hooks{PostWorktree: "echo hook-secret"},
		IgnorePatterns:          []string{"vendor/**"},
		ProcessTerminationGrace: 1750 * time.Millisecond,
		DisableProjectSettings:  true,
		Prompts: PromptConfig{
			Shared: "shared review guidance",
			Review: "review-only guidance",
			Test:   "test-only-secret",
		},
		Review: Review{PathInstructions: []PathInstruction{{Path: "internal/**", Instructions: "inspect error paths"}}},
		Test:   Test{Evidence: Evidence{LocalRoot: "/private/evidence"}},
	}

	encoded, err := MarshalEvalReplayConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"/private/machine-agent", "machine-secret", "shell-secret", "hook-secret", "test-only-secret", "/private/evidence"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("eval replay config leaked %q:\n%s", forbidden, encoded)
		}
	}
	for _, required := range []string{"shared review guidance", "review-only guidance", "internal/**", "inspect error paths"} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("eval replay config omitted %q:\n%s", required, encoded)
		}
	}
	if strings.Contains(string(encoded), `"Path"`) || !strings.Contains(string(encoded), `"path":"internal/**"`) {
		t.Fatalf("eval replay path instructions are not canonical snake-case JSON:\n%s", encoded)
	}

	replayed, err := UnmarshalEvalReplayConfig(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed.IgnorePatterns, cfg.IgnorePatterns) || replayed.ProcessTerminationGrace != cfg.ProcessTerminationGrace || !replayed.DisableProjectSettings {
		t.Fatalf("replayed scalar config = %#v", replayed)
	}
	if replayed.Prompts.Shared != cfg.Prompts.Shared || replayed.Prompts.Review != cfg.Prompts.Review || replayed.Prompts.Test != "" {
		t.Fatalf("replayed prompts = %#v", replayed.Prompts)
	}
	if !reflect.DeepEqual(replayed.Review.PathInstructions, cfg.Review.PathInstructions) {
		t.Fatalf("replayed path instructions = %#v", replayed.Review.PathInstructions)
	}
	if replayed.AgentPathOverride != nil || replayed.AgentArgsOverride != nil || replayed.Commands != (Commands{}) || replayed.Hooks != (Hooks{}) || replayed.Test.Evidence.LocalRoot != "" {
		t.Fatalf("replayed config retained non-review inputs: %#v", replayed)
	}
}

func TestUnmarshalEvalReplayConfigRejectsInvalidContract(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{name: "unsupported version", json: `{"version":2}`, want: "version"},
		{name: "unknown field", json: `{"version":1,"command":"secret"}`, want: "unknown field"},
		{name: "negative grace", json: `{"version":1,"process_termination_grace_ns":-1}`, want: "termination grace"},
		{name: "invalid review instructions", json: `{"version":1,"review_path_instructions":[{"path":"","instructions":"review"}]}`, want: "path must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := UnmarshalEvalReplayConfig([]byte(tt.json))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("UnmarshalEvalReplayConfig() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestEvalReplayConfigCarriesNoAgentRoute(t *testing.T) {
	cfg := &Config{
		Agent:      types.AgentClaude,
		Agents:     []types.AgentName{types.AgentClaude, types.AgentCodex},
		StepAgents: map[types.StepName][]types.AgentName{types.StepReview: {types.AgentCodex}},
		StepModels: map[types.StepName]ModelRoute{types.StepReview: {Name: "gpt-5.6-sol", Vendor: "openai"}},
	}
	encoded, err := MarshalEvalReplayConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "claude") || strings.Contains(string(encoded), "codex") || strings.Contains(string(encoded), "gpt-5.6-sol") {
		t.Fatalf("candidate-independent replay config contains a launch route:\n%s", encoded)
	}
}
