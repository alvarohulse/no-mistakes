package steps

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// LintStep runs linters and asks the agent to fix issues.
type LintStep struct{}

func (s *LintStep) Name() types.StepName { return types.StepLint }

func (s *LintStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, effectiveBaseBranch(sctx))
	configuredCommand := sctx.Config.Commands.LintCommand()
	lintCmd := configuredCommand.Run
	plannedLint := configuredCommand.IsZero()

	if plannedLint {
		if sctx.PlannedCommand == "" {
			sctx.Log("no lint command configured, asking agent to plan one...")
			command, err := planPipelineCommand(sctx, s.Name(), `Return the exact formatter, linter, or static-analysis command that checks the relevant changed files when possible.
- Do not run tests or broader behavioral validation.
- Prefer the repository's canonical lint command when a changed-files-only form is not available.`)
			if err != nil {
				return lintNotEstablishedOutcome(err.Error()), nil
			}
			sctx.PlannedCommand = command
		}
		if sctx.PlannedCommand == "" {
			return lintNotEstablishedOutcome("lint planner found no meaningful lint, format, or static-analysis command"), nil
		}
		lintCmd = sctx.PlannedCommand
	}

	// In fix mode, ask agent to fix lint issues first
	var fixSummary string
	if sctx.Fixing {
		historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx) + configuredPromptSection(sctx, s.Name())
		fixPrompt := fmt.Sprintf(
			`Fix the lint issues in this repository. Run the linter, identify all issues, and fix them.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Rules:
- Make the smallest correct root-cause fix.
- Do not refactor beyond what is needed for that root-cause fix.
- Do not run tests or broader behavioral validation.
- Re-run the relevant lint or format commands before finishing.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			historySection,
		)
		if sctx.PreviousFindings != "" {
			fixPrompt += `

Previous lint findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
		}
		summary, err := executeFixMode(sctx, s.Name(), fixExecutionOptions{
			LogMessage:      "asking agent to fix lint issues...",
			Prompt:          fixPrompt,
			ErrorPrefix:     "agent fix lint",
			FallbackSummary: "fix lint issues",
		})
		if err != nil {
			return nil, err
		}
		fixSummary = summary
	}

	// Run configured lint command
	sctx.Log(fmt.Sprintf("running linter: %s", lintCmd))
	var output string
	var exitCode int
	var err error
	if plannedLint {
		output, exitCode, err = runStepShellCommand(sctx, lintCmd, string(types.StepLint))
	} else {
		output, exitCode, err = runStepRunnerCommand(sctx, configuredCommand, string(types.StepLint))
	}
	if err != nil {
		return nil, fmt.Errorf("run lint command: %w", err)
	}

	projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepLint)

	if exitCode != 0 {
		findings := Findings{
			Items: []Finding{{
				Severity:    "warning",
				Description: fmt.Sprintf("linter found issues (exit code %d)", exitCode),
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

	sctx.Log("lint passed")
	return &pipeline.StepOutcome{FixSummary: fixSummary}, nil
}

func lintNotEstablishedOutcome(description string) *pipeline.StepOutcome {
	description = commandPlanFailureDescription(description)
	findings := Findings{Items: []Finding{{Severity: "warning", Description: description, Action: types.ActionAskUser}}, Summary: description}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: string(findingsJSON)}
}
