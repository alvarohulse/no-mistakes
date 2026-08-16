package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/prbody"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newHookTestContext(t *testing.T, hook string) (*pipeline.StepContext, *[]string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-command fixtures are POSIX")
	}
	workDir := t.TempDir()
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, workDir, "base", "head", config.Commands{})
	sctx.Config.Hooks.PRBody = hook
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	return sctx, &logs
}

func logsContain(logs []string, want string) bool {
	for _, line := range logs {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

func TestApplyPRBodyHookUsesFormatterOutput(t *testing.T) {
	t.Parallel()
	sctx, _ := newHookTestContext(t, "cat > /dev/null; printf '## Templated\\n\\nformatted body\\n'")

	got := applyPRBodyHook(sctx, RunRecords{}, prContent{Title: "fix: x", Body: "built-in"}, "## What Changed\n\n- x", prBodyScope{branch: "feature", baseBranch: "main"})
	if got.Body != "## Templated\n\nformatted body" {
		t.Fatalf("body = %q", got.Body)
	}
	if got.Title != "fix: x" {
		t.Fatalf("title = %q, want the hook to leave it alone", got.Title)
	}
}

func TestApplyPRBodyHookFallsBackLoudlyOnFailure(t *testing.T) {
	t.Parallel()
	sctx, logs := newHookTestContext(t, "cat > /dev/null; echo 'no template' >&2; exit 2")

	got := applyPRBodyHook(sctx, RunRecords{}, prContent{Title: "fix: x", Body: "built-in body"}, "wc", prBodyScope{})
	if got.Body != "built-in body" {
		t.Fatalf("body = %q, want the built-in body", got.Body)
	}
	if !logsContain(*logs, "using the built-in PR body") {
		t.Fatalf("expected a loud fallback in the log, got %v", *logs)
	}
	if !logsContain(*logs, "no template") {
		t.Fatalf("expected the formatter's own diagnostic in the log, got %v", *logs)
	}
}

// A formatter that exits 0 with nothing is the silent-failure case: without
// this the PR ships with an empty body and no warning.
func TestApplyPRBodyHookFallsBackOnEmptyOutput(t *testing.T) {
	t.Parallel()
	sctx, logs := newHookTestContext(t, "cat > /dev/null; exit 0")

	got := applyPRBodyHook(sctx, RunRecords{}, prContent{Body: "built-in body"}, "wc", prBodyScope{})
	if got.Body != "built-in body" {
		t.Fatalf("body = %q, want the built-in body", got.Body)
	}
	if !logsContain(*logs, "wrote no body") {
		t.Fatalf("expected the empty-output reason in the log, got %v", *logs)
	}
}

func TestApplyPRBodyHookNoopWithoutConfiguration(t *testing.T) {
	t.Parallel()
	sctx, logs := newHookTestContext(t, "")

	got := applyPRBodyHook(sctx, RunRecords{}, prContent{Body: "built-in body"}, "wc", prBodyScope{})
	if got.Body != "built-in body" {
		t.Fatalf("body = %q", got.Body)
	}
	if len(*logs) != 0 {
		t.Fatalf("expected silence when no hook is configured, got %v", *logs)
	}
}

func TestApplyPRBodyHookClampsOverLimitOutput(t *testing.T) {
	t.Parallel()
	sctx, logs := newHookTestContext(t, "cat > /dev/null; head -c 5000 /dev/zero | tr '\\0' 'x'")

	got := applyPRBodyHook(sctx, RunRecords{}, prContent{Body: "built-in"}, "wc", prBodyScope{bodyLimit: 4000})
	// Measured the way a host measures it (UTF-16 units), not in bytes.
	if n := scm.PRBodyLen(got.Body); n > 4000 {
		t.Fatalf("body is %d characters, want it clamped to the host limit", n)
	}
	if !logsContain(*logs, "clamping") {
		t.Fatalf("expected the clamp to be reported, got %v", *logs)
	}
}

func TestApplyPRBodyHookPassesContractOnStdin(t *testing.T) {
	t.Parallel()
	dump := filepath.Join(t.TempDir(), "contract.json")
	sctx, _ := newHookTestContext(t, "cat > "+dump+"; echo body")
	sctx.PRNote = "Skipping the dead-letter path deliberately."
	sctx.UserIntent = "Bound the retry window."
	sctx.IntentSource = db.RunIntentSourceAgent
	intentStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepIntent)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(intentStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}

	applyPRBodyHook(sctx, LoadRunRecords(sctx.DB, sctx.Run.ID), prContent{
		Title:   "fix(scheduler): bound the retry window",
		Summary: "Bounds retries through `maxRetryWindow`.",
		Body:    "assembled",
	},
		"- cap retries",
		prBodyScope{branch: "feature", baseBranch: "main", provider: "github", bodyLimit: 0})

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("formatter did not receive a contract: %v", err)
	}
	var contract prbody.Contract
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("contract is not valid JSON: %v", err)
	}
	if contract.Version != prbody.Version {
		t.Errorf("version = %d, want %d", contract.Version, prbody.Version)
	}
	if contract.Title != "fix(scheduler): bound the retry window" {
		t.Errorf("title = %q", contract.Title)
	}
	if !contract.Sections.Notes.Supplied || !contract.Sections.Notes.Trusted {
		t.Errorf("notes = %+v, want a supplied trusted note", contract.Sections.Notes)
	}
	if contract.Sections.Intent != nil {
		t.Errorf("legacy intent section = %+v, want intent only on the pipeline step", contract.Sections.Intent)
	}
	if contract.Sections.Summary == nil || !strings.Contains(contract.Sections.Summary.Text, "`maxRetryWindow`") {
		t.Errorf("summary = %+v", contract.Sections.Summary)
	}
	if contract.Sections.Pipeline == nil || len(contract.Sections.Pipeline.Steps) != 1 {
		t.Fatalf("pipeline = %+v", contract.Sections.Pipeline)
	}
	intentResult := contract.Sections.Pipeline.Steps[0].Intent
	if intentResult == nil || !intentResult.Provided || intentResult.Text != "Bound the retry window." {
		t.Errorf("intent result = %+v", intentResult)
	}
	// The formatter gets the agent's own prose, not the assembled body it
	// would otherwise have to unpick.
	if contract.Sections.WhatChanged == nil || !strings.Contains(contract.Sections.WhatChanged.Text, "cap retries") {
		t.Errorf("what_changed = %+v", contract.Sections.WhatChanged)
	}
}

