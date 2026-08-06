package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestConfiguredPromptSection(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{
		Config: &config.Config{
			Prompts: config.PromptConfig{
				Shared: "shared guidance",
				Review: "review guidance",
			},
		},
	}

	got := configuredPromptSection(sctx, types.StepReview)
	for _, want := range []string{
		"Additional prompt config:",
		"built-in instructions above",
		"shared guidance",
		"review guidance",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt section missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "shared guidance") > strings.Index(got, "review guidance") {
		t.Fatalf("shared guidance should appear before review guidance:\n%s", got)
	}
	if !strings.HasPrefix(got, "\n\n") {
		t.Fatalf("prompt section should be appendable after built-in prompt text:\n%q", got)
	}
}

func TestConfiguredPromptSectionCoversEveryAgentInvokingStep(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{
		Config: &config.Config{
			Prompts: config.PromptConfig{
				Intent:   "intent guidance",
				Refresh:  "refresh guidance",
				Review:   "review guidance",
				Build:    "build guidance",
				Test:     "test guidance",
				Document: "document guidance",
				Lint:     "lint guidance",
				PR:       "pr guidance",
				CI:       "ci guidance",
			},
		},
	}

	wantByStep := map[types.StepName]string{
		types.StepIntent:   "intent guidance",
		types.StepRefresh:  "refresh guidance",
		types.StepReview:   "review guidance",
		types.StepBuild:    "build guidance",
		types.StepTest:     "test guidance",
		types.StepDocument: "document guidance",
		types.StepLint:     "lint guidance",
		types.StepPR:       "pr guidance",
		types.StepCI:       "ci guidance",
	}
	for step, want := range wantByStep {
		if got := configuredPromptSection(sctx, step); !strings.Contains(got, want) {
			t.Errorf("step %s: section = %q, want it to contain %q", step, got, want)
		}
	}
	// Push never prompts an agent, so step-specific guidance has no push key
	// and only shared guidance could ever apply to it.
	if got := configuredPromptSection(sctx, types.StepPush); got != "" {
		t.Errorf("push section = %q, want empty without shared guidance", got)
	}
}

func TestConfiguredPromptSectionEmpty(t *testing.T) {
	t.Parallel()
	if got := configuredPromptSection(nil, types.StepReview); got != "" {
		t.Fatalf("nil context section = %q, want empty", got)
	}
	sctx := &pipeline.StepContext{Config: &config.Config{}}
	if got := configuredPromptSection(sctx, types.StepReview); got != "" {
		t.Fatalf("empty prompt section = %q, want empty", got)
	}
}
