package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRefreshOperationsRoundTripOrderedReferences(t *testing.T) {
	d := openTestDB(t)
	receipt, firstAttempt, secondAttempt, artifact := newRefreshOperationFixture(t, d)

	stored, err := d.InsertRefreshOperation(receipt)
	if err != nil {
		t.Fatalf("insert refresh operation: %v", err)
	}
	if stored.ID == "" || stored.RunID != receipt.RunID || stored.Kind != OperationKindRefresh {
		t.Fatalf("stored operation identity = %+v", stored)
	}
	if stored.Strategy != types.RefreshStrategyMerge || stored.SourceRef != "refs/heads/feature" || stored.DestinationRef != "refs/remotes/origin/main" || stored.AuthoritativeBaseRef != "refs/remotes/origin/main" || stored.AuthoritativeBaseSHA != "authoritative-base" || stored.StartingHeadSHA != "starting-head" || stored.ResultingHeadSHA != "resulting-head" || stored.Decision != RefreshDecisionMerged || stored.ConflictState != RefreshConflictStateNone || stored.RepairState != RefreshRepairStateNotNeeded {
		t.Fatalf("stored receipt fields = %+v", stored)
	}
	if stored.StartedAt != 100 || stored.CompletedAt != 145 || stored.DurationMS != 45 {
		t.Fatalf("stored timing = %+v", stored)
	}
	if stored.DiagnosticArtifactID == nil || *stored.DiagnosticArtifactID != artifact.ID {
		t.Fatalf("stored diagnostic artifact = %+v", stored.DiagnosticArtifactID)
	}
	if got := strings.Join(stored.CommandAttemptIDs, ","); got != secondAttempt.ID+","+firstAttempt.ID {
		t.Fatalf("stored command attempt order = %q", got)
	}

	operations, err := d.GetRefreshOperationsByRun(receipt.RunID)
	if err != nil {
		t.Fatalf("get refresh operations: %v", err)
	}
	if len(operations) != 1 || operations[0].ID != stored.ID || strings.Join(operations[0].CommandAttemptIDs, ",") != secondAttempt.ID+","+firstAttempt.ID {
		t.Fatalf("round-tripped operations = %+v", operations)
	}

	if _, err := d.sql.Exec(`DELETE FROM runs WHERE id = ?`, receipt.RunID); err != nil {
		t.Fatalf("delete run with refresh receipt: %v", err)
	}
	for _, table := range []string{"operations", "operation_command_attempts", "artifacts"} {
		var count int
		if err := d.sql.QueryRow(`SELECT count(*) FROM `+table+` WHERE run_id = ?`, receipt.RunID).Scan(&count); err != nil {
			t.Fatalf("count %s after run delete: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s rows after run delete = %d, want 0", table, count)
		}
	}
	var refreshCount int
	if err := d.sql.QueryRow(`SELECT count(*) FROM refresh_operations WHERE operation_id = ?`, stored.ID).Scan(&refreshCount); err != nil {
		t.Fatalf("count refresh operations after run delete: %v", err)
	}
	if refreshCount != 0 {
		t.Fatalf("refresh operation rows after run delete = %d, want 0", refreshCount)
	}
}

