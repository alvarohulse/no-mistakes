package stats

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/legacycost"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTextJSONAndCSVProjectTheSameSelectedFacts(t *testing.T) {
	database, run := newAuditRun(t)
	report, err := BuildReport(database, Query{RunID: run.ID}, time.Unix(run.CreatedAt+1, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	jsonOutput, err := report.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	textOutput := RenderText(report)
	csvOutput, err := RenderCSV(report)
	if err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{"json": jsonOutput, "text": textOutput, "csv": csvOutput} {
		if !strings.Contains(output, run.ID) || !strings.Contains(output, string(types.RunCompleted)) {
			t.Fatalf("%s projection omitted selected run facts:\n%s", name, output)
		}
	}
	rows, err := csv.NewReader(strings.NewReader(csvOutput)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := []string{"schema_version", "repo_id", "run_id", "record_type", "entity_id", "section", "group", "metric", "value", "unit", "reported", "eligible", "complete", "basis", "reason", "json_path"}
	if strings.Join(rows[0], ",") != strings.Join(wantHeader, ",") {
		t.Fatalf("CSV header = %v", rows[0])
	}
	assertCSVLeafParity(t, jsonOutput, rows)
}

func TestDetailedTextRendersPerPurposeAggregateTablesBeforeInvocationDetail(t *testing.T) {
	input, cacheWrite, roundtrips := int64(150), int64(45), int64(2)
	report := &Report{
		Runs: ReportRuns{ByStatus: map[types.RunStatus]int{}, Items: []RunIdentity{}},
		AgentAggregates: []AgentAggregate{{
			Purpose: "review", Count: 2, TotalDurationMS: 90_000, AvgDurationMS: 45_000,
			Started: 1, Resumed: 1, InputTokens: &input, CacheWriteTokens: &cacheWrite,
			ModelRoundtrips: &roundtrips, MetricsRows: 2,
		}},
		Agents: []ReportAgent{{RunID: "run-1", Invocation: Invocation{ID: "inv-1", Step: types.StepReview, Purpose: "review", Agent: "codex", ExitStatus: "ok"}}},
	}

	output := RenderDetailedText(report)
	for _, want := range []string{"PURPOSE", "COUNT", "CACHE WRITE TOK", "review", "45", "METRICS", "2/2", "ROUNDTRIPS", "agent run-1/inv-1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("detailed text omitted %q:\n%s", want, output)
		}
	}
	if strings.Index(output, "PURPOSE") > strings.Index(output, "agent run-1/inv-1") {
		t.Fatalf("aggregate tables should precede invocation detail:\n%s", output)
	}
}

func TestDetailedTextKeepsLegacyEmptyAgentMessage(t *testing.T) {
	output := RenderDetailedText(&Report{Runs: ReportRuns{ByStatus: map[types.RunStatus]int{}, Items: []RunIdentity{}}, AgentAggregates: []AgentAggregate{}})
	if !strings.Contains(output, "no agent invocations recorded yet") {
		t.Fatalf("detailed empty state omitted compatibility message:\n%s", output)
	}
}

func TestCSVDescribesPartialAgentAggregateCoverage(t *testing.T) {
	database, run := newAuditRun(t)
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	knownInput := 10
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: "codex",
		SessionMode: db.InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 100, ExitStatus: "ok",
		InputTokens: knownInput, DeltaInputTokens: &knownInput,
	})
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: string(types.StepReview), Round: 2, Purpose: "review", Agent: "codex",
		SessionMode: db.InvocationModeResumed, StartedAt: 3, CompletedAt: 4, DurationMS: 100, ExitStatus: "ok",
	})
	report, err := BuildReport(database, Query{}, time.Unix(run.CreatedAt+1, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	csvOutput, err := RenderCSV(report)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(csvOutput)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	foundCoverage, foundFreshBasis, foundAggregate, foundStep, foundRepo := false, false, false, false, false
	for _, row := range rows[1:] {
		switch row[15] {
		case "/agent_aggregates/0/input_tokens":
			if row[8] != "" || row[10] != "1" || row[11] != "2" || row[12] != "false" || row[13] != "selected_invocation_delta_meters" || row[14] != "partial_coverage" {
				t.Fatalf("partial aggregate CSV fact = %v", row)
			}
			foundCoverage = true
		case "/agent_aggregates/0/fresh_input_tokens":
			foundFreshBasis = row[13] == "selected_invocation_canonical_delta_meters"
		case "/agent_aggregates/0/purpose":
			foundAggregate = row[3] == "agent_aggregate" && row[4] == "review" && row[6] == "review"
		case "/dashboard/steps/0/step":
			foundStep = row[3] == "dashboard_step" && row[4] == string(types.StepReview)
		case "/dashboard/repositories/0/repo_id":
			foundRepo = row[1] == run.RepoID && row[3] == "dashboard_repository" && row[4] == run.RepoID
		}
	}
	if !foundCoverage || !foundFreshBasis || !foundAggregate || !foundStep || !foundRepo {
		t.Fatalf("CSV contexts = coverage:%t fresh_basis:%t aggregate:%t step:%t repo:%t\n%s", foundCoverage, foundFreshBasis, foundAggregate, foundStep, foundRepo, csvOutput)
	}
}

func TestTextJSONAndCSVProjectContentFreeCommandAndRepairFacts(t *testing.T) {
	database, run := newAuditRun(t)
	step, err := database.InsertStepResult(run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	round, err := database.InsertStepRound(step.ID, 1, "auto_fix", nil, nil, 25)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepRoundRepairAudit(round.ID, "sha256:repair-fingerprint", "resolved"); err != nil {
		t.Fatal(err)
	}
	zero := 0
	version := "5.2.26"
	const privateCommand = "/private/worktree/COMMAND-MUST-NOT-BE-PROJECTED"
	if err := database.SetStepEvidence(step.ID, db.StepEvidence{Commands: []db.CommandEvidence{
		{
			Round: 1, Sequence: 1, Command: privateCommand, Outcome: db.CommandOutcomePassed, ExitCode: &zero,
			CommandSource: runner.SourceLinux,
			Runner: &runner.Provenance{
				SchemaVersion: runner.SchemaVersion, Platform: "linux", Source: runner.SourceLinux,
				Executable: "zsh", Args: []string{"-lc"}, Version: &version,
			},
		},
		{Round: 1, Sequence: 2, Command: privateCommand, Outcome: db.CommandOutcomeError},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 25, ""); err != nil {
		t.Fatal(err)
	}

	report, err := BuildReport(database, Query{RunID: run.ID}, time.Unix(run.CreatedAt+1, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Repairs) != 1 || report.Repairs[0].RepairFailureFingerprint == nil || *report.Repairs[0].RepairFailureFingerprint != "sha256:repair-fingerprint" || report.Repairs[0].RepairResult == nil || *report.Repairs[0].RepairResult != "resolved" {
		t.Fatalf("report repairs = %+v", report.Repairs)
	}
	jsonOutput, err := report.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	textOutput := RenderText(report)
	csvOutput, err := RenderCSV(report)
	if err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{"json": jsonOutput, "text": textOutput, "csv": csvOutput} {
		if strings.Contains(output, privateCommand) {
			t.Fatalf("%s projection retained command content:\n%s", name, output)
		}
	}
	for _, want := range []string{"failure_fingerprint=sha256:repair-fingerprint", "result=resolved", "command " + run.ID + "/build/1/1", "runner=zsh", "command " + run.ID + "/build/1/2", "runner=—"} {
		if !strings.Contains(textOutput, want) {
			t.Fatalf("text projection omitted %q:\n%s", want, textOutput)
		}
	}
	if !strings.Contains(jsonOutput, `"repair_failure_fingerprint":"sha256:repair-fingerprint"`) || !strings.Contains(jsonOutput, `"runner":{"schema_version":1`) || !strings.Contains(jsonOutput, `"runner":null`) {
		t.Fatalf("JSON projection omitted audit facts:\n%s", jsonOutput)
	}

	rows, err := csv.NewReader(strings.NewReader(csvOutput)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	commandFacts := map[string]map[string][]string{}
	repairFacts := map[string]string{}
	for _, row := range rows[1:] {
		switch row[3] {
		case "command":
			if commandFacts[row[4]] == nil {
				commandFacts[row[4]] = map[string][]string{}
			}
			commandFacts[row[4]][row[7]] = row
		case "repair":
			repairFacts[row[7]] = row[8]
		}
	}
	firstID := step.ID + ":1:1"
	secondID := step.ID + ":1:2"
	if commandFacts[firstID]["outcome"][8] != db.CommandOutcomePassed || commandFacts[firstID]["command_source"][8] != runner.SourceLinux || commandFacts[firstID]["runner.executable"][8] != "zsh" || commandFacts[firstID]["runner.args.0"][8] != "-lc" {
		t.Fatalf("first command CSV facts = %+v", commandFacts[firstID])
	}
	if commandFacts[secondID]["outcome"][8] != db.CommandOutcomeError || commandFacts[secondID]["runner"][10] != "0" || commandFacts[secondID]["runner"][12] != "false" || commandFacts[secondID]["runner"][14] != "not_reported" {
		t.Fatalf("runner-less command CSV facts = %+v", commandFacts[secondID])
	}
	if repairFacts["repair_failure_fingerprint"] != "sha256:repair-fingerprint" || repairFacts["repair_result"] != "resolved" {
		t.Fatalf("repair CSV facts = %+v", repairFacts)
	}
}

func TestProjectionsCarryPopulatedCostProvenanceCoverageAndIntegrityErrors(t *testing.T) {
	database, run := newAuditRun(t)
	input, output, cacheRead, cacheWrite := 1_000_000, 1_000_000, 1_000_000, 1_000_000
	reported := 9.25
	listValue, effectiveValue := 36.75, 40.75
	provider := "anthropic"
	receiptBytes, err := json.Marshal(legacycost.CostClasses{
		HarnessReported: legacycost.CostEstimate{
			ValueUSD: &reported, Coverage: legacycost.Coverage{Reported: 1, Eligible: 1},
			Complete: true, Basis: "agent_invocations.reported_cost_usd",
		},
		APIListEstimate: legacycost.CostEstimate{
			ValueUSD: &listValue, Coverage: legacycost.Coverage{Reported: 4, Eligible: 4},
			Complete: true, Basis: "canonical_delta_token_meters_x_public_list_rate",
			Provenance: legacycost.Provenance{CatalogVersion: 2, CatalogSHA256: "historical-catalog"},
		},
		HarnessAdjustedEstimate: legacycost.CostEstimate{
			ValueUSD: &effectiveValue, Coverage: legacycost.Coverage{Reported: 4, Eligible: 4},
			Complete: true, Basis: "public_list_estimate_plus_harness_profile",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt := string(receiptBytes)
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "cursor", Model: "claude-opus-5", ModelProvider: &provider,
		SessionMode: db.InvocationModeCold, StartedAt: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC).Unix(), CompletedAt: time.Date(2026, 8, 17, 0, 1, 0, 0, time.UTC).Unix(), ExitStatus: "ok",
		DeltaInputTokens: &input, DeltaOutputTokens: &output, DeltaCacheReadTokens: &cacheRead, DeltaCacheCreationTokens: &cacheWrite,
		ReportedCostUSD: &reported, PricingReceiptJSON: &receipt,
	})

	report, err := BuildReport(database, Query{RunID: run.ID}, time.Unix(run.CreatedAt+1, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	jsonOutput, err := report.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	textOutput := RenderText(report)
	csvOutput, err := RenderCSV(report)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(csvOutput)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	assertCSVLeafParity(t, jsonOutput, rows)
	byPath := map[string][]string{}
	for _, row := range rows[1:] {
		byPath[row[15]] = row
	}
	valueRow := byPath["/agents/0/invocation/costs/api_list_estimate/value_usd"]
	if valueRow[8] != "36.75" || valueRow[10] != "4" || valueRow[11] != "4" || valueRow[12] != "true" || valueRow[13] != "canonical_delta_token_meters_x_public_list_rate" {
		t.Fatalf("API list CSV fact = %v", valueRow)
	}
	provenanceRow := byPath["/costs/items/0/classes/api_list_estimate/provenance/catalog_sha256"]
	if provenanceRow[8] == "" {
		t.Fatalf("catalog provenance CSV fact = %v", provenanceRow)
	}
	if errorRow := byPath["/data_errors/0/detail"]; errorRow[8] == "" {
		t.Fatalf("integrity error CSV fact = %v", errorRow)
	}
	for _, want := range []string{"coverage=4/4", "catalog=v2@", "data_error run="} {
		if !strings.Contains(textOutput, want) {
			t.Fatalf("text projection omitted %q:\n%s", want, textOutput)
		}
	}
}

type projectedLeaf struct {
	value   string
	present bool
}

func assertCSVLeafParity(t *testing.T, canonicalJSON string, rows [][]string) {
	t.Helper()
	expected := canonicalLeaves(t, canonicalJSON)
	if len(rows)-1 != len(expected) {
		t.Fatalf("CSV facts = %d, JSON leaves = %d", len(rows)-1, len(expected))
	}
	seen := make(map[string]bool, len(expected))
	for _, row := range rows[1:] {
		path := row[15]
		leaf, ok := expected[path]
		if !ok {
			t.Fatalf("CSV contains non-JSON fact path %q", path)
		}
		if seen[path] {
			t.Fatalf("CSV repeats JSON fact path %q", path)
		}
		seen[path] = true
		if leaf.present && row[8] != leaf.value {
			t.Fatalf("CSV %s value = %q, want %q", path, row[8], leaf.value)
		}
		if !leaf.present && (row[8] != "" || row[10] != "0" || row[12] != "false") {
			t.Fatalf("CSV null %s = %v", path, row)
		}
	}
}

func canonicalLeaves(t *testing.T, encoded string) map[string]projectedLeaf {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.UseNumber()
	var envelope any
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	result := map[string]projectedLeaf{}
	var walk func([]string, any)
	walk = func(path []string, value any) {
		switch typed := value.(type) {
		case map[string]any:
			if len(typed) == 0 {
				result[testJSONPointer(path)] = projectedLeaf{value: "{}", present: true}
			}
			for key, item := range typed {
				walk(append(append([]string(nil), path...), key), item)
			}
		case []any:
			if len(typed) == 0 {
				result[testJSONPointer(path)] = projectedLeaf{value: "[]", present: true}
			}
			for index, item := range typed {
				walk(append(append([]string(nil), path...), strconv.Itoa(index)), item)
			}
		case nil:
			result[testJSONPointer(path)] = projectedLeaf{}
		case string:
			result[testJSONPointer(path)] = projectedLeaf{value: typed, present: true}
		case json.Number:
			result[testJSONPointer(path)] = projectedLeaf{value: typed.String(), present: true}
		case bool:
			result[testJSONPointer(path)] = projectedLeaf{value: strconv.FormatBool(typed), present: true}
		default:
			t.Fatalf("unexpected canonical JSON leaf %T at %v", typed, path)
		}
	}
	walk(nil, envelope)
	return result
}

func testJSONPointer(path []string) string {
	escaped := make([]string, len(path))
	for index, segment := range path {
		escaped[index] = strings.ReplaceAll(strings.ReplaceAll(segment, "~", "~0"), "/", "~1")
	}
	return fmt.Sprintf("/%s", strings.Join(escaped, "/"))
}
