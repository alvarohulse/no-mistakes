package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	runstats "github.com/kunchenguid/no-mistakes/internal/stats"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestStatsAgentsReportsLocalPerformanceTelemetry proves the read-only
// report surface exposes locally persisted invocation evidence through the
// shared report envelope for both --agents and --run.
func TestStatsAgentsReportsLocalPerformanceTelemetry(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature/x", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	seed := []db.AgentInvocation{
		{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex", Model: "gpt-5.2", InvocationMode: types.AgentInvocationModeHarnessCLI, AgentObservationsReported: true, SessionMode: db.InvocationModeStarted, SessionKey: "deadbeef00000000", StartedAt: 1, CompletedAt: 2, DurationMS: 60_000, ExitStatus: "ok", InputTokens: 100, OutputTokens: 10, CacheReadTokens: 40, CacheCreationTokens: statsIntPtr(20), DeltaInputTokens: statsIntPtr(100), DeltaOutputTokens: statsIntPtr(10), DeltaCacheReadTokens: statsIntPtr(40), DeltaCacheCreationTokens: statsIntPtr(20)},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex", Model: "gpt-5.2", SessionMode: db.InvocationModeResumed, SessionKey: "deadbeef00000000", StartedAt: 3, CompletedAt: 4, DurationMS: 30_000, ExitStatus: "ok", InputTokens: 50, OutputTokens: 5, CacheReadTokens: 45, CacheCreationTokens: statsIntPtr(25), DeltaInputTokens: statsIntPtr(50), DeltaOutputTokens: statsIntPtr(5), DeltaCacheReadTokens: statsIntPtr(45), DeltaCacheCreationTokens: statsIntPtr(25)},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review-fix", Agent: "codex", Model: "gpt-5.2", SessionMode: db.InvocationModeStarted, SessionKey: "feedface00000000", StartedAt: 5, CompletedAt: 6, DurationMS: 45_000, ExitStatus: "ok"},
	}
	for _, inv := range seed {
		if _, err := d.InsertAgentInvocation(inv); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.AddRunParkedDuration(run.ID, 90_000); err != nil {
		t.Fatal(err)
	}
	d.Close()

	out, err := executeCmd("stats", "--agents")
	if err != nil {
		t.Fatalf("stats --agents: %v\n%s", err, out)
	}
	for _, want := range []string{"PURPOSE", "review", "review-fix", "RESUMED", "CACHE WRITE TOK", "45", "session=resumed", "cache_write_tokens=25"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats --agents missing %q in:\n%s", want, out)
		}
	}

	out, err = executeCmd("stats", "--run", run.ID)
	if err != nil {
		t.Fatalf("stats --run: %v\n%s", err, out)
	}
	for _, want := range []string{run.ID, "parked_ms=90000", "session=resumed", "gpt-5.2", "cache_write_tokens=20", "nested_agents=0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats --run missing %q in:\n%s", want, out)
		}
	}
	// The seeded rows carry no activity metrics, so those fields render as the
	// unknown marker, distinct from a recorded zero.
	if !strings.Contains(out, "=—") {
		t.Fatalf("stats --run should render unknown metric fields as an em dash:\n%s", out)
	}
}

func statsIntPtr(v int) *int       { return &v }
func statsInt64Ptr(v int64) *int64 { return &v }

