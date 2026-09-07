package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

type OperationKind string

const (
	OperationKindRefresh OperationKind = "refresh"
	OperationKindPush    OperationKind = "push"
)

type RefreshDecision string

const (
	RefreshDecisionSkipped       RefreshDecision = "skipped"
	RefreshDecisionFastForwarded RefreshDecision = "fast-forwarded"
	RefreshDecisionRebased       RefreshDecision = "rebased"
	RefreshDecisionMerged        RefreshDecision = "merged"
	RefreshDecisionConflicted    RefreshDecision = "conflicted"
	RefreshDecisionRepaired      RefreshDecision = "repaired"
	RefreshDecisionRefused       RefreshDecision = "refused"
	RefreshDecisionError         RefreshDecision = "error"
)

type RefreshConflictState string

const (
	RefreshConflictStateNone     RefreshConflictState = "none"
	RefreshConflictStateDetected RefreshConflictState = "detected"
	RefreshConflictStateResolved RefreshConflictState = "resolved"
)

type RefreshRepairState string

const (
	RefreshRepairStateNotNeeded    RefreshRepairState = "not_needed"
	RefreshRepairStateNotAttempted RefreshRepairState = "not_attempted"
	RefreshRepairStateSucceeded    RefreshRepairState = "succeeded"
	RefreshRepairStateFailed       RefreshRepairState = "failed"
)

// RefreshOperation records one exact Refresh decision and the command attempts
// that produced it. The generic operations collection owns identity, timing,
// and shared references; this type owns only the Refresh-specific facts.
type RefreshOperation struct {
	ID                   string
	RunID                string
	Kind                 OperationKind
	StepID               string
	RoundID              string
	Strategy             types.RefreshStrategy
	SourceRef            string
	DestinationRef       string
	AuthoritativeBaseRef string
	AuthoritativeBaseSHA *string
	StartingHeadSHA      string
	Decision             RefreshDecision
	ResultingHeadSHA     string
	ConflictState        RefreshConflictState
	RepairState          RefreshRepairState
	CommandAttemptIDs    []string
	StartedAt            int64
	CompletedAt          int64
	DurationMS           int64
	DiagnosticArtifactID *string
}

