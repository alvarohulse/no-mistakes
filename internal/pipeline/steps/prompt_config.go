package steps

import (
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// configuredPromptSection returns the append-only prompt section carrying
// configured prompt additions (global config plus the trusted repo config)
// for the model-invoking steps this invocation owns, or "" when nothing is
// configured. Pass more than one step when a single agent pass carries several
// steps' duties (the combined document+lint housekeeping pass); shared
// guidance is still emitted only once.
func configuredPromptSection(sctx *pipeline.StepContext, steps ...types.StepName) string {
	if sctx == nil || sctx.Config == nil {
		return ""
	}
	return sctx.Config.Prompts.SectionForSteps(steps...)
}