// TestStatsRendersPopulatedFidelityMetrics proves the report surfaces the new
// activity histogram, subprocess/model time split, and per-round token deltas
// when they are recorded.
func TestStatsRendersPopulatedFidelityMetrics(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature/x", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	inv := db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 2, Purpose: "review-fix", Agent: "codex",
		InvocationMode: types.AgentInvocationModeHarnessCLI,
		AgentObservations: []types.AgentObservation{{
			Identity:       "Explore",
			InvocationMode: types.AgentInvocationModeSubagentTool,
		}},
		AgentObservationsReported: true,
		Model:                     "gpt-5.6-sol", ModelProvider: strPtrCLI("openai"),
		SessionMode: db.InvocationModeResumed, SessionKey: "deadbeef00000000",
		StartedAt: 1, CompletedAt: 2, DurationMS: 10_000, SubprocessWaitMS: statsInt64Ptr(2_000),
		ExitStatus: "ok", InputTokens: 2500, OutputTokens: 250, CacheReadTokens: 1800,
		FreshInputTokens: statsIntPtr(700), ReasoningTokens: statsIntPtr(9),
		DeltaInputTokens: statsIntPtr(1500), DeltaOutputTokens: statsIntPtr(150), DeltaCacheReadTokens: statsIntPtr(1200),
		ModelRoundtrips: statsIntPtr(24), ToolCalls: statsIntPtr(7),
		ToolWaitCalls: statsIntPtr(0), ToolTestLintCalls: statsIntPtr(2), ToolEditCalls: statsIntPtr(3),
		ToolReadCalls: statsIntPtr(1), ToolGitCalls: statsIntPtr(1), ToolOtherCalls: statsIntPtr(0),
		WorkloadFiles: statsIntPtr(12), WorkloadLines: statsIntPtr(1060), FindingCount: statsIntPtr(3),
	}
	if _, err := d.InsertAgentInvocation(inv); err != nil {
		t.Fatal(err)
	}
	d.Close()

	out, err := executeCmd("stats", "--agents")
	if err != nil {
		t.Fatalf("stats --agents: %v\n%s", err, out)
	}
	for _, want := range []string{"ROUNDTRIPS", "TEST/LINT", "SUBPROC", "24", "METRICS", "1/1", "roundtrips=24", "test_lint_calls=2", "subprocess_wait_ms=2000"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats --agents missing %q in:\n%s", want, out)
		}
	}

	out, err = executeCmd("stats", "--run", run.ID)
	if err != nil {
		t.Fatalf("stats --run: %v\n%s", err, out)
	}
	// Per-round delta is shown distinctly from raw cumulative usage alongside
	// activity, workload, invocation mode, and nested-agent facts.
	for _, want := range []string{"delta_input_tokens=1500", "raw_input_tokens=2500", "tool_calls=7", "workload=12/1060", "harness=codex", "invoked_via=harness_cli", "Explore(subagent_tool)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats --run missing %q in:\n%s", want, out)
		}
	}
}

func strPtrCLI(s string) *string { return &s }

func TestStatsRunLabelsHistoricalRefreshInvocationFromRunStrategy(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepoWithID("repo-refresh", "/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRunWithOptions(repo.ID, "feature/stack", "abc", "def", db.RunOptions{RefreshStrategy: types.RefreshStrategyMerge})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertAgentInvocation(db.AgentInvocation{
		RunID:          run.ID,
		StepName:       "rebase",
		Round:          1,
		Purpose:        "refresh",
		Agent:          "codex",
		InvocationMode: types.AgentInvocationModeHarnessCLI,
		SessionMode:    db.InvocationModeCold,
		StartedAt:      1,
		CompletedAt:    2,
		DurationMS:     1,
		ExitStatus:     "ok",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := renderRunAgentPerf(&out, d, run.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Merge") || strings.Contains(out.String(), "rebase") {
		t.Fatalf("historical invocation did not use strategy-aware display label:\n%s", out.String())
	}
}

func TestStatsRunJSONUsesSharedReportEnvelope(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepoWithID("repo-json", "/tmp/repo-json", "https://github.com/test/repo-json", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/json", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	if _, err := database.InsertAgentInvocation(db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex",
		InvocationMode: types.AgentInvocationModeHarnessCLI, SessionMode: db.InvocationModeCold,
		StartedAt: 1, CompletedAt: 2, DurationMS: 1, ExitStatus: "ok",
		DeltaInputTokens: &zero, DeltaOutputTokens: &zero, DeltaCacheReadTokens: &zero, DeltaCacheCreationTokens: &zero,
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("stats", "--run", run.ID, "--format", "json")
	if err != nil {
		t.Fatalf("stats --run --format json: %v\n%s", err, out)
	}
	var report runstats.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode stats report: %v\n%s", err, out)
	}
	if report.SchemaVersion != runstats.ReportSchemaVersion || report.Scope.RunID != run.ID || len(report.Runs.Items) != 1 || report.Runs.Items[0].ID != run.ID ||
		report.Runs.Items[0].NoMistakesVersion == nil || report.Runs.Items[0].NoMistakesBuildSHA == nil || len(report.Agents) != 1 ||
		report.Agents[0].Invocation.DeltaUsage.InputTokens == nil || *report.Agents[0].Invocation.DeltaUsage.InputTokens != 0 || len(report.Metrics.Items) != 1 ||
		report.Metrics.Items[0].Metrics.DeltaInputTokens.Value == nil || *report.Metrics.Items[0].Metrics.DeltaInputTokens.Value != 0 {
		t.Fatalf("stats report = %+v", report)
	}
	if strings.Contains(out, "/tmp/repo-json") {
		t.Fatalf("run audit leaked repository path: %s", out)
	}
}

func TestStatsJSONSupportsAggregateReportWithoutRun(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	out, err := executeCmd("stats", "--format", "json")
	if err != nil {
		t.Fatalf("stats --format json: %v\n%s", err, out)
	}
	var report runstats.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode stats report: %v\n%s", err, out)
	}
	if report.SchemaVersion != runstats.ReportSchemaVersion || report.Runs.Count != 0 {
		t.Fatalf("stats report = %+v", report)
	}
}

