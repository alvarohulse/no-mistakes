package stats

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestBuildReportUsesHalfOpenRunCreatedWindow(t *testing.T) {
	database, run := newAuditRun(t)
	created := time.Unix(run.CreatedAt, 0).UTC()

	included, err := BuildReport(database, Query{Since: &created, Until: timePointer(created.Add(time.Second))}, created.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(included.Runs.Items) != 1 || included.Runs.Items[0].ID != run.ID {
		t.Fatalf("included runs = %+v, want %s", included.Runs.Items, run.ID)
	}

	excluded, err := BuildReport(database, Query{Until: &created}, created.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(excluded.Runs.Items) != 0 {
		t.Fatalf("until boundary included run: %+v", excluded.Runs.Items)
	}
}

func TestBuildReportCombinesAllDirectFiltersAndSections(t *testing.T) {
	database, run := newAuditRun(t)
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(review.ID, 1, "auto_fix", nil, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(review.ID, types.StepStatusCompleted, 0, 10, ""); err != nil {
		t.Fatal(err)
	}
	build, err := database.InsertStepResult(run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(build.ID, types.StepStatusCompleted, 0, 10, ""); err != nil {
		t.Fatal(err)
	}
	openAI := "openai"
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: "codex",
		Model: "gpt-5.6-sol", ModelProvider: &openAI, SessionMode: db.InvocationModeCold,
		StartedAt: run.CreatedAt, CompletedAt: run.CreatedAt + 1, ExitStatus: "ok",
	})
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: string(types.StepBuild), Round: 1, Purpose: "build", Agent: "cursor",
		Model: "gpt-5.6-luna", ModelProvider: &openAI, SessionMode: db.InvocationModeCold,
		StartedAt: run.CreatedAt + 2, CompletedAt: run.CreatedAt + 3, ExitStatus: "ok",
	})

	report, err := BuildReport(database, Query{
		RepoIDs: []string{run.RepoID}, Steps: []types.StepName{types.StepReview}, Agents: []string{"codex"},
		Models: []string{"gpt-5.6-sol"}, Purposes: []string{"review"}, Statuses: []types.RunStatus{types.RunCompleted},
	}, time.Unix(run.CreatedAt+60, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Runs.Items) != 1 || len(report.Steps) != 1 || len(report.Agents) != 1 || len(report.Repairs) != 1 || len(report.Metrics.Items) != 1 || len(report.Costs.Items) != 1 {
		t.Fatalf("report sections = runs %d steps %d agents %d repairs %d metrics %d costs %d", len(report.Runs.Items), len(report.Steps), len(report.Agents), len(report.Repairs), len(report.Metrics.Items), len(report.Costs.Items))
	}
	if report.Steps[0].Step.Name != types.StepReview || report.Agents[0].Invocation.Agent != "codex" || report.Costs.Items[0].InvocationID != report.Agents[0].Invocation.ID {
		t.Fatalf("filtered report = %+v", report)
	}
}

func TestBuildReportScopesDashboardAndAgentAggregatesToSelectedFacts(t *testing.T) {
	database, run := newAuditRun(t)
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	reviewInitial := `{"findings":[{"id":"r1","severity":"warning","description":"one","action":"auto-fix"},{"id":"r2","severity":"warning","description":"two","action":"auto-fix"}],"summary":"two","risk_level":"medium","risk_rationale":"test"}`
	reviewFinal := `{"findings":[{"id":"r2","severity":"warning","description":"two","action":"ask-user"}],"summary":"one","risk_level":"medium","risk_rationale":"test"}`
	if _, err := database.InsertStepRound(review.ID, 1, "initial", &reviewInitial, nil, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(review.ID, 2, "auto_fix", &reviewFinal, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(review.ID, types.StepStatusCompleted, 0, 20, ""); err != nil {
		t.Fatal(err)
	}
	build, err := database.InsertStepResult(run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	buildInitial := `{"findings":[{"id":"b1","severity":"error","description":"build","action":"auto-fix"}],"summary":"one","risk_level":"low","risk_rationale":"test"}`
	if _, err := database.InsertStepRound(build.ID, 1, "initial", &buildInitial, nil, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(build.ID, 2, "auto_fix", nil, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(build.ID, types.StepStatusCompleted, 0, 20, ""); err != nil {
		t.Fatal(err)
	}
	reviewInput, buildInput := 10, 20
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: "codex",
		SessionMode: db.InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 100, ExitStatus: "ok",
		InputTokens: reviewInput, DeltaInputTokens: &reviewInput,
	})
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: string(types.StepBuild), Round: 1, Purpose: "build", Agent: "cursor",
		SessionMode: db.InvocationModeStarted, StartedAt: 3, CompletedAt: 4, DurationMS: 200, ExitStatus: "ok",
		InputTokens: buildInput, DeltaInputTokens: &buildInput,
	})

	report, err := BuildReport(database, Query{Steps: []types.StepName{types.StepReview}, Purposes: []string{"review"}}, time.Unix(run.CreatedAt+60, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if report.Dashboard.TotalRepos != 1 || report.Dashboard.TotalRuns != 1 || report.Dashboard.RescueRuns != 1 ||
		report.Dashboard.ReportedFindings != 2 || report.Dashboard.FixedFindings != 1 {
		t.Fatalf("filtered dashboard = %+v", report.Dashboard)
	}
	if len(report.Dashboard.Steps) != 1 || report.Dashboard.Steps[0].Step != types.StepReview || report.Dashboard.Steps[0].FixedFindings != 1 {
		t.Fatalf("filtered dashboard steps = %+v", report.Dashboard.Steps)
	}
	if len(report.AgentAggregates) != 1 || report.AgentAggregates[0].Purpose != "review" || report.AgentAggregates[0].Count != 1 ||
		report.AgentAggregates[0].InputTokens == nil || *report.AgentAggregates[0].InputTokens != int64(reviewInput) {
		t.Fatalf("filtered agent aggregates = %+v", report.AgentAggregates)
	}
	if report.Dashboard.RescueRate.Value == nil || *report.Dashboard.RescueRate.Value != 1 ||
		report.Dashboard.FixRate.Value == nil || *report.Dashboard.FixRate.Value != 0.5 {
		t.Fatalf("filtered dashboard rates = rescue %+v fix %+v", report.Dashboard.RescueRate, report.Dashboard.FixRate)
	}
}

func TestBuildReportKeepsIncompleteAgentAggregateMetersUnknown(t *testing.T) {
	database, run := newAuditRun(t)
	knownInput, zero := 10, 0
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: "codex",
		SessionMode: db.InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 100, ExitStatus: "ok",
		InputTokens: knownInput, DeltaInputTokens: &knownInput, ModelRoundtrips: &zero,
	})
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: string(types.StepReview), Round: 2, Purpose: "review", Agent: "codex",
		SessionMode: db.InvocationModeResumed, StartedAt: 3, CompletedAt: 4, DurationMS: 200, ExitStatus: "ok",
	})

	report, err := BuildReport(database, Query{}, time.Unix(run.CreatedAt+60, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.AgentAggregates) != 1 {
		t.Fatalf("agent aggregates = %+v", report.AgentAggregates)
	}
	aggregate := report.AgentAggregates[0]
	if aggregate.InputTokens != nil || aggregate.ModelRoundtrips != nil {
		t.Fatalf("partial aggregate meters became totals: %+v", aggregate)
	}
	if aggregate.MetricsRows != 1 || aggregate.Count != 2 {
		t.Fatalf("aggregate coverage = %d/%d, want 1/2", aggregate.MetricsRows, aggregate.Count)
	}
	if aggregate.Coverage.InputTokens != (Coverage{Reported: 1, Total: 2}) || aggregate.Coverage.ModelRoundtrips != (Coverage{Reported: 1, Total: 2}) {
		t.Fatalf("per-meter aggregate coverage = %+v", aggregate.Coverage)
	}
}

func TestBuildReportAgentAggregatesDoNotDoubleCountCumulativeSessionMeters(t *testing.T) {
	database, run := newAuditRun(t)
	r1Input, r1CacheRead, r1CacheWrite, r1Fresh, r1Reasoning := 1000, 600, 20, 380, 5
	r2Input, r2CacheRead, r2CacheWrite, r2Fresh, r2Reasoning := 1500, 1200, 50, 630, 10
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: "codex",
		SessionMode: db.InvocationModeStarted, StartedAt: 1, CompletedAt: 2, DurationMS: 100, ExitStatus: "ok",
		DeltaInputTokens: &r1Input, DeltaCacheReadTokens: &r1CacheRead, DeltaCacheCreationTokens: &r1CacheWrite,
		FreshInputTokens: &r1Fresh, ReasoningTokens: &r1Reasoning,
	})
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: string(types.StepReview), Round: 2, Purpose: "review", Agent: "codex",
		SessionMode: db.InvocationModeResumed, StartedAt: 3, CompletedAt: 4, DurationMS: 100, ExitStatus: "ok",
		DeltaInputTokens: &r2Input, DeltaCacheReadTokens: &r2CacheRead, DeltaCacheCreationTokens: &r2CacheWrite,
		FreshInputTokens: &r2Fresh, ReasoningTokens: &r2Reasoning,
	})

	report, err := BuildReport(database, Query{}, time.Unix(run.CreatedAt+60, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.AgentAggregates) != 1 {
		t.Fatalf("agent aggregates = %+v", report.AgentAggregates)
	}
	aggregate := report.AgentAggregates[0]
	if aggregate.FreshInputTokens == nil || *aggregate.FreshInputTokens != 630 {
		t.Fatalf("fresh input aggregate = %v, want per-round total 630", aggregate.FreshInputTokens)
	}
	if aggregate.ReasoningTokens != nil || aggregate.Coverage.ReasoningTokens != (Coverage{Reported: 1, Total: 2}) {
		t.Fatalf("cumulative reasoning aggregate should remain unknown: %+v", aggregate)
	}
}

