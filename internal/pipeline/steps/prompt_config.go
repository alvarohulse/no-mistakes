package steps

import (
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// configuredPromptSection returns the append-only prompt section carrying
// configured prompt additions (global config plus the trusted repo config)
// for one model-invoking step, or "" when nothing is configured.
func configuredPromptSection(sctx *pipeline.StepContext, step types.StepName) string {
	if sctx == nil || sctx.Config == nil {
		return ""
	}
	return sctx.Config.Prompts.SectionForStep(step)
}
