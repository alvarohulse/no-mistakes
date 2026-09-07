package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestOpenMigratesRoundDecisionSourcesAndPreservesDecisionReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "round-decisions.sqlite")
	before, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := before.InsertRepo("/tmp/round-decision-migration", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := before.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := before.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	round, err := before.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err != nil {
		t.Fatal(err)
	}
	if err := before.CompleteStepRoundStructured(round.ID, StepRoundEvaluation{
		Kind: RoundEvaluationInitialReview,
		Findings: []StepRoundFinding{{
			ExternalID: "review-1", Description: "needs a decision", Action: types.ActionAskUser,
		}},
	}, StructuredRoundSubject{}, nil, 1); err != nil {
		t.Fatal(err)
	}
	selected := `["review-1"]`
	if err := before.SetStepRoundSelection(round.ID, &selected, RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE round_decisions_legacy (
			id             TEXT PRIMARY KEY,
			run_id         TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
			round_id       TEXT NOT NULL UNIQUE REFERENCES step_rounds(id) ON DELETE CASCADE,
			source         TEXT NOT NULL CHECK (source IN ('user', 'auto_fix', 'user_declined')),
			explicit_empty INTEGER NOT NULL DEFAULT 0,
			created_at     INTEGER NOT NULL
		)`,
		`INSERT INTO round_decisions_legacy (id, run_id, round_id, source, explicit_empty, created_at)
			SELECT id, run_id, round_id, source, explicit_empty, created_at FROM round_decisions`,
		`DROP TABLE round_decisions`,
		`ALTER TABLE round_decisions_legacy RENAME TO round_decisions`,
		`DELETE FROM schema_migrations WHERE name = 'round_decision_sources_v1'`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			raw.Close()
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	preserved, err := after.GetRoundDecision(round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved == nil || preserved.Source != RoundSelectionSourceUser || len(preserved.Findings) != 1 || preserved.Findings[0].State != RoundDecisionFindingSelected {
		t.Fatalf("preserved decision = %#v", preserved)
	}

	skippedStep, err := after.InsertStepResult(run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	skippedRound, err := after.BeginStepRound(skippedStep.ID, 1, RoundTriggerInitial)
	if err != nil {
		t.Fatal(err)
	}
	if err := after.CompleteStepRoundStructured(skippedRound.ID, StepRoundEvaluation{
		Kind: RoundEvaluationValidation,
		Findings: []StepRoundFinding{{
			ExternalID: "build-1", Description: "needs a decision", Action: types.ActionAskUser,
		}},
	}, StructuredRoundSubject{}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := after.CompleteStepWithUserSkipDecision(skippedRound.ID, skippedStep.ID, run.ID, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	skippedDecision, err := after.GetRoundDecision(skippedRound.ID)
	if err != nil {
		t.Fatal(err)
	}
	if skippedDecision == nil || skippedDecision.Source != RoundSelectionSourceUserSkipped || !skippedDecision.ExplicitEmpty || len(skippedDecision.Findings) != 1 || skippedDecision.Findings[0].State != RoundDecisionFindingUnselected {
		t.Fatalf("migrated skip decision = %#v", skippedDecision)
	}
}