// The pre-assembly capture happens in buildPRContent, not in applyPRBodyHook,
// so it is only observable from here: a formatter must receive the drafting
// agent's own prose, never the assembled body it ends up inside. Asserting the
// prose is present is not enough - the assembled body contains it too - so this
// asserts the sections assembly adds around it are absent.
func TestBuildPRContentPassesPreAssemblyWhatChangedToTheFormatter(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell-command fixtures are POSIX")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	summary := "Bounds retries through `maxRetryWindow`."
	prose := "- cap scheduler retries at the configured window"
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload, err := json.Marshal(prContent{Title: "fix(scheduler): bound the retry window", Summary: summary, WhatChanged: prose})
			if err != nil {
				return nil, err
			}
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Bound the retry window."
	dump := filepath.Join(t.TempDir(), "contract.json")
	sctx.Config.Hooks.PRBody = "cat > " + dump + "; printf 'formatted body\\n'"

	// A completed step, so assembly has a Pipeline section to add and this test
	// can tell the assembled body from the prose that produced it.
	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}

	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if content.Body != "formatted body" {
		t.Fatalf("body = %q, want the formatter's output", content.Body)
	}

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("formatter did not receive a contract: %v", err)
	}
	var contract prbody.Contract
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("contract is not valid JSON: %v", err)
	}
	if contract.Sections.WhatChanged == nil {
		t.Fatal("what_changed is absent, want the agent's own prose")
	}
	if got := contract.Sections.WhatChanged.Text; got != prose {
		t.Fatalf("what_changed = %q, want exactly the agent's prose %q", got, prose)
	}
	if contract.Sections.Summary == nil || contract.Sections.Summary.Text != summary {
		t.Fatalf("summary = %+v, want exactly the agent's summary %q", contract.Sections.Summary, summary)
	}
	// Assembly's own additions must not have leaked in: a formatter that has to
	// strip a Pipeline section back out has been handed a layout, not material.
	for _, assembled := range []string{"**Review**", "Bound the retry window."} {
		if strings.Contains(contract.Sections.WhatChanged.Text, assembled) {
			t.Fatalf("what_changed carries assembled section %q:\n%s", assembled, contract.Sections.WhatChanged.Text)
		}
	}
	if contract.Sections.Pipeline == nil || len(contract.Sections.Pipeline.Steps) == 0 {
		t.Fatal("pipeline section should still carry the per-step records separately")
	}
}

