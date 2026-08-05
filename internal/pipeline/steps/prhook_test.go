package steps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
	sctx := newTestContext(t, &mockAgent{name: "test"}, t.TempDir(), "base", "head", config.Commands{})
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

	got := applyPRBodyHook(sctx, prContent{Title: "fix: x", Body: "built-in"}, "## What Changed\n\n- x", prBodyScope{branch: "feature", baseBranch: "main"})
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

	got := applyPRBodyHook(sctx, prContent{Title: "fix: x", Body: "built-in body"}, "wc", prBodyScope{})
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

	got := applyPRBodyHook(sctx, prContent{Body: "built-in body"}, "wc", prBodyScope{})
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

	got := applyPRBodyHook(sctx, prContent{Body: "built-in body"}, "wc", prBodyScope{})
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

	got := applyPRBodyHook(sctx, prContent{Body: "built-in"}, "wc", prBodyScope{bodyLimit: 4000})
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

	applyPRBodyHook(sctx,
		prContent{Title: "fix(scheduler): bound the retry window", Body: "assembled"},
		"## What Changed\n\n- cap retries",
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
	if contract.Sections.Intent == nil || !contract.Sections.Intent.Authoritative {
		t.Errorf("intent = %+v, want an authoritative intent", contract.Sections.Intent)
	}
	// The formatter gets the agent's own prose, not the assembled body it
	// would otherwise have to unpick.
	if contract.Sections.WhatChanged == nil || !strings.Contains(contract.Sections.WhatChanged.Text, "cap retries") {
		t.Errorf("what_changed = %+v", contract.Sections.WhatChanged)
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
