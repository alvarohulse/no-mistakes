package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	runstats "github.com/kunchenguid/no-mistakes/internal/stats"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestStatsAgentsReportsLocalPerformanceTelemetry proves the read-only
// report surface exposes the locally persisted invocation evidence: per-
// purpose aggregates via --agents and per-run detail (including accumulated
// parked time) via --run.
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
		{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex", Model: "gpt-5.2", InvocationMode: types.AgentInvocationModeHarnessCLI, AgentObservationsReported: true, SessionMode: db.InvocationModeStarted, SessionKey: "deadbeef00000000", StartedAt: 1, CompletedAt: 2, DurationMS: 60_000, ExitStatus: "ok", InputTokens: 100, OutputTokens: 10, CacheReadTokens: 40, CacheCreationTokens: statsIntPtr(20)},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex", Model: "gpt-5.2", SessionMode: db.InvocationModeResumed, SessionKey: "deadbeef00000000", StartedAt: 3, CompletedAt: 4, DurationMS: 30_000, ExitStatus: "ok", InputTokens: 50, OutputTokens: 5, CacheReadTokens: 45, CacheCreationTokens: statsIntPtr(25)},
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
	for _, want := range []string{"PURPOSE", "review", "review-fix", "RESUMED", "CACHE WRITE TOK", "45"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats --agents missing %q in:\n%s", want, out)
		}
	}

	out, err = executeCmd("stats", "--run", run.ID)
	if err != nil {
		t.Fatalf("stats --run: %v\n%s", err, out)
	}
	for _, want := range []string{run.ID, "parked at gates 1m30s total", "resumed", "deadbeef00000000", "gpt-5.2", "CACHE WR", "20", "none"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats --run missing %q in:\n%s", want, out)
		}
	}
	// The seeded rows carry no activity metrics, so those fields render as the
	// unknown marker, distinct from a recorded zero.
	if !strings.Contains(out, "-") {
		t.Fatalf("stats --run should render unknown metric fields as \"-\":\n%s", out)
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
	for _, want := range []string{"ROUNDTRIPS", "TEST/LINT", "SUBPROC", "24", "METRICS", "1/1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats --agents missing %q in:\n%s", want, out)
		}
	}

	out, err = executeCmd("stats", "--run", run.ID)
	if err != nil {
		t.Fatalf("stats --run: %v\n%s", err, out)
	}
	// Per-round delta (1500) is shown distinctly from the raw cumulative (2500),
	// the tool histogram and the workload render, and the model-time split appears.
	for _, want := range []string{"Δ IN (round)", "1500", "2500", "7 0/2/3/1/1/0", "12/1060", "MODEL", "INVOKED VIA", "harness_cli", "Explore (subagent_tool)"} {
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

func TestStatsRunJSONUsesCanonicalRunAudit(t *testing.T) {
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
	var audit runstats.RunAudit
	if err := json.Unmarshal([]byte(out), &audit); err != nil {
		t.Fatalf("decode canonical audit: %v\n%s", err, out)
	}
	if audit.SchemaVersion != runstats.SchemaVersion || audit.Run.ID != run.ID || audit.Run.NoMistakesVersion == nil || audit.Run.NoMistakesBuildSHA == nil || audit.Metrics.DeltaInputTokens.Value == nil || *audit.Metrics.DeltaInputTokens.Value != 0 {
		t.Fatalf("run audit = %+v", audit)
	}
	if strings.Contains(out, "/tmp/repo-json") {
		t.Fatalf("run audit leaked repository path: %s", out)
	}
}

func TestStatsJSONRequiresRun(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	out, err := executeCmd("stats", "--format", "json")
	if err == nil || !strings.Contains(err.Error(), "requires --run") {
		t.Fatalf("stats --format json = error %v output %q", err, out)
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
