package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	maxPushReceiptReasonBytes = 8 * 1024
	maxPushRetryReasonBytes   = 1024
)

func MaxPushReceiptReasonBytes() int { return maxPushReceiptReasonBytes }

type OperationKind string

const (
	OperationKindRefresh OperationKind = "refresh"
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
	RefreshDecisionCancelled     RefreshDecision = "cancelled"
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
	ID                    string
	RunID                 string
	Kind                  OperationKind
	StepID                string
	RoundID               string
	Strategy              types.RefreshStrategy
	SourceRef             string
	DestinationRef        string
	AuthoritativeBaseRef  string
	AuthoritativeBaseSHA  *string
	StartingHeadSHA       *string
	ResolvedTargetHeadSHA *string
	Decision              RefreshDecision
	ResultingHeadSHA      *string
	ConflictState         RefreshConflictState
	RepairState           RefreshRepairState
	CommandAttemptIDs     []string
	StartedAt             int64
	CompletedAt           int64
	DurationMS            int64
	DiagnosticArtifactID  *string
}

type PushLeaseOrForceDecision string

const (
	PushLeaseOrForceDecisionNewBranch      PushLeaseOrForceDecision = "new_branch"
	PushLeaseOrForceDecisionAlreadyEqual   PushLeaseOrForceDecision = "already_equal"
	PushLeaseOrForceDecisionForceWithLease PushLeaseOrForceDecision = "force_with_lease"
	PushLeaseOrForceDecisionRefused        PushLeaseOrForceDecision = "refused"
	PushLeaseOrForceDecisionUnavailable    PushLeaseOrForceDecision = "unavailable"
)

type PushOperationOutcome string

const (
	PushOperationOutcomeCreated      PushOperationOutcome = "created"
	PushOperationOutcomeUpdated      PushOperationOutcome = "updated"
	PushOperationOutcomeAlreadyEqual PushOperationOutcome = "already_equal"
	PushOperationOutcomeRefused      PushOperationOutcome = "refused"
	PushOperationOutcomeFailed       PushOperationOutcome = "failed"
	PushOperationOutcomeProcessError PushOperationOutcome = "process_error"
)

// PushOperation records one attempted push safety decision. The run's push
// binding remains authoritative for current custody; this row is immutable
// history of what the controller observed and decided.
type PushOperation struct {
	ID                    string
	RunID                 string
	Kind                  OperationKind
	StepID                string
	RoundID               string
	TargetKind            string
	TargetFingerprint     string
	TargetIdentity        string
	DestinationRef        string
	PushedSHA             *string
	ObservedRemoteSHA     *string
	LeaseOrForceDecision  PushLeaseOrForceDecision
	DecisionReason        string
	Outcome               PushOperationOutcome
	ReviewApprovedHeadSHA *string
	LastSeenSHA           *string
	RemoteBeforeSHA       *string
	RemoteAfterSHA        *string
	BindingUpdated        bool
	ResultingGeneration   *int64
	RetryOfOperationID    *string
	RetryReason           *string
	CommandAttemptIDs     []string
	StartedAt             int64
	CompletedAt           int64
	DurationMS            int64
	DiagnosticArtifactID  *string
}

func redactPushOperation(operation *PushOperation) {
	operation.TargetIdentity = safeurl.Redact(operation.TargetIdentity)
	operation.DecisionReason = safeurl.RedactText(operation.DecisionReason)
	if operation.RetryReason != nil {
		redacted := safeurl.RedactText(*operation.RetryReason)
		operation.RetryReason = &redacted
	}
}

