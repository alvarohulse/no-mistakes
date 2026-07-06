package steps

import (
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func configuredPromptSection(sctx *pipeline.StepContext, step types.StepName) string {
	if sctx == nil || sctx.Config == nil {
		return ""
	}
	return sctx.Config.Prompts.SectionForStep(step)
}
