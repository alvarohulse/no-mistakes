package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestUserTerminalDecisionRollsBackWithStepTransition(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		status   types.StepStatus
		trigger  string
		complete func(*DB, string, string, string) error
	}{
		{
			name:   "skip rolls back when decision insert fails",
			source: RoundSelectionSourceUserSkipped,
			status: types.StepStatusSkipped,
			trigger: `CREATE TRIGGER reject_skip_decision
				BEFORE INSERT ON round_decisions
				BEGIN
					SELECT RAISE(FAIL, 'injected skip decision failure');
				END`,
			complete: func(database *DB, roundID, stepID, runID string) error {
				return database.CompleteStepWithUserSkipDecision(roundID, stepID, runID, 0, 1, "")
			},
		},
		{
			name:   "abort rolls back when terminal step fails",
			source: RoundSelectionSourceUserAborted,
			status: types.StepStatusFailed,
			trigger: `CREATE TRIGGER reject_abort_step
				BEFORE UPDATE OF status ON step_results
				WHEN NEW.status = 'failed'
				BEGIN
					SELECT RAISE(FAIL, 'injected abort step failure');
				END`,
			complete: func(database *DB, roundID, stepID, runID string) error {
				return database.FailStepWithUserAbortDecision(roundID, stepID, runID, 1)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openTestDB(t)
			repo, err := database.InsertRepo("/tmp/terminal-decision", "https://example.com/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "feature", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			step, err := database.InsertStepResult(run.ID, types.StepBuild)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			round, err := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.CompleteStepRoundStructured(round.ID, StepRoundEvaluation{
				Kind: RoundEvaluationValidation,
				Findings: []StepRoundFinding{{
					ExternalID: "build-1", Description: "needs a decision", Action: types.ActionAskUser,
				}},
			}, StructuredRoundSubject{}, nil, 1); err != nil {
				t.Fatal(err)
			}

			raw, err := sql.Open("sqlite", databasePath(t, database)+"?_pragma=busy_timeout(5000)")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(test.trigger); err != nil {
				raw.Close()
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			err = test.complete(database, round.ID, step.ID, run.ID)
			if err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("terminal decision error = %v, want injected transaction failure", err)
			}
			persistedStep, err := database.GetStepResult(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persistedStep.Status == test.status {
				t.Fatalf("step after rollback = %#v, unexpectedly reached %s", persistedStep, test.status)
			}
			decision, err := database.GetRoundDecision(round.ID)
			if err != nil {
				t.Fatal(err)
			}
			if decision != nil {
				t.Fatalf("decision after rollback = %#v", decision)
			}
		})
	}
}

func databasePath(t *testing.T, database *DB) string {
	t.Helper()
	var path string
	if err := database.sql.QueryRow(`PRAGMA database_list`).Scan(new(int), new(string), &path); err != nil {
		t.Fatal(err)
	}
	return path
}