// InsertPushOperation atomically records one completed Push receipt and its
// ordered command-attempt references. Target identity is redacted at the
// persistence boundary so credentialed remotes cannot enter history.
func (d *DB) InsertPushOperation(operation PushOperation) (*PushOperation, error) {
	if operation.ID != "" {
		return nil, fmt.Errorf("insert push operation: ID is assigned by the database")
	}
	if operation.Kind != "" && operation.Kind != OperationKindPush {
		return nil, fmt.Errorf("insert push operation: kind must be %q", OperationKindPush)
	}
	operation.Kind = OperationKindPush
	redactPushOperation(&operation)
	operation.ID = newID()
	if err := validatePushOperation(d, operation); err != nil {
		return nil, err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("insert push operation: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO operations
		 (id, run_id, kind, step_id, round_id, started_at, completed_at, duration_ms, diagnostic_artifact_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		operation.ID, operation.RunID, operation.Kind, operation.StepID, operation.RoundID,
		operation.StartedAt, operation.CompletedAt, operation.DurationMS, operation.DiagnosticArtifactID,
	); err != nil {
		return nil, fmt.Errorf("insert push operation: insert operation: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO push_operations
		 (operation_id, target_kind, target_fingerprint, target_identity, destination_ref, pushed_sha,
		  observed_remote_sha, lease_or_force_decision, decision_reason, outcome,
		  review_approved_head_sha, last_seen_sha, remote_before_sha, remote_after_sha,
			binding_updated, resulting_generation, retry_of_operation_id, retry_reason, terminalized)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		operation.ID, operation.TargetKind, operation.TargetFingerprint, operation.TargetIdentity,
		operation.DestinationRef, operation.PushedSHA, operation.ObservedRemoteSHA,
		operation.LeaseOrForceDecision, operation.DecisionReason, operation.Outcome,
		operation.ReviewApprovedHeadSHA, operation.LastSeenSHA, operation.RemoteBeforeSHA,
		operation.RemoteAfterSHA, boolInt(operation.BindingUpdated), operation.ResultingGeneration,
		operation.RetryOfOperationID, operation.RetryReason, 1,
	); err != nil {
		return nil, fmt.Errorf("insert push operation: insert push receipt: %w", err)
	}
	for index, attemptID := range operation.CommandAttemptIDs {
		if _, err := tx.Exec(
			`INSERT INTO operation_command_attempts (operation_id, run_id, attempt_id, sequence) VALUES (?, ?, ?, ?)`,
			operation.ID, operation.RunID, attemptID, index+1,
		); err != nil {
			return nil, fmt.Errorf("insert push operation: link command attempt: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("insert push operation: commit: %w", err)
	}
	return &operation, nil
}

// StartPushOperation allocates and persists the stable operation identity
// before safety evaluation. Callers terminalize the same row with
// CompletePushOperation on every controller exit path.
func (d *DB) StartPushOperation(operation PushOperation) (*PushOperation, error) {
	if operation.ID != "" {
		return nil, fmt.Errorf("start push operation: ID is assigned by the database")
	}
	if operation.Kind != "" && operation.Kind != OperationKindPush {
		return nil, fmt.Errorf("start push operation: kind must be %q", OperationKindPush)
	}
	operation.Kind = OperationKindPush
	redactPushOperation(&operation)
	operation.LeaseOrForceDecision = PushLeaseOrForceDecisionUnavailable
	operation.Outcome = PushOperationOutcomeProcessError
	if strings.TrimSpace(operation.DecisionReason) == "" {
		operation.DecisionReason = "push operation started"
	}
	operation.DecisionReason = safeurl.RedactText(operation.DecisionReason)
	if operation.StartedAt <= 0 {
		return nil, fmt.Errorf("start push operation: start time is required")
	}
	operation.CompletedAt = operation.StartedAt
	operation.DurationMS = 0
	operation.ID = newID()
	if err := validatePushOperation(d, operation); err != nil {
		return nil, err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("start push operation: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := insertPushOperationRows(tx, operation); err != nil {
		return nil, fmt.Errorf("start push operation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("start push operation: commit: %w", err)
	}
	return &operation, nil
}

// CompletePushOperation terminalizes a previously started Push receipt while
// retaining its database-assigned identity and immutable owner scope.
func (d *DB) CompletePushOperation(operation PushOperation) (*PushOperation, error) {
	if strings.TrimSpace(operation.ID) == "" {
		return nil, fmt.Errorf("complete push operation: ID is required")
	}
	if operation.Kind != "" && operation.Kind != OperationKindPush {
		return nil, fmt.Errorf("complete push operation: kind must be %q", OperationKindPush)
	}
	operation.Kind = OperationKindPush
	redactPushOperation(&operation)
	var runID, stepID, roundID string
	var terminalized int
	if err := d.sql.QueryRow(`SELECT run_id, step_id, round_id, po.terminalized FROM operations o JOIN push_operations po ON po.operation_id = o.id WHERE o.id = ? AND o.kind = ?`, operation.ID, OperationKindPush).Scan(&runID, &stepID, &roundID, &terminalized); err != nil {
		return nil, fmt.Errorf("complete push operation: load operation: %w", err)
	}
	if terminalized != 0 {
		return nil, fmt.Errorf("complete push operation: operation is already terminal")
	}
	if runID != operation.RunID || stepID != operation.StepID || roundID != operation.RoundID {
		return nil, fmt.Errorf("complete push operation: owner scope cannot change")
	}
	if err := validatePushOperation(d, operation); err != nil {
		return nil, err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("complete push operation: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE operations SET started_at = ?, completed_at = ?, duration_ms = ?, diagnostic_artifact_id = ? WHERE id = ? AND run_id = ? AND kind = ?`, operation.StartedAt, operation.CompletedAt, operation.DurationMS, operation.DiagnosticArtifactID, operation.ID, operation.RunID, OperationKindPush); err != nil {
		return nil, fmt.Errorf("complete push operation: update operation: %w", err)
	}
	if result, err := tx.Exec(`UPDATE push_operations SET target_kind = ?, target_fingerprint = ?, target_identity = ?, destination_ref = ?, pushed_sha = ?, observed_remote_sha = ?, lease_or_force_decision = ?, decision_reason = ?, outcome = ?, review_approved_head_sha = ?, last_seen_sha = ?, remote_before_sha = ?, remote_after_sha = ?, binding_updated = ?, resulting_generation = ?, retry_of_operation_id = ?, retry_reason = ?, terminalized = 1 WHERE operation_id = ? AND terminalized = 0`, operation.TargetKind, operation.TargetFingerprint, operation.TargetIdentity, operation.DestinationRef, operation.PushedSHA, operation.ObservedRemoteSHA, operation.LeaseOrForceDecision, operation.DecisionReason, operation.Outcome, operation.ReviewApprovedHeadSHA, operation.LastSeenSHA, operation.RemoteBeforeSHA, operation.RemoteAfterSHA, boolInt(operation.BindingUpdated), operation.ResultingGeneration, operation.RetryOfOperationID, operation.RetryReason, operation.ID); err != nil {
		return nil, fmt.Errorf("complete push operation: update push receipt: %w", err)
	} else if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return nil, fmt.Errorf("complete push operation: operation is already terminal")
	}
	if _, err := tx.Exec(`DELETE FROM operation_command_attempts WHERE operation_id = ?`, operation.ID); err != nil {
		return nil, fmt.Errorf("complete push operation: clear attempt links: %w", err)
	}
	for index, attemptID := range operation.CommandAttemptIDs {
		if _, err := tx.Exec(`INSERT INTO operation_command_attempts (operation_id, run_id, attempt_id, sequence) VALUES (?, ?, ?, ?)`, operation.ID, operation.RunID, attemptID, index+1); err != nil {
			return nil, fmt.Errorf("complete push operation: link command attempt: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("complete push operation: commit: %w", err)
	}
	return &operation, nil
}

func insertPushOperationRows(tx *sql.Tx, operation PushOperation) error {
	if _, err := tx.Exec(`INSERT INTO operations (id, run_id, kind, step_id, round_id, started_at, completed_at, duration_ms, diagnostic_artifact_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, operation.ID, operation.RunID, operation.Kind, operation.StepID, operation.RoundID, operation.StartedAt, operation.CompletedAt, operation.DurationMS, operation.DiagnosticArtifactID); err != nil {
		return fmt.Errorf("insert operation: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO push_operations (operation_id, target_kind, target_fingerprint, target_identity, destination_ref, pushed_sha, observed_remote_sha, lease_or_force_decision, decision_reason, outcome, review_approved_head_sha, last_seen_sha, remote_before_sha, remote_after_sha, binding_updated, resulting_generation, retry_of_operation_id, retry_reason, terminalized) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, operation.ID, operation.TargetKind, operation.TargetFingerprint, operation.TargetIdentity, operation.DestinationRef, operation.PushedSHA, operation.ObservedRemoteSHA, operation.LeaseOrForceDecision, operation.DecisionReason, operation.Outcome, operation.ReviewApprovedHeadSHA, operation.LastSeenSHA, operation.RemoteBeforeSHA, operation.RemoteAfterSHA, boolInt(operation.BindingUpdated), operation.ResultingGeneration, operation.RetryOfOperationID, operation.RetryReason, 0); err != nil {
		return fmt.Errorf("insert push receipt: %w", err)
	}
	for index, attemptID := range operation.CommandAttemptIDs {
		if _, err := tx.Exec(`INSERT INTO operation_command_attempts (operation_id, run_id, attempt_id, sequence) VALUES (?, ?, ?, ?)`, operation.ID, operation.RunID, attemptID, index+1); err != nil {
			return fmt.Errorf("link command attempt: %w", err)
		}
	}
	return nil
}

func validatePushOperation(d *DB, operation PushOperation) error {
	if strings.TrimSpace(operation.RunID) == "" || strings.TrimSpace(operation.StepID) == "" || strings.TrimSpace(operation.RoundID) == "" ||
		strings.TrimSpace(operation.TargetKind) == "" || strings.TrimSpace(operation.TargetFingerprint) == "" ||
		strings.TrimSpace(operation.TargetIdentity) == "" || strings.TrimSpace(operation.DestinationRef) == "" ||
		strings.TrimSpace(operation.DecisionReason) == "" {
		return fmt.Errorf("insert push operation: required receipt identity is incomplete")
	}
	if !validPushLeaseOrForceDecision(operation.LeaseOrForceDecision) {
		return fmt.Errorf("insert push operation: unsupported lease/force decision %q", operation.LeaseOrForceDecision)
	}
	if !validPushOperationOutcome(operation.Outcome) {
		return fmt.Errorf("insert push operation: unsupported outcome %q", operation.Outcome)
	}
	if operation.StartedAt <= 0 || operation.CompletedAt < operation.StartedAt || operation.DurationMS < 0 {
		return fmt.Errorf("insert push operation: timing is invalid")
	}
	if operation.PushedSHA != nil && strings.TrimSpace(*operation.PushedSHA) == "" {
		return fmt.Errorf("insert push operation: pushed SHA is empty")
	}
	if operation.ResultingGeneration != nil && *operation.ResultingGeneration < 0 {
		return fmt.Errorf("insert push operation: resulting generation must be non-negative")
	}
	if len(operation.DecisionReason) > maxPushReceiptReasonBytes {
		return fmt.Errorf("insert push operation: decision reason exceeds %d bytes", maxPushReceiptReasonBytes)
	}
	if operation.RetryReason != nil && len(*operation.RetryReason) > maxPushRetryReasonBytes {
		return fmt.Errorf("insert push operation: retry reason exceeds %d bytes", maxPushRetryReasonBytes)
	}
	var scopeCount int
	if err := d.sql.QueryRow(
		`SELECT count(*) FROM step_rounds r JOIN step_results s ON s.id = r.step_result_id
		 WHERE r.id = ? AND s.id = ? AND s.run_id = ? AND s.step_name IN (?, ?)`,
		operation.RoundID, operation.StepID, operation.RunID, types.StepPush, types.StepCI,
	).Scan(&scopeCount); err != nil {
		return fmt.Errorf("insert push operation: validate step and round: %w", err)
	}
	if scopeCount != 1 {
		return fmt.Errorf("insert push operation: push or ci step and round do not belong to run")
	}
	if operation.RetryOfOperationID != nil {
		if strings.TrimSpace(*operation.RetryOfOperationID) == "" {
			return fmt.Errorf("insert push operation: retry operation reference is empty")
		}
		if operation.ID != "" && *operation.RetryOfOperationID == operation.ID {
			return fmt.Errorf("insert push operation: retry operation cannot reference itself")
		}
		if operation.RetryReason == nil || strings.TrimSpace(*operation.RetryReason) == "" {
			return fmt.Errorf("insert push operation: retry reason is required")
		}
		var compatible int
		var pushedSHA sql.NullString
		if operation.PushedSHA != nil {
			pushedSHA.Valid, pushedSHA.String = true, *operation.PushedSHA
		}
		if err := d.sql.QueryRow(
			`SELECT count(*) FROM operations o JOIN push_operations po ON po.operation_id = o.id
			 WHERE o.id = ? AND o.run_id = ? AND o.kind = ? AND po.target_kind = ?
			   AND po.target_fingerprint = ? AND po.target_identity = ? AND po.destination_ref = ?
			   AND (o.started_at < ? OR (o.started_at = ? AND o.id < ?))
			   AND ((po.pushed_sha IS NULL AND ? IS NULL) OR po.pushed_sha = ?)`,
			*operation.RetryOfOperationID, operation.RunID, OperationKindPush,
			operation.TargetKind, operation.TargetFingerprint, operation.TargetIdentity, operation.DestinationRef,
			operation.StartedAt, operation.StartedAt, operation.ID,
			func() any {
				if pushedSHA.Valid {
					return pushedSHA.String
				}
				return nil
			}(),
			func() any {
				if pushedSHA.Valid {
					return pushedSHA.String
				}
				return nil
			}(),
		).Scan(&compatible); err != nil {
			return fmt.Errorf("insert push operation: validate retry operation: %w", err)
		}
		if compatible != 1 {
			return fmt.Errorf("insert push operation: retry operation is missing or incompatible")
		}
	} else if operation.RetryReason != nil && strings.TrimSpace(*operation.RetryReason) != "" {
		return fmt.Errorf("insert push operation: retry reason requires retry operation")
	}
	if operation.ID != "" && operation.RetryOfOperationID != nil {
		var cycle int
		if err := d.sql.QueryRow(`WITH RECURSIVE chain(id) AS (
			SELECT ?
			UNION ALL
			SELECT po.retry_of_operation_id FROM push_operations po JOIN chain c ON po.operation_id = c.id WHERE po.retry_of_operation_id IS NOT NULL
		) SELECT count(*) FROM chain WHERE id = ?`, *operation.RetryOfOperationID, operation.ID).Scan(&cycle); err != nil {
			return fmt.Errorf("insert push operation: validate retry cycle: %w", err)
		}
		if cycle > 0 {
			return fmt.Errorf("insert push operation: retry link would create a cycle")
		}
	}
	seen := make(map[string]struct{}, len(operation.CommandAttemptIDs))
	for _, attemptID := range operation.CommandAttemptIDs {
		if strings.TrimSpace(attemptID) == "" {
			return fmt.Errorf("insert push operation: command attempt reference is empty")
		}
		if _, exists := seen[attemptID]; exists {
			return fmt.Errorf("insert push operation: command attempt references must be unique")
		}
		seen[attemptID] = struct{}{}
		var count int
		if err := d.sql.QueryRow(
			`SELECT count(*) FROM command_attempts WHERE id = ? AND run_id = ? AND step_id = ? AND round_id = ?`,
			attemptID, operation.RunID, operation.StepID, operation.RoundID,
		).Scan(&count); err != nil {
			return fmt.Errorf("insert push operation: validate command attempt: %w", err)
		}
		if count != 1 {
			return fmt.Errorf("insert push operation: command attempt does not belong to push receipt")
		}
	}
	if operation.DiagnosticArtifactID == nil {
		return nil
	}
	if strings.TrimSpace(*operation.DiagnosticArtifactID) == "" {
		return fmt.Errorf("insert push operation: diagnostic artifact reference is empty")
	}
	var artifactCount int
	if err := d.sql.QueryRow(
		`SELECT count(*) FROM artifacts WHERE id = ? AND run_id = ? AND step_id = ? AND round_id = ?
		 AND purpose = ? AND kind = ? AND storage_root = ? AND command_attempt_id IS NULL`,
		*operation.DiagnosticArtifactID, operation.RunID, operation.StepID, operation.RoundID,
		ArtifactPurposeOperationDiagnostic, ArtifactKindOperationDiagnostic, ArtifactStorageRootRun,
	).Scan(&artifactCount); err != nil {
		return fmt.Errorf("insert push operation: validate diagnostic artifact: %w", err)
	}
	if artifactCount != 1 {
		return fmt.Errorf("insert push operation: diagnostic artifact does not belong to push receipt")
	}
	return nil
}

func validPushLeaseOrForceDecision(value PushLeaseOrForceDecision) bool {
	switch value {
	case PushLeaseOrForceDecisionNewBranch, PushLeaseOrForceDecisionAlreadyEqual,
		PushLeaseOrForceDecisionForceWithLease, PushLeaseOrForceDecisionRefused,
		PushLeaseOrForceDecisionUnavailable:
		return true
	default:
		return false
	}
}

func validPushOperationOutcome(value PushOperationOutcome) bool {
	switch value {
	case PushOperationOutcomeCreated, PushOperationOutcomeUpdated, PushOperationOutcomeAlreadyEqual,
		PushOperationOutcomeRefused, PushOperationOutcomeFailed, PushOperationOutcomeProcessError:
		return true
	default:
		return false
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// GetPushOperationsByRun returns Push receipts in stable operation order.
func (d *DB) GetPushOperationsByRun(runID string) ([]*PushOperation, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, fmt.Errorf("get push operations by run: run ID is required")
	}
	rows, err := d.sql.Query(
		`SELECT o.id, o.run_id, o.kind, o.step_id, o.round_id,
		        po.target_kind, po.target_fingerprint, po.target_identity, po.destination_ref, po.pushed_sha,
		        po.observed_remote_sha, po.lease_or_force_decision, po.decision_reason, po.outcome,
		        po.review_approved_head_sha, po.last_seen_sha, po.remote_before_sha, po.remote_after_sha,
		        po.binding_updated, po.resulting_generation, po.retry_of_operation_id, po.retry_reason,
		        o.started_at, o.completed_at, o.duration_ms, o.diagnostic_artifact_id
		 FROM operations o JOIN push_operations po ON po.operation_id = o.id
		 WHERE o.run_id = ? AND o.kind = ? ORDER BY o.started_at, o.id`,
		runID, OperationKindPush,
	)
	if err != nil {
		return nil, fmt.Errorf("get push operations by run: %w", err)
	}
	defer rows.Close()
	var operations []*PushOperation
	for rows.Next() {
		operation := &PushOperation{}
		var kind, decision, outcome string
		var pushed, observed, approved, lastSeen, remoteBefore, remoteAfter sql.NullString
		var binding int
		if err := rows.Scan(&operation.ID, &operation.RunID, &kind, &operation.StepID, &operation.RoundID,
			&operation.TargetKind, &operation.TargetFingerprint, &operation.TargetIdentity, &operation.DestinationRef,
			&pushed, &observed, &decision, &operation.DecisionReason, &outcome,
			&approved, &lastSeen, &remoteBefore, &remoteAfter, &binding, &operation.ResultingGeneration,
			&operation.RetryOfOperationID, &operation.RetryReason,
			&operation.StartedAt, &operation.CompletedAt, &operation.DurationMS, &operation.DiagnosticArtifactID); err != nil {
			return nil, fmt.Errorf("get push operations by run: scan: %w", err)
		}
		operation.Kind = OperationKind(kind)
		operation.PushedSHA = nullableStringPointer(pushed)
		operation.LeaseOrForceDecision = PushLeaseOrForceDecision(decision)
		operation.Outcome = PushOperationOutcome(outcome)
		operation.BindingUpdated = binding != 0
		operation.ObservedRemoteSHA = nullableStringPointer(observed)
		operation.ReviewApprovedHeadSHA = nullableStringPointer(approved)
		operation.LastSeenSHA = nullableStringPointer(lastSeen)
		operation.RemoteBeforeSHA = nullableStringPointer(remoteBefore)
		operation.RemoteAfterSHA = nullableStringPointer(remoteAfter)
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get push operations by run: iterate rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("get push operations by run: close rows: %w", err)
	}
	for _, operation := range operations {
		attemptIDs, err := d.loadOperationCommandAttemptIDs(operation.ID)
		if err != nil {
			return nil, fmt.Errorf("get push operations by run: %w", err)
		}
		operation.CommandAttemptIDs = attemptIDs
	}
	return operations, nil
}

func nullableStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	result := value.String
	return &result
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
	if err := insertRefreshOperationRows(tx, operation); err != nil {
		return nil, err
	}
	if operation.DiagnosticArtifactID != nil {
		if _, err := tx.Exec(
			`UPDATE artifacts SET operation_id = ? WHERE id = ? AND operation_id IS NULL`,
			operation.ID, *operation.DiagnosticArtifactID,
		); err != nil {
			return nil, fmt.Errorf("insert refresh operation: claim diagnostic artifact: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("insert refresh operation: commit: %w", err)
	}
	return &operation, nil
}

// InsertRefreshOperationWithDiagnostic atomically records a Refresh receipt
// and its operation-owned diagnostic artifact. Both stable IDs are assigned
// inside the transaction so the artifact cannot be attached to a different
// operation or outlive a partially inserted receipt.
func (d *DB) InsertRefreshOperationWithDiagnostic(operation RefreshOperation, diagnostic Artifact) (*RefreshOperation, error) {
	if operation.ID != "" {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: ID is assigned by the database")
	}
	if operation.Kind != "" && operation.Kind != OperationKindRefresh {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: kind must be %q", OperationKindRefresh)
	}
	operation.Kind = OperationKindRefresh
	strategy, err := types.ParseRefreshStrategy(string(operation.Strategy))
	if err != nil || strategy == "" {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: unsupported strategy %q", operation.Strategy)
	}
	operation.Strategy = strategy
	if operation.DiagnosticArtifactID != nil {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: diagnostic artifact ID is assigned by the database")
	}
	if diagnostic.ID != "" || diagnostic.OperationID != nil {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: artifact IDs are assigned by the database")
	}
	if diagnostic.CommandAttemptID != nil {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: diagnostic cannot belong to a command attempt")
	}
	if diagnostic.Purpose != ArtifactPurposeOperationDiagnostic || diagnostic.Kind != ArtifactKindOperationDiagnostic || diagnostic.StorageRoot != ArtifactStorageRootRun {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: artifact is not an operation diagnostic")
	}
	if err := validateArtifactForInsert(diagnostic); err != nil {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: %w", err)
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := validateRefreshOperation(tx, operation); err != nil {
		return nil, err
	}

	operation.ID = newID()
	diagnostic.ID = newID()
	diagnostic.OperationID = &operation.ID
	diagnostic.CreatedAt = time.Now().UnixMilli()
	operation.DiagnosticArtifactID = &diagnostic.ID
	if err := insertRefreshOperationRows(tx, RefreshOperation{
		ID:                    operation.ID,
		RunID:                 operation.RunID,
		Kind:                  operation.Kind,
		StepID:                operation.StepID,
		RoundID:               operation.RoundID,
		Strategy:              operation.Strategy,
		SourceRef:             operation.SourceRef,
		DestinationRef:        operation.DestinationRef,
		AuthoritativeBaseRef:  operation.AuthoritativeBaseRef,
		AuthoritativeBaseSHA:  operation.AuthoritativeBaseSHA,
		StartingHeadSHA:       operation.StartingHeadSHA,
		ResolvedTargetHeadSHA: operation.ResolvedTargetHeadSHA,
		Decision:              operation.Decision,
		ResultingHeadSHA:      operation.ResultingHeadSHA,
		ConflictState:         operation.ConflictState,
		RepairState:           operation.RepairState,
		CommandAttemptIDs:     operation.CommandAttemptIDs,
		StartedAt:             operation.StartedAt,
		CompletedAt:           operation.CompletedAt,
		DurationMS:            operation.DurationMS,
		DiagnosticArtifactID:  nil,
	}); err != nil {
		return nil, err
	}
	if err := validateArtifactProducer(tx, diagnostic); err != nil {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: %w", err)
	}
	if existing, err := getArtifactByStoragePath(tx, diagnostic.StorageRoot, diagnostic.RelativePath); err != nil {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: check artifact path: %w", err)
	} else if existing != nil {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: artifact already exists at %s", diagnostic.RelativePath)
	}
	if _, err := tx.Exec(
		`INSERT INTO artifacts
		 (id, run_id, step_id, round_id, invocation_id, command_attempt_id, operation_id, purpose, label, description,
		  storage_root, relative_path, kind, media_type, encoding, sha256, source_bytes, state, reason,
		  publication_state, publication_url, publication_commit_sha, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		diagnostic.ID, diagnostic.RunID, diagnostic.StepID, diagnostic.RoundID, diagnostic.InvocationID, diagnostic.CommandAttemptID, diagnostic.OperationID,
		diagnostic.Purpose, diagnostic.Label, diagnostic.Description, diagnostic.StorageRoot, diagnostic.RelativePath,
		diagnostic.Kind, diagnostic.MediaType, diagnostic.Encoding, diagnostic.SHA256, diagnostic.SourceBytes, diagnostic.State,
		diagnostic.Reason, diagnostic.PublicationState, diagnostic.PublicationURL, diagnostic.PublicationCommitSHA, diagnostic.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: insert artifact: %w", err)
	}
	if _, err := tx.Exec(`UPDATE operations SET diagnostic_artifact_id = ? WHERE id = ?`, diagnostic.ID, operation.ID); err != nil {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: link artifact: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("insert refresh operation with diagnostic: commit: %w", err)
	}
	return &operation, nil
}

func insertRefreshOperationRows(tx *sql.Tx, operation RefreshOperation) error {
	if _, err := tx.Exec(
		`INSERT INTO operations
		 (id, run_id, kind, step_id, round_id, started_at, completed_at, duration_ms, diagnostic_artifact_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		operation.ID, operation.RunID, operation.Kind, operation.StepID, operation.RoundID,
		operation.StartedAt, operation.CompletedAt, operation.DurationMS, operation.DiagnosticArtifactID,
	); err != nil {
		return fmt.Errorf("insert refresh operation: insert operation: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO refresh_operations
			 (operation_id, strategy, source_ref, destination_ref, authoritative_base_ref, authoritative_base_sha,
			  starting_head_sha, resolved_target_head_sha, decision, resulting_head_sha, conflict_state, repair_state)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		operation.ID, operation.Strategy, operation.SourceRef, operation.DestinationRef,
		operation.AuthoritativeBaseRef, operation.AuthoritativeBaseSHA, operation.StartingHeadSHA,
		operation.ResolvedTargetHeadSHA,
		operation.Decision, operation.ResultingHeadSHA, operation.ConflictState, operation.RepairState,
	); err != nil {
		return fmt.Errorf("insert refresh operation: insert refresh receipt: %w", err)
	}
	for index, attemptID := range operation.CommandAttemptIDs {
		if _, err := tx.Exec(
			`INSERT INTO operation_command_attempts (operation_id, run_id, attempt_id, sequence) VALUES (?, ?, ?, ?)`,
			operation.ID, operation.RunID, attemptID, index+1,
		); err != nil {
			return fmt.Errorf("insert refresh operation: link command attempt: %w", err)
		}
	}
	return nil
}

type operationQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func validateRefreshOperation(q operationQuerier, operation RefreshOperation) error {
	if strings.TrimSpace(operation.RunID) == "" || strings.TrimSpace(operation.StepID) == "" || strings.TrimSpace(operation.RoundID) == "" ||
		strings.TrimSpace(operation.SourceRef) == "" || strings.TrimSpace(operation.DestinationRef) == "" ||
		strings.TrimSpace(operation.AuthoritativeBaseRef) == "" {
		return fmt.Errorf("insert refresh operation: required receipt identity is incomplete")
	}
	if operation.StartingHeadSHA != nil && strings.TrimSpace(*operation.StartingHeadSHA) == "" {
		return fmt.Errorf("insert refresh operation: starting head SHA is empty")
	}
	if operation.ResolvedTargetHeadSHA != nil && strings.TrimSpace(*operation.ResolvedTargetHeadSHA) == "" {
		return fmt.Errorf("insert refresh operation: resolved target head SHA is empty")
	}
	if operation.Strategy != types.RefreshStrategyRebase && operation.Strategy != types.RefreshStrategyMerge {
		return fmt.Errorf("insert refresh operation: unsupported strategy %q", operation.Strategy)
	}
	if !validRefreshDecision(operation.Decision) {
		return fmt.Errorf("insert refresh operation: unsupported decision %q", operation.Decision)
	}
	if operation.AuthoritativeBaseSHA == nil {
		if operation.Decision != RefreshDecisionRefused && operation.Decision != RefreshDecisionError && operation.Decision != RefreshDecisionCancelled {
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
	if !validRefreshOutcomeTriple(operation.Decision, operation.ConflictState, operation.RepairState) {
		return fmt.Errorf("insert refresh operation: unsupported decision/conflict/repair combination (%q, %q, %q)", operation.Decision, operation.ConflictState, operation.RepairState)
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
	var operationID sql.NullString
	if err := q.QueryRow(`SELECT operation_id FROM artifacts WHERE id = ?`, *operation.DiagnosticArtifactID).Scan(&operationID); err != nil {
		return fmt.Errorf("insert refresh operation: inspect diagnostic artifact owner: %w", err)
	}
	if operationID.Valid {
		return fmt.Errorf("insert refresh operation: diagnostic artifact already belongs to operation %q", operationID.String)
	}
	return nil
}

func validRefreshDecision(value RefreshDecision) bool {
	switch value {
	case RefreshDecisionSkipped, RefreshDecisionFastForwarded, RefreshDecisionRebased, RefreshDecisionMerged,
		RefreshDecisionConflicted, RefreshDecisionRepaired, RefreshDecisionRefused, RefreshDecisionError, RefreshDecisionCancelled:
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

func validRefreshOutcomeTriple(decision RefreshDecision, conflict RefreshConflictState, repair RefreshRepairState) bool {
	switch decision {
	case RefreshDecisionSkipped, RefreshDecisionFastForwarded, RefreshDecisionRebased, RefreshDecisionMerged:
		return conflict == RefreshConflictStateNone && repair == RefreshRepairStateNotNeeded
	case RefreshDecisionConflicted:
		return conflict == RefreshConflictStateDetected && (repair == RefreshRepairStateNotAttempted || repair == RefreshRepairStateFailed)
	case RefreshDecisionRepaired:
		return conflict == RefreshConflictStateResolved && repair == RefreshRepairStateSucceeded
	case RefreshDecisionRefused:
		return conflict == RefreshConflictStateNone && repair == RefreshRepairStateNotAttempted
	case RefreshDecisionError:
		return conflict == RefreshConflictStateNone && (repair == RefreshRepairStateNotAttempted || repair == RefreshRepairStateFailed)
	case RefreshDecisionCancelled:
		if conflict == RefreshConflictStateNone {
			return repair == RefreshRepairStateNotAttempted
		}
		return conflict == RefreshConflictStateDetected && (repair == RefreshRepairStateNotAttempted || repair == RefreshRepairStateFailed)
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
		        ro.starting_head_sha, ro.resolved_target_head_sha, ro.decision, ro.resulting_head_sha, ro.conflict_state, ro.repair_state,
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
	var authoritativeBaseSHA, startingHeadSHA, resolvedTargetHeadSHA, resultingHeadSHA sql.NullString
	if err := row.Scan(
		&operation.ID, &operation.RunID, &kind, &operation.StepID, &operation.RoundID,
		&strategy, &operation.SourceRef, &operation.DestinationRef, &operation.AuthoritativeBaseRef, &authoritativeBaseSHA,
		&startingHeadSHA, &resolvedTargetHeadSHA, &decision, &resultingHeadSHA, &conflictState, &repairState,
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
	if startingHeadSHA.Valid {
		operation.StartingHeadSHA = &startingHeadSHA.String
	}
	if resolvedTargetHeadSHA.Valid {
		operation.ResolvedTargetHeadSHA = &resolvedTargetHeadSHA.String
	}
	if resultingHeadSHA.Valid {
		operation.ResultingHeadSHA = &resultingHeadSHA.String
	}
	return operation, nil
}

func (d *DB) populateOperationCommandAttemptIDs(operation *RefreshOperation) error {
	ids, err := d.loadOperationCommandAttemptIDs(operation.ID)
	if err != nil {
		return err
	}
	operation.CommandAttemptIDs = ids
	return nil
}

func (d *DB) loadOperationCommandAttemptIDs(operationID string) ([]string, error) {
	rows, err := d.sql.Query(
		`SELECT attempt_id FROM operation_command_attempts WHERE operation_id = ? ORDER BY sequence`,
		operationID,
	)
	if err != nil {
		return nil, fmt.Errorf("load operation command attempts: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var attemptID string
		if err := rows.Scan(&attemptID); err != nil {
			return nil, fmt.Errorf("scan operation command attempt: %w", err)
		}
		ids = append(ids, attemptID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate operation command attempts: %w", err)
	}
	return ids, nil
}
