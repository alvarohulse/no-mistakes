package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const maxAgentBuildCommandBytes = 4096

// BuildStep verifies that the change compiles before review and testing.
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

	source := "configured build command"
	command := strings.TrimSpace(sctx.Config.Commands.Build)
	before, err := snapshotCleanBuildWorktree(sctx, source)
	if err != nil {
		return nil, err
	}
	if command == "" {
		source = "agent-selected build command"
		var outcome *pipeline.StepOutcome
		command, outcome, err = s.selectBuildCommand(sctx)
		if unchangedErr := assertBuildWorktreeUnchanged(sctx, "build command selection", before); unchangedErr != nil {
			return nil, unchangedErr
		}
		if err != nil {
			return nil, err
		}
		if outcome != nil {
			outcome.FixSummary = fixSummary
			return outcome, nil
		}
	}

	outcome, err := runBuildCommand(sctx, command, source, before, runStepShellCommand)
	if outcome != nil {
		outcome.FixSummary = fixSummary
	}
	return outcome, err
}

type buildWorktreeSnapshot struct {
	head   string
	status string
}

func snapshotCleanBuildWorktree(sctx *pipeline.StepContext, source string) (buildWorktreeSnapshot, error) {
	snapshot, err := snapshotBuildWorktree(sctx)
	if err != nil {
		return buildWorktreeSnapshot{}, fmt.Errorf("snapshot worktree before %s: %w", source, err)
	}
	if snapshot.status != "" {
		return buildWorktreeSnapshot{}, fmt.Errorf("Build requires a clean worktree before %s; found:\n%s", source, printableBuildStatus(snapshot.status))
	}
	return snapshot, nil
}

func snapshotBuildWorktree(sctx *pipeline.StepContext) (buildWorktreeSnapshot, error) {
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return buildWorktreeSnapshot{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	status, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return buildWorktreeSnapshot{}, fmt.Errorf("read worktree status: %w", err)
	}
	return buildWorktreeSnapshot{head: head, status: string(status)}, nil
}

func assertBuildWorktreeUnchanged(sctx *pipeline.StepContext, source string, before buildWorktreeSnapshot) error {
	after, err := snapshotBuildWorktree(sctx)
	if err != nil {
		return fmt.Errorf("snapshot worktree after %s: %w", source, err)
	}
	if after == before {
		return nil
	}
	return fmt.Errorf("Build verification must be side-effect free; worktree changed after %s (before HEAD %s, after HEAD %s; status after:\n%s)",
		source, before.head, after.head, printableBuildStatus(after.status))
}

func printableBuildStatus(status string) string {
	if status == "" {
		return "(clean)"
	}
	const limit = 4096
	status = strings.ReplaceAll(status, "\x00", "\n")
	if len(status) > limit {
		return status[:limit] + "\n[status truncated]"
	}
	return status
}

type buildCommandRunner func(*pipeline.StepContext, string) (string, int, error)

func runBuildCommand(sctx *pipeline.StepContext, command, source string, before buildWorktreeSnapshot, runner buildCommandRunner) (*pipeline.StepOutcome, error) {
	sctx.Log(fmt.Sprintf("running build: %s", command))
	output, exitCode, err := runner(sctx, command)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", source, err)
	}
	projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepBuild)
	if sideEffectErr := assertBuildWorktreeUnchanged(sctx, source, before); sideEffectErr != nil {
		if exitCode != 0 {
			return nil, fmt.Errorf("build failed with exit code %d: %s: %w", exitCode, projectedOutput, sideEffectErr)
		}
		return nil, sideEffectErr
	}
	if exitCode == 0 {
		return &pipeline.StepOutcome{}, nil
	}

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
	}, nil
}

type buildPlan struct {
	Items        []Finding `json:"findings"`
	Summary      string    `json:"summary"`
	BuildCommand string    `json:"build_command"`
}

func (s *BuildStep) selectBuildCommand(sctx *pipeline.StepContext) (string, *pipeline.StepOutcome, error) {
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
	sctx.Log("no build command configured, asking agent to select one...")
	result, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
		Prompt: fmt.Sprintf(`Select one focused command that establishes this repository's changed production code builds or compiles.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Rules:
- Inspect repository build metadata and the changed production files.
- For a Go module, return "go build ./..." in "build_command" so every package is covered.
- Do NOT execute the command. The pipeline executes it and records the actual result.
- Automatic execution accepts only "go build" with compile-safe flags followed by the full-module target "./...". Other build systems require a trusted commands.build configuration; when one is required, leave "build_command" empty and explain that finding.
- Do NOT run tests, linters, formatters, or static analysis unless inseparable from the canonical build command.
- Do NOT modify any files or Git state.
- Return an empty findings array when a meaningful command exists.
- If no meaningful build or compile command exists, leave "build_command" empty and return one ask-user finding explaining what you inspected.
- Never report a successful build; command execution happens after this turn.%s`,
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
		return "", nil, fmt.Errorf("agent select build command: %w", err)
	}
	if result.Output == nil {
		return "", buildNotEstablishedOutcome("build agent returned no structured result"), nil
	}

	var plan buildPlan
	if err := json.Unmarshal(result.Output, &plan); err != nil {
		return "", buildNotEstablishedOutcome("build agent returned an invalid structured result"), nil
	}
	command := strings.TrimSpace(plan.BuildCommand)
	if command == "" {
		return "", buildNotEstablishedOutcome("build agent did not identify a build or compile command", plan), nil
	}
	argv, err := parseAgentBuildCommand(command)
	if err != nil {
		return "", buildNotEstablishedOutcome(err.Error(), plan), nil
	}
	if covered, reason, err := automaticGoBuildCoversWorktree(sctx.Ctx, sctx.WorkDir); err != nil {
		return "", nil, fmt.Errorf("inspect Go module layout: %w", err)
	} else if !covered {
		return "", buildNotEstablishedOutcome(
			fmt.Sprintf("automatic \"go build ./...\" cannot cover the changed code because %s; configure a trusted commands.build for this build system", reason),
			plan,
		), nil
	}
	// Execute only the exact token sequence accepted above, not the raw agent text.
	command = strings.Join(argv, " ")
	sctx.Log(fmt.Sprintf("agent selected build command: %s", command))
	return command, nil, nil
}

