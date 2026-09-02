package pipeline

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	_ "modernc.org/sqlite"
)

func TestExecutor_TerminalizesRunWhenInitialStatusWriteFails(t *testing.T) {
	database, p, run, repo := setupTest(t)
	installRunStatusFailureTrigger(t, p.DB(), string(types.RunRunning))
	events := &eventCollector{}
	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, events.handler)

	err := exec.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "update run status") {
		t.Fatalf("Execute() error = %v", err)
	}
	assertDurableFailedRunAndEvent(t, database, run.ID, events)
}

func TestExecutor_TerminalizesRunWhenFinalCompletedWriteFails(t *testing.T) {
	database, p, run, repo := setupTest(t)
	installRunStatusFailureTrigger(t, p.DB(), string(types.RunCompleted))
	events := &eventCollector{}
	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, events.handler)

	err := exec.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "update run status") {
		t.Fatalf("Execute() error = %v", err)
	}
	assertDurableFailedRunAndEvent(t, database, run.ID, events)
}

func TestExecutor_TerminalizesStepWhenRoundInsertFails(t *testing.T) {
	database, p, run, repo := setupTest(t)
	raw, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER reject_step_round_insert
		BEFORE INSERT ON step_rounds
		BEGIN
			SELECT RAISE(FAIL, 'injected round insert failure');
		END`); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
	err = exec.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "begin review round 1") || !strings.Contains(err.Error(), "injected round insert failure") {
		t.Fatalf("Execute() error = %v, want original round insert failure", err)
	}
	gotRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed", gotRun.Status)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusFailed || steps[0].Error == nil || !strings.Contains(*steps[0].Error, "injected round insert failure") {
		t.Fatalf("step result = %+v, want one failed step with original persistence error", steps)
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 0 {
		t.Fatalf("rounds = %+v, want no partially inserted round", rounds)
	}
}

func TestExecutor_TerminalizesStepAndRoundWhenRoundCompletionFails(t *testing.T) {
	database, p, run, repo := setupTest(t)
	raw, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER reject_step_round_completion
		BEFORE UPDATE OF status ON step_rounds
		WHEN NEW.status = 'completed'
		BEGIN
			SELECT RAISE(FAIL, 'injected round completion failure');
		END`); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
	err = exec.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "complete review round 1") || !strings.Contains(err.Error(), "injected round completion failure") {
		t.Fatalf("Execute() error = %v, want original round completion failure", err)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusFailed || steps[0].Error == nil || !strings.Contains(*steps[0].Error, "injected round completion failure") {
		t.Fatalf("step result = %+v, want failed completion", steps)
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 || rounds[0].Status != db.RoundStatusFailed {
		t.Fatalf("rounds = %+v, want failed active round", rounds)
	}
}

func installRunStatusFailureTrigger(t *testing.T, path, status string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, err = raw.Exec(`CREATE TRIGGER reject_test_run_status
		BEFORE UPDATE OF status ON runs
		WHEN NEW.status = '` + status + `'
		BEGIN
			SELECT RAISE(FAIL, 'injected status write failure');
		END`)
	if err != nil {
		t.Fatal(err)
	}
}

func assertDurableFailedRunAndEvent(t *testing.T, database *db.DB, runID string, events *eventCollector) {
	t.Helper()
	got, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunFailed {
		t.Fatalf("durable run status = %s, want failed", got.Status)
	}
	completed := events.findRunEvent(ipc.EventRunCompleted)
	if completed == nil || completed.Status == nil || *completed.Status != string(types.RunFailed) {
		t.Fatalf("terminal event = %+v, want failed run_completed", completed)
	}
}
