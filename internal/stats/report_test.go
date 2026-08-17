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
	if len(report.Runs.Items) != 1 || len(report.Steps) != 1 || len(report.Agents) != 1 || len(report.Repairs) != 1 || len(report.Costs.Items) != 1 {
		t.Fatalf("report sections = runs %d steps %d agents %d repairs %d costs %d", len(report.Runs.Items), len(report.Steps), len(report.Agents), len(report.Repairs), len(report.Costs.Items))
	}
	if report.Steps[0].Step.Name != types.StepReview || report.Agents[0].Invocation.Agent != "codex" || report.Costs.Items[0].InvocationID != report.Agents[0].Invocation.ID {
		t.Fatalf("filtered report = %+v", report)
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

func timePointer(value time.Time) *time.Time { return &value }
