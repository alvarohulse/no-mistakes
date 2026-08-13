package steps

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// BuildStep verifies that the change compiles before the test step exercises it.
type BuildStep struct{}

func (s *BuildStep) Name() types.StepName { return types.StepBuild }

func (s *BuildStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	fixSummary := ""
	if sctx.Fixing {
		var err error
		fixSummary, err = s.executeFix(sctx)
		if err != nil {
			return nil, err
		}
	}

	buildCommand := sctx.Config.Commands.Build
	if buildCommand == "" {
		outcome, err := s.executeAgentBuild(sctx)
		if outcome != nil {
			outcome.FixSummary = fixSummary
		}
		return outcome, err
	}

	sctx.Log(fmt.Sprintf("running build: %s", buildCommand))
	output, exitCode, err := runStepShellCommand(sctx, buildCommand)
	if err != nil {
		return nil, fmt.Errorf("run build command: %w", err)
	}
	projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepBuild)
	if exitCode != 0 {
		findings := Findings{
			Items: []Finding{{
				Severity:    "error",
				Description: fmt.Sprintf("build failed with exit code %d", exitCode),
				Action:      types.ActionAutoFix,
			}},
			Summary: projectedOutput,
		}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   true,
			Findings:      string(findingsJSON),
			ExitCode:      exitCode,
			FixSummary:    fixSummary,
		}, nil
	}
	return &pipeline.StepOutcome{ExitCode: exitCode, FixSummary: fixSummary}, nil
}

func (s *BuildStep) executeFix(sctx *pipeline.StepContext) (string, error) {
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, effectiveBaseBranch(sctx))
	historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx) + configuredPromptSection(sctx, s.Name())
	configuredCommand := sctx.Config.Commands.Build
	if configuredCommand == "" {
		configuredCommand = sctx.PlannedCommand
		if configuredCommand == "" {
			configuredCommand = "not configured; the pipeline will reuse its read-only planned command after repair"
		}
	}
	prompt := fmt.Sprintf(`Fix the unresolved build or compile failure in this repository.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- build command: %s

Rules:
- Make the smallest build root-cause fix.
- Do not refactor beyond what is needed for that root-cause fix.
- Do NOT run tests.
- Do NOT run linters, formatters, or static analysis tools unless they are inseparable from the build command needed to reproduce the failure.
- Do NOT update documentation.
- Reproduce only the reported build or compile failure, apply the fix, and re-run only the focused build command needed to establish it is resolved.
- Before finishing, remove transient build outputs or caches created in the working tree so they are not committed and pushed.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		configuredCommand,
		historySection,
	)
	if sctx.PreviousFindings != "" {
		prompt += "\n\nPrevious build findings to address:\n" + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}
	return executeFixMode(sctx, s.Name(), fixExecutionOptions{
		LogMessage:      "asking agent to fix build failure...",
		Prompt:          prompt,
		ErrorPrefix:     "agent fix build",
		FallbackSummary: "fix build failure",
	})
}

func (s *BuildStep) executeAgentBuild(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if sctx.PlannedCommand == "" {
		sctx.Log("no build command configured, asking agent to plan one...")
		command, err := planPipelineCommand(sctx, s.Name(), `Examine the repository and return the exact build or compile command that exercises the smallest surface establishing the changed production code compiles.
- Do NOT run tests.
- Do NOT run linters, formatters, or static analysis unless inseparable from the canonical build.
- Do NOT update documentation.`)
		if err != nil {
			return buildNotEstablishedOutcome(err.Error()), nil
		}
		sctx.PlannedCommand = command
	}
	if sctx.PlannedCommand == "" {
		return buildNotEstablishedOutcome("build planner found no meaningful build or compile command"), nil
	}
	sctx.Log(fmt.Sprintf("running planned build: %s", sctx.PlannedCommand))
	output, exitCode, err := runStepShellCommand(sctx, sctx.PlannedCommand)
	if err != nil {
		return nil, fmt.Errorf("run planned build command: %w", err)
	}
	projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepBuild)
	findings := Findings{Tested: []string{sctx.PlannedCommand}}
	if exitCode != 0 {
		findings.Items = []Finding{{Severity: "error", Description: fmt.Sprintf("build failed with exit code %d", exitCode), Action: types.ActionAutoFix}}
		findings.Summary = projectedOutput
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{NeedsApproval: exitCode != 0, AutoFixable: exitCode != 0, Findings: string(findingsJSON), ExitCode: exitCode}, nil
}

func buildNotEstablishedOutcome(description string) *pipeline.StepOutcome {
	findings := Findings{
		Items: []Finding{{
			Severity:    "error",
			Description: description,
			Action:      types.ActionAskUser,
		}},
		Summary: description,
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: string(findingsJSON)}
}
