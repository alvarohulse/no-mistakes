package db

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

func TestAgentInvocations_InsertAndReadBack(t *testing.T) {
	d, _, run := openSessionTestDB(t)

	inv := AgentInvocation{
		RunID:                    run.ID,
		StepName:                 "review",
		Round:                    2,
		Purpose:                  "review-fix",
		Agent:                    "codex",
		UsageCoverage:            agent.UsageCoverageComplete,
		Model:                    "gpt-5.2-codex",
		SessionMode:              InvocationModeResumed,
		SessionKey:               "abcd1234abcd1234",
		StartedAt:                1_700_000_000,
		CompletedAt:              1_700_000_090,
		DurationMS:               90_000,
		ExitStatus:               "ok",
		InputTokens:              1000,
		OutputTokens:             200,
		CacheReadTokens:          800,
		CacheCreationTokens:      intPtr(50),
		DeltaCacheCreationTokens: intPtr(50),
		ReportedCostUSD:          float64Ptr(1.25),
	}
	if _, err := d.InsertAgentInvocation(inv); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := d.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d invocations, want 1", len(got))
	}
	back := got[0]
	if back.Purpose != "review-fix" || back.Round != 2 || back.SessionMode != InvocationModeResumed ||
		back.DurationMS != 90_000 || back.InputTokens != 1000 || back.CacheReadTokens != 800 || back.Model != "gpt-5.2-codex" {
		t.Fatalf("readback mismatch: %+v", back)
	}
	if back.UsageCoverage != agent.UsageCoverageComplete {
		t.Fatalf("usage coverage = %q, want complete", back.UsageCoverage)
	}
	if back.CacheCreationTokens == nil || *back.CacheCreationTokens != 50 {
		t.Fatalf("cache creation readback = %v, want 50", back.CacheCreationTokens)
	}
	if back.DeltaCacheCreationTokens == nil || *back.DeltaCacheCreationTokens != 50 || back.ReportedCostUSD == nil || *back.ReportedCostUSD != 1.25 {
		t.Fatalf("cache-write/cost readback = %v/%v", back.DeltaCacheCreationTokens, back.ReportedCostUSD)
	}
}

func TestAgentInvocations_ReviewCandidatePoolRoundTrip(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	pool := []ReviewCandidateReceipt{
		{Agent: "claude", Model: "claude-opus-5", Vendor: "anthropic"},
		{Agent: "cursor", Model: "grok-4.6", Vendor: "xai", Optional: true},
	}
	inv := AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "claude",
		Model: "claude-opus-5", ModelProvider: strPtr("anthropic"), ReviewCandidatePool: pool,
		SessionMode: InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 1, ExitStatus: "ok",
	}
	if _, err := d.InsertAgentInvocation(inv); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := d.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0].ReviewCandidatePool, pool) {
		t.Fatalf("review candidate pool = %#v, want %#v", got, pool)
	}
	var encoded *string
	if err := d.sql.QueryRow(`SELECT review_candidate_pool_json FROM agent_invocations WHERE run_id = ?`, run.ID).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	wantJSON := `[{"agent":"claude","model":"claude-opus-5","vendor":"anthropic"},{"agent":"cursor","model":"grok-4.6","vendor":"xai","optional":true}]`
	if encoded == nil || *encoded != wantJSON {
		t.Fatalf("candidate pool JSON = %v, want %s", encoded, wantJSON)
	}
}

func TestAgentInvocations_RejectsUnsafeReviewCandidateReceipt(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	_, err := d.InsertAgentInvocation(AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "claude",
		ReviewCandidatePool: []ReviewCandidateReceipt{{Agent: "claude", Model: "prompt\ncontents", Vendor: "anthropic"}},
		SessionMode:         InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 1, ExitStatus: "ok",
	})
	if err == nil || !strings.Contains(err.Error(), "candidate") {
		t.Fatalf("InsertAgentInvocation() error = %v, want unsafe candidate refusal", err)
	}
}