func TestInsertRefreshOperationRejectsInvalidReferencesAndDecisions(t *testing.T) {
	d := openTestDB(t)
	receipt, firstAttempt, _, _ := newRefreshOperationFixture(t, d)

	foreignRun, err := d.InsertRun(mustRepoForRun(t, d, receipt.RunID).ID, "other", "other-head", "other-base")
	if err != nil {
		t.Fatal(err)
	}
	foreignStep, err := d.InsertStepResult(foreignRun.ID, types.StepRefresh)
	if err != nil {
		t.Fatal(err)
	}
	foreignRound, err := d.InsertStepRound(foreignStep.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	foreignDefinition, err := d.EnsureCommandDefinition(foreignRun.ID, refreshCommandDefinition())
	if err != nil {
		t.Fatal(err)
	}
	foreignAttempt, err := d.StartCommandAttempt(CommandAttempt{
		RunID: foreignRun.ID, CommandID: foreignDefinition.ID, StepID: foreignStep.ID, RoundID: foreignRound.ID,
		Sequence: 1, Purpose: "refresh", Observer: CommandObserverController, Trigger: "initial", BeforeSHA: "other-head",
		InputStateID: stringPointer("git:other-head"), CommandSource: runner.SourceBase, RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDefault,
	})
	if err != nil {
		t.Fatal(err)
	}
	foreignArtifact, err := d.RegisterArtifact(testEvidenceArtifact(filepath.ToSlash(filepath.Join(foreignRun.ID, "evidence", "refresh.txt")), foreignRun.ID, foreignStep.ID, foreignRound.ID))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		mutate  func(*RefreshOperation)
		wantErr string
	}{
		{
			name: "foreign command attempt",
			mutate: func(operation *RefreshOperation) {
				operation.CommandAttemptIDs = []string{foreignAttempt.ID}
			},
			wantErr: "command attempt",
		},
		{
			name: "duplicate command attempt",
			mutate: func(operation *RefreshOperation) {
				operation.CommandAttemptIDs = []string{firstAttempt.ID, firstAttempt.ID}
			},
			wantErr: "unique",
		},
		{
			name: "foreign diagnostic artifact",
			mutate: func(operation *RefreshOperation) {
				operation.DiagnosticArtifactID = &foreignArtifact.ID
			},
			wantErr: "diagnostic artifact",
		},
		{
			name: "unsupported decision",
			mutate: func(operation *RefreshOperation) {
				operation.Decision = "guessed"
			},
			wantErr: "decision",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := receipt
			candidate.CommandAttemptIDs = append([]string(nil), receipt.CommandAttemptIDs...)
			tt.mutate(&candidate)
			if _, err := d.InsertRefreshOperation(candidate); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("insert error = %v, want %q", err, tt.wantErr)
			}
		})
	}

	operations, err := d.GetRefreshOperationsByRun(receipt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 0 {
		t.Fatalf("rejected operations persisted = %+v", operations)
	}
}