// A run with no note and no risk assessment still has to say so, because
// "no risk was assessed" and "risk is low" are different facts.
func TestContractStatesAbsentNoteAndRisk(t *testing.T) {
	t.Parallel()
	contract := BuildContract(ContractInput{Branch: "feature"})

	if contract.Sections.Notes.Supplied {
		t.Error("notes.supplied should be false with no author note")
	}
	if !contract.Sections.Notes.Trusted {
		t.Error("notes.trusted describes the channel, not the content; it stays true")
	}
	if contract.Sections.Risk.Reported {
		t.Error("risk.reported should be false with no review assessment")
	}
}

func TestContractCarriesAllThreeRiskFields(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"summary":"","risk_level":"high","risk_rationale":"Touches the auth path.","risk_scope":"source-or-external"}`
	contract := BuildContract(ContractInput{
		Steps: []*db.StepResult{{
			ID: "s1", StepName: types.StepReview, StepOrder: 3,
			Status: types.StepStatusCompleted, FindingsJSON: &findings,
		}},
	})

	risk := contract.Sections.Risk
	if !risk.Reported || risk.Level != "high" || risk.Rationale != "Touches the auth path." || risk.Scope != "source-or-external" {
		t.Fatalf("risk = %+v, want all three fields", risk)
	}
}

func TestContractPipelineIsPerStepData(t *testing.T) {
	t.Parallel()
	provider := "anthropic"
	exit := 0
	duration := int64(1234)
	findings := `{"findings":[{"severity":"P1","description":"d","action":"auto-fix"},{"severity":"P2","description":"e","action":"no-op"}],"summary":""}`

	contract := BuildContract(ContractInput{
		Run: &db.Run{ID: "run-1", RefreshStrategy: types.RefreshStrategyMerge},
		Steps: []*db.StepResult{{
			ID: "s1", StepName: types.StepRefresh, StepOrder: 2,
			Status: types.StepStatusCompleted, ExitCode: &exit, DurationMS: &duration,
			FindingsJSON: &findings,
		}},
		Rounds: map[string][]*db.StepRound{"s1": {{ID: "r1"}, {ID: "r2"}}},
		Invocations: []db.AgentInvocation{{
			StepName: string(types.StepRefresh), Round: 1, Purpose: "refresh",
			Agent: "claude", Model: "claude-opus-5", ModelProvider: &provider,
			InvocationMode:            types.AgentInvocationModeHarnessCLI,
			AgentObservationsReported: true,
			AgentObservations: []types.AgentObservation{
				{Identity: "Explore", InvocationMode: types.AgentInvocationModeSubagentTool},
			},
		}},
	})

	if contract.Sections.Pipeline == nil || len(contract.Sections.Pipeline.Steps) != 1 {
		t.Fatalf("pipeline = %+v", contract.Sections.Pipeline)
	}
	step := contract.Sections.Pipeline.Steps[0]
	if step.Name != "refresh" {
		t.Errorf("name = %q", step.Name)
	}
	// The step ID is stable; only the label follows the run's strategy.
	if step.Label != "Merge" {
		t.Errorf("label = %q, want the merge-strategy label", step.Label)
	}
	if step.Rounds != 2 {
		t.Errorf("rounds = %d, want 2", step.Rounds)
	}
	if step.Findings.Total != 2 || step.Findings.BySeverity["P1"] != 1 || step.Findings.BySeverity["P2"] != 1 {
		t.Errorf("findings = %+v", step.Findings)
	}
	if len(step.Agents) != 1 {
		t.Fatalf("agents = %+v", step.Agents)
	}
	agent := step.Agents[0]
	if agent.Model != "claude-opus-5" || agent.Vendor != "anthropic" || agent.InvocationMode != "harness_cli" {
		t.Errorf("agent telemetry = %+v", agent)
	}
	if !agent.NestedReported || len(agent.Nested) != 1 || agent.Nested[0].Identity != "Explore" {
		t.Errorf("nested agents = %+v", agent.Nested)
	}
}

// The built-in body omits the pr and ci rows because printing them inside the PR
// body being written reads badly. That is a layout call, and the contract exists
// to delegate layout - so it carries the rows, and the sample that advertises
// them (including its only failed and in-flight steps) stays honest.
func TestContractPipelineCarriesPRAndCISteps(t *testing.T) {
	t.Parallel()
	exit := 0
	failExit := 1
	contract := BuildContract(ContractInput{
		Run: &db.Run{ID: "run-1"},
		Steps: []*db.StepResult{
			{ID: "s1", StepName: types.StepPush, StepOrder: 8, Status: types.StepStatusCompleted, ExitCode: &exit},
			{ID: "s2", StepName: types.StepPR, StepOrder: 9, Status: types.StepStatusRunning},
			{ID: "s3", StepName: types.StepCI, StepOrder: 10, Status: types.StepStatusFailed, ExitCode: &failExit},
		},
	})

	if contract.Sections.Pipeline == nil {
		t.Fatal("pipeline section is absent")
	}
	byName := map[string]prbody.PipelineStep{}
	for _, step := range contract.Sections.Pipeline.Steps {
		byName[step.Name] = step
	}
	pr, ok := byName["pr"]
	if !ok {
		t.Fatalf("pr step is missing from %+v", contract.Sections.Pipeline.Steps)
	}
	if pr.Status != "running" {
		t.Errorf("pr status = %q, want running", pr.Status)
	}
	ci, ok := byName["ci"]
	if !ok {
		t.Fatalf("ci step is missing from %+v", contract.Sections.Pipeline.Steps)
	}
	if ci.Status != "failed" {
		t.Errorf("ci status = %q, want failed", ci.Status)
	}
}

func TestContractPipelineCarriesConfiguredSkipSource(t *testing.T) {
	t.Parallel()
	source := string(types.SkipSourceGlobalOverride)
	contract := BuildContract(ContractInput{
		Run: &db.Run{ID: "run-1"},
		Steps: []*db.StepResult{{
			ID: "s1", StepName: types.StepCI, StepOrder: 10,
			Status: types.StepStatusSkipped, SkipSource: &source,
		}},
	})
	if contract.Sections.Pipeline == nil || len(contract.Sections.Pipeline.Steps) != 1 {
		t.Fatalf("pipeline section = %+v", contract.Sections.Pipeline)
	}
	ci := contract.Sections.Pipeline.Steps[0]
	if ci.Status != "skipped" || ci.SkipSource == nil || *ci.SkipSource != source {
		t.Fatalf("configured CI skip = %+v", ci)
	}
}

// The built-in Pipeline markdown keeps its own omission: this pins the two
// renderings apart so a future edit cannot collapse them back together.
func TestBuiltInPipelineSectionStillOmitsPRAndCI(t *testing.T) {
	t.Parallel()
	exit := 0
	failExit := 1
	records := RunRecords{Steps: []*db.StepResult{
		{ID: "s1", StepName: types.StepPush, StepOrder: 8, Status: types.StepStatusCompleted, ExitCode: &exit},
		{ID: "s2", StepName: types.StepPR, StepOrder: 9, Status: types.StepStatusRunning},
		{ID: "s3", StepName: types.StepCI, StepOrder: 10, Status: types.StepStatusFailed, ExitCode: &failExit},
	}}

	got, _ := buildPipelineSummary(records.Steps, records.Rounds, records.Invocations, types.RefreshStrategyRebase, "")
	if strings.Contains(got, "**PR**") || strings.Contains(got, "**CI**") {
		t.Fatalf("built-in pipeline section should omit the pr and ci rows, got:\n%s", got)
	}
}