// InsertRefreshOperation atomically records a completed Refresh receipt and
// its ordered command-attempt references. The database assigns the stable
// operation ID so callers cannot accidentally repurpose an existing receipt.
func (d *DB) InsertRefreshOperation(operation RefreshOperation) (*RefreshOperation, error) {
	if operation.ID != "" {
		return nil, fmt.Errorf("insert refresh operation: ID is assigned by the database")
	}
	if operation.Kind != "" && operation.Kind != OperationKindRefresh {
		return nil, fmt.Errorf("insert refresh operation: kind must be %q", OperationKindRefresh)
	}
	operation.Kind = OperationKindRefresh
	strategy, err := types.ParseRefreshStrategy(string(operation.Strategy))
	if err != nil || strategy == "" {
		return nil, fmt.Errorf("insert refresh operation: unsupported strategy %q", operation.Strategy)
	}
	operation.Strategy = strategy

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("insert refresh operation: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := validateRefreshOperation(tx, operation); err != nil {
		return nil, err
	}

	operation.ID = newID()
	if _, err := tx.Exec(
		`INSERT INTO operations
		 (id, run_id, kind, step_id, round_id, started_at, completed_at, duration_ms, diagnostic_artifact_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		operation.ID, operation.RunID, operation.Kind, operation.StepID, operation.RoundID,
		operation.StartedAt, operation.CompletedAt, operation.DurationMS, operation.DiagnosticArtifactID,
	); err != nil {
		return nil, fmt.Errorf("insert refresh operation: insert operation: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO refresh_operations
		 (operation_id, strategy, source_ref, destination_ref, authoritative_base_ref, authoritative_base_sha,
		  starting_head_sha, decision, resulting_head_sha, conflict_state, repair_state)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		operation.ID, operation.Strategy, operation.SourceRef, operation.DestinationRef,
		operation.AuthoritativeBaseRef, operation.AuthoritativeBaseSHA, operation.StartingHeadSHA,
		operation.Decision, operation.ResultingHeadSHA, operation.ConflictState, operation.RepairState,
	); err != nil {
		return nil, fmt.Errorf("insert refresh operation: insert refresh receipt: %w", err)
	}
	for index, attemptID := range operation.CommandAttemptIDs {
		if _, err := tx.Exec(
			`INSERT INTO operation_command_attempts (operation_id, run_id, attempt_id, sequence) VALUES (?, ?, ?, ?)`,
			operation.ID, operation.RunID, attemptID, index+1,
		); err != nil {
			return nil, fmt.Errorf("insert refresh operation: link command attempt: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("insert refresh operation: commit: %w", err)
	}
	return &operation, nil
}

type operationQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func validateRefreshOperation(q operationQuerier, operation RefreshOperation) error {
	if strings.TrimSpace(operation.RunID) == "" || strings.TrimSpace(operation.StepID) == "" || strings.TrimSpace(operation.RoundID) == "" ||
		strings.TrimSpace(operation.SourceRef) == "" || strings.TrimSpace(operation.DestinationRef) == "" ||
		strings.TrimSpace(operation.AuthoritativeBaseRef) == "" ||
		strings.TrimSpace(operation.StartingHeadSHA) == "" || strings.TrimSpace(operation.ResultingHeadSHA) == "" {
		return fmt.Errorf("insert refresh operation: required receipt identity is incomplete")
	}
	if operation.Strategy != types.RefreshStrategyRebase && operation.Strategy != types.RefreshStrategyMerge {
		return fmt.Errorf("insert refresh operation: unsupported strategy %q", operation.Strategy)
	}
	if !validRefreshDecision(operation.Decision) {
		return fmt.Errorf("insert refresh operation: unsupported decision %q", operation.Decision)
	}
	if operation.AuthoritativeBaseSHA == nil {
		if operation.Decision != RefreshDecisionRefused && operation.Decision != RefreshDecisionError {
			return fmt.Errorf("insert refresh operation: authoritative base SHA is required for decision %q", operation.Decision)
		}
	} else if strings.TrimSpace(*operation.AuthoritativeBaseSHA) == "" {
		return fmt.Errorf("insert refresh operation: authoritative base SHA is empty")
	}
	if !validRefreshConflictState(operation.ConflictState) {
		return fmt.Errorf("insert refresh operation: unsupported conflict state %q", operation.ConflictState)
	}
	if !validRefreshRepairState(operation.RepairState) {
		return fmt.Errorf("insert refresh operation: unsupported repair state %q", operation.RepairState)
	}
	if operation.StartedAt <= 0 || operation.CompletedAt < operation.StartedAt || operation.DurationMS < 0 {
		return fmt.Errorf("insert refresh operation: timing is invalid")
	}

	var receiptOwnerCount int
	if err := q.QueryRow(
		`SELECT count(*)
		 FROM step_rounds r
		 JOIN step_results s ON s.id = r.step_result_id
		 WHERE r.id = ? AND s.id = ? AND s.run_id = ? AND s.step_name = ?`,
		operation.RoundID, operation.StepID, operation.RunID, types.StepRefresh,
	).Scan(&receiptOwnerCount); err != nil {
		return fmt.Errorf("insert refresh operation: validate step and round: %w", err)
	}
	if receiptOwnerCount != 1 {
		return fmt.Errorf("insert refresh operation: refresh step and round do not belong to run")
	}

	seenAttempts := make(map[string]struct{}, len(operation.CommandAttemptIDs))
	for _, attemptID := range operation.CommandAttemptIDs {
		if strings.TrimSpace(attemptID) == "" {
			return fmt.Errorf("insert refresh operation: command attempt reference is empty")
		}
		if _, exists := seenAttempts[attemptID]; exists {
			return fmt.Errorf("insert refresh operation: command attempt references must be unique")
		}
		seenAttempts[attemptID] = struct{}{}
		var attemptOwnerCount int
		if err := q.QueryRow(
			`SELECT count(*) FROM command_attempts
			 WHERE id = ? AND run_id = ? AND step_id = ? AND round_id = ?`,
			attemptID, operation.RunID, operation.StepID, operation.RoundID,
		).Scan(&attemptOwnerCount); err != nil {
			return fmt.Errorf("insert refresh operation: validate command attempt: %w", err)
		}
		if attemptOwnerCount != 1 {
			return fmt.Errorf("insert refresh operation: command attempt does not belong to refresh receipt")
		}
	}

	if operation.DiagnosticArtifactID == nil {
		return nil
	}
	if strings.TrimSpace(*operation.DiagnosticArtifactID) == "" {
		return fmt.Errorf("insert refresh operation: diagnostic artifact reference is empty")
	}
	var artifactOwnerCount int
	if err := q.QueryRow(
		`SELECT count(*) FROM artifacts
		 WHERE id = ? AND run_id = ? AND step_id = ? AND round_id = ?
		   AND purpose = ? AND kind = ? AND storage_root = ? AND command_attempt_id IS NULL`,
		*operation.DiagnosticArtifactID, operation.RunID, operation.StepID, operation.RoundID,
		ArtifactPurposeOperationDiagnostic, ArtifactKindOperationDiagnostic, ArtifactStorageRootRun,
	).Scan(&artifactOwnerCount); err != nil {
		return fmt.Errorf("insert refresh operation: validate diagnostic artifact: %w", err)
	}
	if artifactOwnerCount != 1 {
		return fmt.Errorf("insert refresh operation: diagnostic artifact does not belong to refresh receipt")
	}
	return nil
}

func validRefreshDecision(value RefreshDecision) bool {
	switch value {
	case RefreshDecisionSkipped, RefreshDecisionFastForwarded, RefreshDecisionRebased, RefreshDecisionMerged,
		RefreshDecisionConflicted, RefreshDecisionRepaired, RefreshDecisionRefused, RefreshDecisionError:
		return true
	default:
		return false
	}
}

func validRefreshConflictState(value RefreshConflictState) bool {
	switch value {
	case RefreshConflictStateNone, RefreshConflictStateDetected, RefreshConflictStateResolved:
		return true
	default:
		return false
	}
}

func validRefreshRepairState(value RefreshRepairState) bool {
	switch value {
	case RefreshRepairStateNotNeeded, RefreshRepairStateNotAttempted, RefreshRepairStateSucceeded, RefreshRepairStateFailed:
		return true
	default:
		return false
	}
}

// GetRefreshOperationsByRun returns Refresh receipts in stable operation order.
func (d *DB) GetRefreshOperationsByRun(runID string) ([]*RefreshOperation, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, fmt.Errorf("get refresh operations by run: run ID is required")
	}
	rows, err := d.sql.Query(
		`SELECT o.id, o.run_id, o.kind, o.step_id, o.round_id,
		        ro.strategy, ro.source_ref, ro.destination_ref, ro.authoritative_base_ref, ro.authoritative_base_sha,
		        ro.starting_head_sha, ro.decision, ro.resulting_head_sha, ro.conflict_state, ro.repair_state,
		        o.started_at, o.completed_at, o.duration_ms, o.diagnostic_artifact_id
		 FROM operations o
		 JOIN refresh_operations ro ON ro.operation_id = o.id
		 WHERE o.run_id = ? AND o.kind = ?
		 ORDER BY o.started_at, o.id`,
		runID, OperationKindRefresh,
	)
	if err != nil {
		return nil, fmt.Errorf("get refresh operations by run: %w", err)
	}

	var operations []*RefreshOperation
	for rows.Next() {
		operation, err := scanRefreshOperation(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("get refresh operations by run: %w", err)
		}
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("get refresh operations by run: iterate rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("get refresh operations by run: close rows: %w", err)
	}
	for _, operation := range operations {
		if err := d.populateOperationCommandAttemptIDs(operation); err != nil {
			return nil, fmt.Errorf("get refresh operations by run: %w", err)
		}
	}
	return operations, nil
}

func scanRefreshOperation(row interface{ Scan(...any) error }) (*RefreshOperation, error) {
	operation := &RefreshOperation{}
	var kind, strategy, decision, conflictState, repairState string
	var authoritativeBaseSHA sql.NullString
	if err := row.Scan(
		&operation.ID, &operation.RunID, &kind, &operation.StepID, &operation.RoundID,
		&strategy, &operation.SourceRef, &operation.DestinationRef, &operation.AuthoritativeBaseRef, &authoritativeBaseSHA,
		&operation.StartingHeadSHA, &decision, &operation.ResultingHeadSHA, &conflictState, &repairState,
		&operation.StartedAt, &operation.CompletedAt, &operation.DurationMS, &operation.DiagnosticArtifactID,
	); err != nil {
		return nil, err
	}
	operation.Kind = OperationKind(kind)
	operation.Strategy = types.RefreshStrategy(strategy)
	operation.Decision = RefreshDecision(decision)
	operation.ConflictState = RefreshConflictState(conflictState)
	operation.RepairState = RefreshRepairState(repairState)
	if authoritativeBaseSHA.Valid {
		operation.AuthoritativeBaseSHA = &authoritativeBaseSHA.String
	}
	return operation, nil
}

func (d *DB) populateOperationCommandAttemptIDs(operation *RefreshOperation) error {
	rows, err := d.sql.Query(
		`SELECT attempt_id FROM operation_command_attempts WHERE operation_id = ? ORDER BY sequence`,
		operation.ID,
	)
	if err != nil {
		return fmt.Errorf("load operation command attempts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var attemptID string
		if err := rows.Scan(&attemptID); err != nil {
			return fmt.Errorf("scan operation command attempt: %w", err)
		}
		operation.CommandAttemptIDs = append(operation.CommandAttemptIDs, attemptID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate operation command attempts: %w", err)
	}
	return nil
}