func TestLatestSessionCumulativePreservesUnknownImmediatePrior(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	usage := AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex",
		SessionMode: InvocationModeResumed, SessionKey: "session", StartedAt: 1, CompletedAt: 2,
		ExitStatus: "ok", InputTokens: 1000, OutputTokens: 100, CacheReadTokens: 600,
		CacheCreationTokens: intPtr(20), DeltaInputTokens: intPtr(1000), DeltaOutputTokens: intPtr(100),
		DeltaCacheReadTokens: intPtr(600), DeltaCacheCreationTokens: intPtr(20),
	}
	if _, err := d.InsertAgentInvocation(usage); err != nil {
		t.Fatal(err)
	}
	failed := AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex",
		SessionMode: InvocationModeResumed, SessionKey: "session", StartedAt: 3, CompletedAt: 4,
		ExitStatus: "error",
	}
	if _, err := d.InsertAgentInvocation(failed); err != nil {
		t.Fatal(err)
	}

	meters, found := d.LatestSessionCumulativeMeters(run.ID, "session")
	if !found {
		t.Fatal("latest session invocation not found")
	}
	if meters.Input != nil || meters.Output != nil || meters.CacheRead != nil || meters.CacheCreation != nil {
		t.Fatalf("latest unknown usage became known: %+v", meters)
	}
}

func intPtr(v int) *int             { return &v }
func float64Ptr(v float64) *float64 { return &v }

// TestAgentInvocations_NullableFidelityFieldsRoundTrip proves the session-
// fidelity columns survive an insert/read cycle both when populated and when
// left unknown (NULL), so missing data reads back as nil rather than zero.
func TestAgentInvocations_NullableFidelityFieldsRoundTrip(t *testing.T) {
	d, _, run := openSessionTestDB(t)

	// Fully populated row.
	full := AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex",
		Model: "gpt-5.6-sol", ModelProvider: strPtr("openai"),
		SessionMode: InvocationModeFallback, SessionKey: "key1", FallbackReason: strPtr(FallbackReasonExit),
		StartedAt: 1, CompletedAt: 2, DurationMS: 5000, SubprocessWaitMS: int64Ptr(1200),
		ExitStatus: "ok", InputTokens: 2500, OutputTokens: 250, CacheReadTokens: 1800,
		CacheCreationTokens: intPtr(0), FreshInputTokens: intPtr(700), ReasoningTokens: intPtr(9),
		DeltaInputTokens: intPtr(1500), DeltaOutputTokens: intPtr(150), DeltaCacheReadTokens: intPtr(1200),
		DeltaCacheCreationTokens: intPtr(25), ReportedCostUSD: float64Ptr(2.5),
		ModelRoundtrips: intPtr(4), ToolCalls: intPtr(3),
		ToolWaitCalls: intPtr(0), ToolTestLintCalls: intPtr(1), ToolEditCalls: intPtr(1),
		ToolReadCalls: intPtr(1), ToolGitCalls: intPtr(0), ToolOtherCalls: intPtr(0),
		WorkloadFiles: intPtr(4), WorkloadLines: intPtr(120), FindingCount: intPtr(2),
	}
	if _, err := d.InsertAgentInvocation(full); err != nil {
		t.Fatalf("insert full: %v", err)
	}
	// Minimal row: every nullable field unknown.
	minimal := AgentInvocation{
		RunID: run.ID, StepName: "test", Round: 1, Purpose: "test-evidence", Agent: "codex",
		SessionMode: InvocationModeCold, StartedAt: 3, CompletedAt: 4, DurationMS: 10, ExitStatus: "ok",
	}
	if _, err := d.InsertAgentInvocation(minimal); err != nil {
		t.Fatalf("insert minimal: %v", err)
	}

	got, err := d.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	f := got[0]
	if f.ModelProvider == nil || *f.ModelProvider != "openai" ||
		f.FallbackReason == nil || *f.FallbackReason != FallbackReasonExit ||
		f.SubprocessWaitMS == nil || *f.SubprocessWaitMS != 1200 ||
		f.CacheCreationTokens == nil || *f.CacheCreationTokens != 0 ||
		f.DeltaInputTokens == nil || *f.DeltaInputTokens != 1500 ||
		f.DeltaCacheCreationTokens == nil || *f.DeltaCacheCreationTokens != 25 ||
		f.ReportedCostUSD == nil || *f.ReportedCostUSD != 2.5 ||
		f.ToolTestLintCalls == nil || *f.ToolTestLintCalls != 1 ||
		f.FindingCount == nil || *f.FindingCount != 2 {
		t.Fatalf("full row lost a fidelity field: %+v", f)
	}
	m := got[1]
	for name, isNil := range map[string]bool{
		"ModelProvider":    m.ModelProvider == nil,
		"FallbackReason":   m.FallbackReason == nil,
		"SubprocessWaitMS": m.SubprocessWaitMS == nil,
		"CacheCreation":    m.CacheCreationTokens == nil,
		"FreshInput":       m.FreshInputTokens == nil,
		"DeltaInput":       m.DeltaInputTokens == nil,
		"DeltaCacheWrite":  m.DeltaCacheCreationTokens == nil,
		"ReportedCost":     m.ReportedCostUSD == nil,
		"ModelRoundtrips":  m.ModelRoundtrips == nil,
		"ToolCalls":        m.ToolCalls == nil,
		"WorkloadFiles":    m.WorkloadFiles == nil,
		"FindingCount":     m.FindingCount == nil,
	} {
		if !isNil {
			t.Fatalf("minimal row %s should read back as unknown (nil)", name)
		}
	}
}