var agentGoBuildFlags = map[string]struct{}{
	"-buildvcs=false": {},
	"-mod=readonly":   {},
	"-mod=vendor":     {},
	"-trimpath":       {},
	"-v":              {},
}

func parseAgentBuildCommand(command string) ([]string, error) {
	if len(command) > maxAgentBuildCommandBytes {
		return nil, fmt.Errorf("agent-selected build command exceeds %d bytes", maxAgentBuildCommandBytes)
	}
	if strings.ContainsAny(command, "\x00\r\n") {
		return nil, fmt.Errorf("agent-selected build command must be a single line")
	}
	if strings.ContainsAny(command, "'\"") {
		return nil, fmt.Errorf("agent-selected build command must use unquoted arguments")
	}
	argv := strings.Fields(command)
	if len(argv) == 0 {
		return nil, fmt.Errorf("agent-selected build command is empty")
	}
	name := strings.ToLower(argv[0])
	if filepath.Base(name) != name {
		return nil, fmt.Errorf("agent-selected build executable must be resolved from PATH")
	}
	name = strings.TrimSuffix(name, ".exe")
	if name != "go" || len(argv) < 2 || argv[1] != "build" {
		return nil, fmt.Errorf("agent-selected command must use the restricted automatic form: go build [safe flags] ./...")
	}
	seenTarget := false
	for _, arg := range argv[2:] {
		if !seenTarget {
			if _, ok := agentGoBuildFlags[arg]; ok {
				continue
			}
		}
		if arg == "./..." && !seenTarget {
			seenTarget = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return nil, fmt.Errorf("agent-selected go build flag %q is not compile-safe", arg)
		}
		return nil, fmt.Errorf("agent-selected go build must cover the full module with ./...; got target %q", arg)
	}
	if !seenTarget {
		return nil, fmt.Errorf("agent-selected go build must cover the full module with ./...")
	}
	return argv, nil
}

// automaticGoBuildCoversWorktree reports whether root-level "go build ./..."
// covers every module in the worktree. A repository without a root Go module,
// workspaces, and nested modules all require a trusted commands.build because
// the restricted automatic form cannot name all of their build targets safely.
func automaticGoBuildCoversWorktree(ctx context.Context, workDir string) (bool, string, error) {
	if _, err := os.Stat(filepath.Join(workDir, "go.work")); err == nil {
		return false, "a Go workspace file (go.work) is present", nil
	} else if !os.IsNotExist(err) {
		return false, "", fmt.Errorf("probe go.work: %w", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "go.mod")); os.IsNotExist(err) {
		return false, "no root Go module (go.mod) is present", nil
	} else if err != nil {
		return false, "", fmt.Errorf("probe go.mod: %w", err)
	}
	tracked, err := git.Run(ctx, workDir, "ls-files", "--", "*go.mod")
	if err != nil {
		return false, "", fmt.Errorf("list tracked go.mod files: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(tracked), "\n") {
		path := strings.TrimSpace(line)
		if strings.HasSuffix(path, "/go.mod") {
			return false, fmt.Sprintf("a nested Go module (%s) is present", path), nil
		}
	}
	return true, "", nil
}

func buildNotEstablishedOutcome(description string, reports ...buildPlan) *pipeline.StepOutcome {
	findings := Findings{
		Items: []Finding{{
			Severity:    "error",
			Description: description,
			Action:      types.ActionAskUser,
		}},
		Summary: description,
	}
	autoFixable := false
	if len(reports) > 0 {
		report := reports[0]
		if summary := strings.TrimSpace(report.Summary); summary != "" {
			findings.Summary += "; agent report: " + summary
		}
		for _, item := range report.Items {
			if item.Action == types.ActionAutoFix {
				autoFixable = true
			}
			findings.Items = append(findings.Items, item)
		}
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{NeedsApproval: true, AutoFixable: autoFixable, Findings: string(findingsJSON)}
}

func (s *BuildStep) executeFix(sctx *pipeline.StepContext) (string, error) {
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
	configuredCommand := strings.TrimSpace(sctx.Config.Commands.Build)
	if configuredCommand == "" {
		configuredCommand = "not configured; the Build step will select a focused command after the repair"
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
- Do NOT run build or compile commands in this fix turn. The Build step reruns verification after committing the repair.
- Do NOT run tests.
- Do NOT run linters, formatters, or static analysis tools.
- Do NOT update documentation.
- Remove transient build outputs or caches before finishing.
- Return JSON with one concise "summary" field suitable for a git commit subject.
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