func TestStatsEveryOutputFormatUsesReportEnvelope(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	textOutput, err := executeCmd("stats", "--format", "text")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(textOutput, "Total changes") || !strings.Contains(textOutput, "git push no-mistakes") {
		t.Fatalf("text did not render the report dashboard:\n%s", textOutput)
	}

	jsonOutput, err := executeCmd("stats", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var report runstats.Report
	if err := json.Unmarshal([]byte(jsonOutput), &report); err != nil || report.SchemaVersion != runstats.ReportSchemaVersion {
		t.Fatalf("JSON report = %+v, err = %v", report, err)
	}

	csvOutput, err := executeCmd("stats", "--format", "csv")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(csvOutput)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	foundSchema := false
	for _, row := range rows[1:] {
		if row[15] == "/schema_version" && row[8] == strconv.Itoa(runstats.ReportSchemaVersion) {
			foundSchema = true
			break
		}
	}
	if !foundSchema {
		t.Fatalf("CSV omitted Report schema fact:\n%s", csvOutput)
	}
}

func TestStatsDirectFiltersUseOneReportForTextJSONAndCSV(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepoWithID("repo-filter", "/tmp/repo-filter", "https://github.com/test/repo-filter", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/filter", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	provider := "openai"
	if _, err := database.InsertAgentInvocation(db.AgentInvocation{
		RunID: run.ID, StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: "codex",
		Model: "gpt-5.6-sol", ModelProvider: &provider, InvocationMode: types.AgentInvocationModeHarnessCLI,
		SessionMode: db.InvocationModeCold, StartedAt: run.CreatedAt, CompletedAt: run.CreatedAt + 1, ExitStatus: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	args := []string{"stats", "--repo", repo.ID, "--step", "review", "--agent", "codex", "--model", "gpt-5.6-sol", "--purpose", "review", "--status", "completed"}
	for _, format := range []string{"text", "json", "csv"} {
		out, err := executeCmd(append(args, "--format", format)...)
		if err != nil {
			t.Fatalf("stats --format %s: %v\n%s", format, err, out)
		}
		for _, want := range []string{run.ID, "completed"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s output missing %q:\n%s", format, want, out)
			}
		}
	}
}

func TestStatsRunJSONAcceptsExplicitAgentsFlag(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	_, database, err := openResources()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepoWithID("repo-json-agents", "/tmp/repo-json-agents", "https://github.com/test/repo-json-agents", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/json-agents", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("stats", "--agents", "--run", run.ID, "--format", "json")
	if err != nil {
		t.Fatalf("stats --agents --run --format json: %v\n%s", err, out)
	}
}