func strPtr(s string) *string { return &s }
func int64Ptr(v int64) *int64 { return &v }

// TestAgentInvocations_PrivacySafeShape guards the privacy boundary: the
// table has no column that could hold prompts, outputs, or diffs, and the
// session identity is stored only as a fingerprint column.
func TestAgentInvocations_PrivacySafeShape(t *testing.T) {
	d, _, _ := openSessionTestDB(t)

	rows, err := d.sql.Query(`SELECT name FROM pragma_table_info('agent_invocations')`)
	if err != nil {
		t.Fatalf("table info: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	for _, col := range columns {
		lower := strings.ToLower(col)
		if strings.HasSuffix(lower, "_tokens") {
			continue // token counts are numeric usage data, not content
		}
		for _, forbidden := range []string{"prompt", "output", "diff", "transcript", "secret", "credential", "text", "content"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("agent_invocations column %q could hold %s content", col, forbidden)
			}
		}
	}
	if !containsString(columns, "usage_coverage") {
		t.Fatalf("agent_invocations columns = %v, want usage_coverage", columns)
	}
	for _, removed := range []string{"invocation_mode", "agent_observations_json", "nested_agent_count"} {
		if containsString(columns, removed) {
			t.Fatalf("agent_invocations columns retained removed field %q: %v", removed, columns)
		}
	}
}

func TestOpenMigratesUsageCoverageAsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`ALTER TABLE agent_invocations DROP COLUMN usage_coverage`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`INSERT INTO agent_invocations
		(id, run_id, step_name, round, purpose, agent, model, session_mode, session_key, started_at, completed_at, duration_ms, exit_status, failure_category, input_tokens, output_tokens, cache_read_tokens)
		VALUES ('legacy-coverage', ?, 'review', 1, 'review', 'codex', '', 'cold', '', 1, 2, 1, 'ok', '', 100, 20, 50)`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UsageCoverage != agent.UsageCoverageUnknown {
		t.Fatalf("migrated invocation = %+v, want unknown usage coverage", got)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestAgentInvocations_HasRunTimelineIndex(t *testing.T) {
	d, _, _ := openSessionTestDB(t)

	rows, err := d.sql.Query(`SELECT name FROM pragma_index_list('agent_invocations')`)
	if err != nil {
		t.Fatalf("index list: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name == "idx_agent_invocations_run_started_id" {
			return
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("agent invocations must have a run timeline index")
}

func TestAgentInvocationAggregatesAndRunSummary(t *testing.T) {
	d, _, run := openSessionTestDB(t)

	seed := []AgentInvocation{
		{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex", SessionMode: InvocationModeStarted, StartedAt: 1, CompletedAt: 2, DurationMS: 100, ExitStatus: "ok", InputTokens: 10, OutputTokens: 5},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex", SessionMode: InvocationModeResumed, StartedAt: 3, CompletedAt: 4, DurationMS: 50, ExitStatus: "ok", InputTokens: 10, OutputTokens: 5},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review-fix", Agent: "codex", SessionMode: InvocationModeFallback, StartedAt: 5, CompletedAt: 6, DurationMS: 70, ExitStatus: "error", FailureCategory: "exit"},
	}
	for _, inv := range seed {
		if _, err := d.InsertAgentInvocation(inv); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	aggregates, err := d.AgentInvocationAggregates()
	if err != nil {
		t.Fatalf("aggregates: %v", err)
	}
	byPurpose := map[string]AgentInvocationAggregate{}
	for _, a := range aggregates {
		byPurpose[a.Purpose] = a
	}
	review := byPurpose["review"]
	if review.Count != 2 || review.TotalDurationMS != 150 || review.AvgDurationMS != 75 ||
		review.Started != 1 || review.Resumed != 1 || review.InputTokens != 20 {
		t.Fatalf("review aggregate = %+v", review)
	}
	fix := byPurpose["review-fix"]
	if fix.Count != 1 || fix.Fallback != 1 || fix.Errors != 1 {
		t.Fatalf("review-fix aggregate = %+v", fix)
	}

	summary, err := d.AgentInvocationSummaryForRun(run.ID)
	if err != nil {
		t.Fatalf("run summary: %v", err)
	}
	if summary.Count != 3 || summary.Resumed != 1 || summary.Fallback != 1 || summary.TotalDurationMS != 220 {
		t.Fatalf("run summary = %+v", summary)
	}
}

func TestAgentInvocationAggregatesPreserveUnknownMetrics(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	inv := AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex",
		SessionMode: InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 10, ExitStatus: "ok",
	}
	if _, err := d.InsertAgentInvocation(inv); err != nil {
		t.Fatal(err)
	}
	aggregates, err := d.AgentInvocationAggregates()
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregates) != 1 {
		t.Fatalf("got %d aggregates, want 1", len(aggregates))
	}
	a := aggregates[0]
	if a.SubprocessWaitMS != nil || a.CacheCreationTokens != nil ||
		a.FreshInputTokens != nil || a.ReasoningTokens != nil ||
		a.ModelRoundtrips != nil || a.ToolCalls != nil {
		t.Fatalf("unknown aggregate metrics became recorded values: %+v", a)
	}
}

func TestAgentInvocationAggregatesHidePartialMetrics(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	for _, inv := range []AgentInvocation{
		{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex", SessionMode: InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 10, ExitStatus: "ok", FreshInputTokens: intPtr(3)},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex", SessionMode: InvocationModeCold, StartedAt: 3, CompletedAt: 4, DurationMS: 10, ExitStatus: "ok"},
	} {
		if _, err := d.InsertAgentInvocation(inv); err != nil {
			t.Fatal(err)
		}
	}
	aggregates, err := d.AgentInvocationAggregates()
	if err != nil {
		t.Fatal(err)
	}
	if aggregates[0].FreshInputTokens != nil {
		t.Fatalf("partial fresh input = %v, want nil", *aggregates[0].FreshInputTokens)
	}
}

func TestAddRunParkedDurationAccumulates(t *testing.T) {
	d, _, run := openSessionTestDB(t)

	if err := d.AddRunParkedDuration(run.ID, 1500); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := d.AddRunParkedDuration(run.ID, 500); err != nil {
		t.Fatalf("add again: %v", err)
	}
	if err := d.AddRunParkedDuration(run.ID, 0); err != nil {
		t.Fatalf("zero add: %v", err)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.ParkedMS != 2000 {
		t.Fatalf("ParkedMS = %d, want 2000", got.ParkedMS)
	}
}

func TestCompleteRunAwaitingAgentAccumulatesParkedDuration(t *testing.T) {
	d, _, run := openSessionTestDB(t)

	if err := d.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatalf("set awaiting: %v", err)
	}
	if err := d.CompleteRunAwaitingAgent(run.ID, 1500); err != nil {
		t.Fatalf("complete awaiting: %v", err)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.AwaitingAgentSince != nil {
		t.Fatal("AwaitingAgentSince must be cleared after completion")
	}
	if got.ParkedMS != 1500 {
		t.Fatalf("ParkedMS = %d, want 1500", got.ParkedMS)
	}
}

// TestRecoverStaleRunsAccumulatesParkedTime proves a crash while parked does
// not lose the parked evidence: recovery folds the live awaiting marker into
// the run's parked total.
func TestRecoverStaleRunsAccumulatesParkedTime(t *testing.T) {
	d, _, run := openSessionTestDB(t)

	if err := d.UpdateRunStatus(run.ID, "running"); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	// Park the run 60 seconds in the past.
	past := now() - 60
	if _, err := d.sql.Exec(`UPDATE runs SET awaiting_agent_since = ? WHERE id = ?`, past, run.ID); err != nil {
		t.Fatalf("seed awaiting: %v", err)
	}

	if _, err := d.RecoverStaleRuns("daemon crashed during execution"); err != nil {
		t.Fatalf("recover: %v", err)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.AwaitingAgentSince != nil {
		t.Fatal("awaiting marker must be cleared by recovery")
	}
	if got.ParkedMS < 59_000 || got.ParkedMS > 62_000 {
		t.Fatalf("ParkedMS = %d, want ~60000 accumulated from the crashed park", got.ParkedMS)
	}
}

// TestOpenMigratesAgentInvocationsAndParkedMS proves databases created before
// the performance-telemetry schema gain the table and column on reopen.
func TestOpenMigratesAgentInvocationsAndParkedMS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := d.sql.Exec(`DROP TABLE agent_invocations`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	// Simulate a pre-parked_ms runs table by rebuilding it without the column.
	if _, err := d.sql.Exec(`ALTER TABLE runs DROP COLUMN parked_ms`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "b", "h", "b")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := d.AddRunParkedDuration(run.ID, 10); err != nil {
		t.Fatalf("parked_ms column missing after migration: %v", err)
	}
	if _, err := d.InsertAgentInvocation(AgentInvocation{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex", SessionMode: InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 1, ExitStatus: "ok"}); err != nil {
		t.Fatalf("agent_invocations table missing after migration: %v", err)
	}
}

// TestOpenMigratesSessionFidelityColumns proves a database whose
// agent_invocations table predates the session-fidelity columns gains them on
// reopen, and that pre-existing rows read those columns back as unknown (nil)
// rather than a fabricated zero.
func TestOpenMigratesSessionFidelityColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "b", "h", "b")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	// Simulate a pre-fidelity table by dropping the new columns, then insert a
	// legacy row that has no fidelity data.
	for _, col := range []string{"model_provider", "fallback_reason", "subprocess_wait_ms",
		"fresh_input_tokens", "reasoning_tokens", "model_roundtrips", "tool_calls", "finding_count", "review_candidate_pool_json"} {
		if _, err := d.sql.Exec(`ALTER TABLE agent_invocations DROP COLUMN ` + col); err != nil {
			t.Fatalf("drop %s: %v", col, err)
		}
	}
	if _, err := d.sql.Exec(`INSERT INTO agent_invocations
		(id, run_id, step_name, round, purpose, agent, model, session_mode, session_key, exit_status, failure_category, started_at, completed_at, duration_ms, input_tokens, output_tokens, cache_read_tokens)
		VALUES ('legacy1', ?, 'review', 1, 'review', 'codex', '', 'started', '', 'ok', '', 1, 2, 100, 500, 20, 300)`, run.ID); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d.Close()

	got, err := d.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatalf("get after migration: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	legacy := got[0]
	if legacy.UsageCoverage != agent.UsageCoverageUnknown {
		t.Fatalf("legacy usage coverage = %q, want unknown", legacy.UsageCoverage)
	}
	if legacy.InputTokens != 500 {
		t.Fatalf("legacy input tokens = %d, want 500", legacy.InputTokens)
	}
	if legacy.ModelProvider != nil || legacy.SubprocessWaitMS != nil ||
		legacy.ModelRoundtrips != nil || legacy.ToolCalls != nil || legacy.FindingCount != nil || legacy.ReviewCandidatePool != nil {
		t.Fatalf("legacy row must read new columns as unknown, got %+v", legacy)
	}
	// The migrated table now accepts the new fields.
	if _, err := d.InsertAgentInvocation(AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex",
		SessionMode: InvocationModeResumed, StartedAt: 3, CompletedAt: 4, DurationMS: 1, ExitStatus: "ok",
		ModelRoundtrips: intPtr(3), ToolCalls: intPtr(2), SubprocessWaitMS: int64Ptr(500),
	}); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
}

func TestOpenDropsAgentAttributionColumnsWithoutLosingInvocationFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "b", "h", "b")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	for _, statement := range []string{
		`ALTER TABLE agent_invocations ADD COLUMN invocation_mode TEXT NOT NULL DEFAULT 'harness_cli'`,
		`ALTER TABLE agent_invocations ADD COLUMN agent_observations_json TEXT`,
		`ALTER TABLE agent_invocations ADD COLUMN nested_agent_count INTEGER`,
		`ALTER TABLE agent_invocations ADD COLUMN pricing_receipt_json TEXT`,
	} {
		if _, err := d.sql.Exec(statement); err != nil {
			t.Fatalf("add legacy attribution column: %v", err)
		}
	}
	if _, err := d.sql.Exec(`INSERT INTO agent_invocations
		(id, run_id, step_name, round, purpose, agent, usage_coverage, invocation_mode, agent_observations_json, nested_agent_count, model, session_mode, session_key, started_at, completed_at, duration_ms, exit_status, failure_category, input_tokens, output_tokens, cache_read_tokens, reported_cost_usd, pricing_receipt_json)
		VALUES ('legacy-attribution', ?, 'review', 1, 'review', 'codex', 'complete', 'harness_cli', '[{"identity":"worker","invocation_mode":"subagent_tool"}]', 1, 'gpt-5.6-sol', 'cold', 'session-key', 1, 2, 1, 'ok', '', 100, 20, 50, 1.25, '{"api_list_estimate":{"value_usd":2.5}}')`, run.ID); err != nil {
		t.Fatalf("insert legacy invocation: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d.Close()

	got, err := d.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatalf("get after migration: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].UsageCoverage != agent.UsageCoverageComplete || got[0].Model != "gpt-5.6-sol" || got[0].InputTokens != 100 || got[0].OutputTokens != 20 || got[0].SessionKey != "session-key" || got[0].ReportedCostUSD == nil || *got[0].ReportedCostUSD != 1.25 {
		t.Fatalf("retained invocation facts = %+v", got[0])
	}
	rows, err := d.sql.Query(`SELECT name FROM pragma_table_info('agent_invocations')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, column)
	}
	for _, removed := range []string{"invocation_mode", "agent_observations_json", "nested_agent_count", "pricing_receipt_json"} {
		if containsString(columns, removed) {
			t.Fatalf("migration retained removed column %q: %v", removed, columns)
		}
	}
}
