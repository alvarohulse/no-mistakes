package steps

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/agent"
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
	historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
	configuredCommand := sctx.Config.Commands.Build
	if configuredCommand == "" {
		configuredCommand = "not configured; inspect the repository for its build or compile command"
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
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, effectiveBaseBranch(sctx))
	historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
	sctx.Log("no build command configured, asking agent to compile the change...")
	result, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
		Prompt: fmt.Sprintf(`Verify that this repository builds or compiles successfully.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Task:
- Examine the repository, detect and run the appropriate build or compile command.
- Exercise the smallest build surface that establishes the changed production code compiles.
- Report only unresolved build or compile failures. Do not report successful builds or other non-actionable information.

Rules:
- Do NOT run tests.
- Do NOT run linters, formatters, or static analysis tools unless they are an inseparable part of the repository's canonical build command.
- Do NOT update documentation.
- Do NOT modify source files during this verification pass.
- If no meaningful build or compile command exists, return an ask-user finding that explains what you inspected and why the Build gate cannot be established.
- Return structured findings with severity, description, and action, plus a non-empty "tested" array containing every build or compile command you ran.
- Set action to "auto-fix" for objective build failures that can be safely fixed, "ask-user" when the build cannot be established, and "no-op" only for informational context attached to an actionable result.%s`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			historySection,
		),
		CWD:        sctx.WorkDir,
		JSONSchema: buildFindingsSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return nil, fmt.Errorf("agent run build: %w", err)
	}

	var findings Findings
	if result.Output == nil {
		return buildNotEstablishedOutcome("build agent returned no structured result"), nil
	}
	if err := json.Unmarshal(result.Output, &findings); err != nil {
		return buildNotEstablishedOutcome("build agent returned an invalid structured result"), nil
	}
	if len(findings.Tested) == 0 {
		return buildNotEstablishedOutcome("build agent did not report any build or compile command"), nil
	}
	findingsJSON, _ := json.Marshal(findings)
	needsApproval := hasBlockingFindings(findings.Items)
	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   needsApproval,
		Findings:      string(findingsJSON),
	}, nil
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
