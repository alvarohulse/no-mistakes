package steps

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestConfiguredPromptSection(t *testing.T) {
	sctx := &pipeline.StepContext{
		Ctx: context.Background(),
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
}

func TestConfiguredPromptSectionEmpty(t *testing.T) {
	if got := configuredPromptSection(nil, types.StepReview); got != "" {
		t.Fatalf("nil context section = %q, want empty", got)
	}
	sctx := &pipeline.StepContext{Config: &config.Config{}}
	if got := configuredPromptSection(sctx, types.StepReview); got != "" {
		t.Fatalf("empty prompt section = %q, want empty", got)
	}
}