func TestInsertRefreshOperationRequiresOperationDiagnosticArtifact(t *testing.T) {
	d := openTestDB(t)
	receipt, _, _, artifact := newRefreshOperationFixture(t, d)

	if _, err := d.sql.Exec(
		`UPDATE artifacts SET purpose = ?, kind = ? WHERE id = ?`,
		ArtifactPurposeTestEvidence,
		"evidence-file",
		artifact.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertRefreshOperation(receipt); err == nil || !strings.Contains(err.Error(), "diagnostic artifact") {
		t.Fatalf("insert error = %v, want diagnostic artifact rejection", err)
	}
}

func TestOpenAddsOperationsWithoutFabricatingLegacyRefreshReceipts(t *testing.T) {
	d := openPreOperationsTestDB(t)
	for _, table := range []string{"operations", "refresh_operations", "operation_command_attempts"} {
		var count int
		if err := d.sql.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("%s missing after migration: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("legacy %s rows = %d, want 0", table, count)
		}
	}
	operations, err := d.GetRefreshOperationsByRun("run")
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 0 {
		t.Fatalf("legacy refresh operations = %+v, want none", operations)
	}
}

func TestOperationsSchemaReservesPushDiscriminator(t *testing.T) {
	for _, tt := range []struct {
		name  string
		open  func(*testing.T) *DB
		runID string
	}{
		{name: "fresh", open: openTestDB},
		{name: "migrated", open: openPreOperationsTestDB, runID: "run"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := tt.open(t)
			runID := tt.runID
			if runID == "" {
				repo, err := d.InsertRepo(filepath.Join("/tmp", "push-operation-"+tt.name), "upstream", "main")
				if err != nil {
					t.Fatal(err)
				}
				run, err := d.InsertRun(repo.ID, "feature", "head", "base")
				if err != nil {
					t.Fatal(err)
				}
				runID = run.ID
			}
			stepID, roundID := insertPushOperationScope(t, d, runID)

			if _, err := d.sql.Exec(
				`INSERT INTO operations (id, run_id, kind, step_id, round_id, started_at, completed_at, duration_ms)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				"push-header", runID, OperationKindPush, stepID, roundID, 100, 120, 20,
			); err != nil {
				t.Fatalf("insert reserved push operation header: %v", err)
			}
		})
	}
}

func openPreOperationsTestDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE repos (id TEXT PRIMARY KEY, working_path TEXT NOT NULL UNIQUE, upstream_url TEXT NOT NULL, default_branch TEXT NOT NULL DEFAULT 'main', created_at INTEGER NOT NULL);
		CREATE TABLE runs (id TEXT PRIMARY KEY, repo_id TEXT NOT NULL, branch TEXT NOT NULL, head_sha TEXT NOT NULL, base_sha TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
		INSERT INTO repos VALUES ('repo', '/tmp/legacy-operations', 'upstream', 'main', 1);
		INSERT INTO runs VALUES ('run', 'repo', 'feature', 'head', 'base', 'completed', 1, 1);
	`); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func insertPushOperationScope(t *testing.T, d *DB, runID string) (string, string) {
	t.Helper()
	const stepID = "push-step"
	const roundID = "push-round"
	if _, err := d.sql.Exec(
		`INSERT INTO step_results (id, run_id, step_name, step_order, status) VALUES (?, ?, ?, ?, ?)`,
		stepID, runID, types.StepPush, 7, types.StepStatusPending,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(
		`INSERT INTO step_rounds (id, step_result_id, round, trigger_type, duration_ms, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		roundID, stepID, 1, "initial", 0, 1,
	); err != nil {
		t.Fatal(err)
	}
	return stepID, roundID
}

func newRefreshOperationFixture(t *testing.T, d *DB) (RefreshOperation, *CommandAttempt, *CommandAttempt, *Artifact) {
	t.Helper()
	repo, err := d.InsertRepo("/home/user/refresh-operation", "git@github.com:user/refresh-operation.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "starting-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepRefresh)
	if err != nil {
		t.Fatal(err)
	}
	round, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := d.EnsureCommandDefinition(run.ID, refreshCommandDefinition())
	if err != nil {
		t.Fatal(err)
	}
	startAttempt := func(sequence int) *CommandAttempt {
		t.Helper()
		attempt, err := d.StartCommandAttempt(CommandAttempt{
			RunID: run.ID, CommandID: definition.ID, StepID: step.ID, RoundID: round.ID,
			Sequence: sequence, Purpose: "refresh", Observer: CommandObserverController, Trigger: "initial", BeforeSHA: "starting-head",
			InputStateID: stringPointer("git:starting-head"), CommandSource: runner.SourceBase, RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDefault,
		})
		if err != nil {
			t.Fatal(err)
		}
		return attempt
	}
	firstAttempt := startAttempt(1)
	secondAttempt := startAttempt(2)
	artifact, err := d.RegisterArtifact(operationDiagnosticArtifact(filepath.ToSlash(filepath.Join(run.ID, "diagnostics", "refresh.txt")), run.ID, step.ID, round.ID))
	if err != nil {
		t.Fatal(err)
	}
	return RefreshOperation{
		RunID:                run.ID,
		StepID:               step.ID,
		RoundID:              round.ID,
		Strategy:             types.RefreshStrategyMerge,
		SourceRef:            "refs/heads/feature",
		DestinationRef:       "refs/remotes/origin/main",
		AuthoritativeBaseRef: "refs/remotes/origin/main",
		AuthoritativeBaseSHA: "authoritative-base",
		StartingHeadSHA:      "starting-head",
		Decision:             RefreshDecisionMerged,
		ResultingHeadSHA:     "resulting-head",
		ConflictState:        RefreshConflictStateNone,
		RepairState:          RefreshRepairStateNotNeeded,
		CommandAttemptIDs:    []string{secondAttempt.ID, firstAttempt.ID},
		StartedAt:            100,
		CompletedAt:          145,
		DurationMS:           45,
		DiagnosticArtifactID: &artifact.ID,
	}, firstAttempt, secondAttempt, artifact
}

func refreshCommandDefinition() runner.Resolved {
	return runner.Resolved{
		Script:        "git merge --no-edit refs/remotes/origin/main",
		CommandSource: runner.SourceBase,
		Provenance: runner.Provenance{
			SchemaVersion: runner.SchemaVersion,
			Platform:      "linux",
			Source:        runner.SourceDefault,
			Executable:    "sh",
			Args:          []string{"-c"},
		},
	}
}

func operationDiagnosticArtifact(relativePath, runID, stepID, roundID string) Artifact {
	artifact := testEvidenceArtifact(relativePath, runID, stepID, roundID)
	artifact.Purpose = ArtifactPurposeOperationDiagnostic
	artifact.Kind = ArtifactKindOperationDiagnostic
	artifact.Label = "Operation diagnostic"
	artifact.StorageRoot = ArtifactStorageRootRun
	artifact.RelativePath = filepath.ToSlash(filepath.Join(runID, "diagnostics", filepath.Base(relativePath)))
	return artifact
}

func mustRepoForRun(t *testing.T, d *DB, runID string) *Repo {
	t.Helper()
	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepo(run.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}