func TestBuildDashboardUsesRepositoryIDAsDeterministicFinalTieBreaker(t *testing.T) {
	runs := []dashboardRun{
		{identity: RunIdentity{ID: "run-b", RepoID: "repo-b"}},
		{identity: RunIdentity{ID: "run-a", RepoID: "repo-a"}},
	}
	displayNames := map[string]string{"repo-a": "shared", "repo-b": "shared"}
	for iteration := 0; iteration < 100; iteration++ {
		dashboard := buildDashboard(runs, displayNames, false)
		if len(dashboard.Repositories) != 2 || dashboard.Repositories[0].RepoID != "repo-a" || dashboard.Repositories[1].RepoID != "repo-b" {
			t.Fatalf("dashboard repository order = %+v", dashboard.Repositories)
		}
	}
}

func TestBuildReportRejectsAnEmptyOrReversedWindow(t *testing.T) {
	database, _ := newAuditRun(t)
	boundary := time.Now().UTC()
	for _, query := range []Query{{Since: &boundary, Until: &boundary}, {Since: timePointer(boundary.Add(time.Second)), Until: &boundary}} {
		if _, err := BuildReport(database, query, boundary); err == nil {
			t.Fatalf("BuildReport accepted window %+v", query)
		}
	}
}

func TestBuildReportRejectsBlankInvocationSelectors(t *testing.T) {
	database, _ := newAuditRun(t)
	for name, query := range map[string]Query{
		"agent": {Agents: []string{" "}}, "model": {Models: []string{" "}}, "purpose": {Purposes: []string{" "}},
	} {
		if _, err := BuildReport(database, query, time.Now().UTC()); err == nil {
			t.Fatalf("BuildReport accepted blank %s selector", name)
		}
	}
}

func TestBuildReportIncludesRepairDecisionOnInitialRound(t *testing.T) {
	database, run := newAuditRun(t)
	step, err := database.InsertStepResult(run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	round, err := database.InsertStepRound(step.ID, 1, "initial", nil, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepRoundRepairAudit(round.ID, "sha256:attempted", "attempted"); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 10, ""); err != nil {
		t.Fatal(err)
	}

	report, err := BuildReport(database, Query{RunID: run.ID}, time.Unix(run.CreatedAt+1, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Repairs) != 1 || report.Repairs[0].Trigger != "initial" || report.Repairs[0].RepairResult == nil || *report.Repairs[0].RepairResult != "attempted" {
		t.Fatalf("initial repair decision = %+v", report.Repairs)
	}
}

func timePointer(value time.Time) *time.Time { return &value }
