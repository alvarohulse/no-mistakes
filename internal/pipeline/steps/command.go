package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// SetupStep runs an optional trusted setup/bootstrap command before validation.
type SetupStep struct{}

func (s *SetupStep) Name() types.StepName { return types.StepSetup }

func (s *SetupStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if sctx.Config == nil {
		return skipCommandStep(sctx, s.Name()), nil
	}
	return executeCommandStep(sctx, s.Name(), sctx.Config.Commands.Setup)
}

// BuildStep runs an optional trusted build command before review/test work.
type BuildStep struct{}

func (s *BuildStep) Name() types.StepName { return types.StepBuild }

func (s *BuildStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if sctx.Config == nil {
		return skipCommandStep(sctx, s.Name()), nil
	}
	return executeCommandStep(sctx, s.Name(), sctx.Config.Commands.Build)
}

func executeCommandStep(sctx *pipeline.StepContext, stepName types.StepName, command string) (*pipeline.StepOutcome, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return skipCommandStep(sctx, stepName), nil
	}

	sctx.Log(fmt.Sprintf("running %s command: %s", stepName, command))
	output, exitCode, err := runStepShellCommand(sctx, command)
	if err != nil {
		return nil, fmt.Errorf("run %s command: %w", stepName, err)
	}
	if output != "" {
		sctx.Log(output)
	}
	if exitCode != 0 {
		return &pipeline.StepOutcome{ExitCode: exitCode}, fmt.Errorf("%s command failed with exit code %d", stepName, exitCode)
	}

	sctx.Log(fmt.Sprintf("%s command passed", stepName))
	return &pipeline.StepOutcome{ExitCode: 0}, nil
}

func skipCommandStep(sctx *pipeline.StepContext, stepName types.StepName) *pipeline.StepOutcome {
	if sctx.Log != nil {
		sctx.Log(fmt.Sprintf("no %s command configured, skipping", stepName))
	}
	return &pipeline.StepOutcome{Skipped: true}
}
