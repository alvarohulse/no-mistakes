package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RunMetricReceipt is the immutable content-free record retained after a rich
// run and its cascading child rows are pruned.
type RunMetricReceipt struct {
	RunID            string
	RepoID           string
	RunCreatedAt     int64
	RunStatus        types.RunStatus
	SchemaVersion    int
	PayloadJSON      string
	ReceiptSHA256    string
	ArchivedAt       int64
	PullRequest      bool
	ReportedFindings int
	FixedFindings    int
	StepStats        []StepStats
	AgentAggregates  []AgentInvocationAggregate
}

// ListRunRetentionCandidates returns terminal, unpinned runs outside both the
// age window and the newest-terminal floor, oldest first. Unknown lifecycle
// states fail safe by never entering the terminal candidate set.
func (d *DB) ListRunRetentionCandidates(createdBefore int64, keepNewestTerminal int) ([]string, error) {
	if keepNewestTerminal < 0 {
		return nil, fmt.Errorf("keep newest terminal must be non-negative")
	}
	rows, err := d.sql.Query(`
		WITH ranked AS (
			SELECT id, created_at,
			       ROW_NUMBER() OVER (ORDER BY created_at DESC, id DESC) AS terminal_rank
			FROM runs
			WHERE status IN ('completed', 'failed', 'cancelled') AND pinned_at IS NULL
		)
		SELECT id FROM ranked
		WHERE created_at < ? AND terminal_rank > ?
		ORDER BY created_at, id`, createdBefore, keepNewestTerminal)
	if err != nil {
		return nil, fmt.Errorf("list run retention candidates: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan run retention candidate: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run retention candidates: %w", err)
	}
	return ids, nil
}

// ListNewestTerminalUnpinned returns the exact run IDs protected by the
// terminal-run floor, newest first.
func (d *DB) ListNewestTerminalUnpinned(limit int) ([]string, error) {
	if limit <= 0 {
		return []string{}, nil
	}
	rows, err := d.sql.Query(`
		SELECT id FROM runs
		WHERE status IN ('completed', 'failed', 'cancelled') AND pinned_at IS NULL
		ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list newest terminal unpinned runs: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan newest terminal unpinned run: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate newest terminal unpinned runs: %w", err)
	}
	return ids, nil
}

// ArchiveRunWithMetricReceipt stores one immutable receipt and deletes the
// matching terminal rich run in the same transaction.
func (d *DB) ArchiveRunWithMetricReceipt(receipt RunMetricReceipt, requireUnpinned bool) (bool, error) {
	return d.archiveRunWithMetricReceipt(receipt, requireUnpinned, nil, nil)
}

// ArchiveRunWithMetricReceiptAndTargets also records the exact filesystem
// cleanup targets before deleting the rich row. A crash or cleanup failure can
// then be retried without resolving paths from mutable configuration.
func (d *DB) ArchiveRunWithMetricReceiptAndTargets(receipt RunMetricReceipt, requireUnpinned bool, targets []string, beforeDelete func() error) (bool, error) {
	if beforeDelete != nil && len(targets) == 0 {
		return false, fmt.Errorf("archive run metric receipt: artifact cleanup targets are required")
	}
	return d.archiveRunWithMetricReceipt(receipt, requireUnpinned, targets, beforeDelete)
}

func (d *DB) archiveRunWithMetricReceipt(receipt RunMetricReceipt, requireUnpinned bool, targets []string, beforeDelete func() error) (bool, error) {
	if strings.TrimSpace(receipt.RunID) == "" || strings.TrimSpace(receipt.RepoID) == "" {
		return false, fmt.Errorf("archive run metric receipt: run and repository IDs are required")
	}
	if receipt.SchemaVersion < 1 || strings.TrimSpace(receipt.PayloadJSON) == "" {
		return false, fmt.Errorf("archive run metric receipt: schema version and payload are required")
	}
	stepStatsJSON, err := json.Marshal(nonNilStepStats(receipt.StepStats))
	if err != nil {
		return false, fmt.Errorf("encode receipt step stats: %w", err)
	}
	aggregatesJSON, err := json.Marshal(nonNilAgentAggregates(receipt.AgentAggregates))
	if err != nil {
		return false, fmt.Errorf("encode receipt agent aggregates: %w", err)
	}
	receipt.ReceiptSHA256, err = runMetricReceiptDigest(receipt, string(stepStatsJSON), string(aggregatesJSON))
	if err != nil {
		return false, err
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return false, fmt.Errorf("begin run metric archival: %w", err)
	}
	defer tx.Rollback()
	var repoID string
	var status types.RunStatus
	var createdAt int64
	var pinnedAt *int64
	err = tx.QueryRow(`SELECT repo_id, status, created_at, pinned_at FROM runs WHERE id = ?`, receipt.RunID).Scan(&repoID, &status, &createdAt, &pinnedAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read run for metric archival: %w", err)
	}
	if repoID != receipt.RepoID || createdAt != receipt.RunCreatedAt || status != receipt.RunStatus {
		return false, fmt.Errorf("archive run metric receipt: payload identity does not match run")
	}
	if !terminalRunStatus(status) {
		return false, fmt.Errorf("archive run metric receipt: run %q is not terminal", receipt.RunID)
	}
	if requireUnpinned && pinnedAt != nil {
		return false, nil
	}
	lockSQL := `UPDATE runs SET updated_at = updated_at WHERE id = ? AND status IN ('completed', 'failed', 'cancelled')`
	if requireUnpinned {
		lockSQL += ` AND pinned_at IS NULL`
	}
	lockResult, err := tx.Exec(lockSQL, receipt.RunID)
	if err != nil {
		return false, fmt.Errorf("lock run for metric archival: %w", err)
	}
	locked, err := lockResult.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read metric archival lock result: %w", err)
	}
	if locked != 1 {
		return false, nil
	}

	var existingSHA string
	err = tx.QueryRow(`SELECT receipt_sha256 FROM run_metric_receipts WHERE run_id = ?`, receipt.RunID).Scan(&existingSHA)
	switch {
	case err == nil && existingSHA != receipt.ReceiptSHA256:
		return false, fmt.Errorf("archive run metric receipt: immutable receipt %q differs", receipt.RunID)
	case err != nil && err != sql.ErrNoRows:
		return false, fmt.Errorf("read existing run metric receipt: %w", err)
	case err == sql.ErrNoRows:
		cleanupPending := 0
		if beforeDelete != nil {
			cleanupPending = 1
		}
		_, err = tx.Exec(`
			INSERT INTO run_metric_receipts (
				run_id, repo_id, run_created_at, run_status, schema_version,
				payload_json, receipt_sha256, archived_at, pull_request,
				reported_findings, fixed_findings, step_stats_json, agent_aggregates_json,
				artifact_cleanup_pending
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			receipt.RunID, receipt.RepoID, receipt.RunCreatedAt, receipt.RunStatus, receipt.SchemaVersion,
			receipt.PayloadJSON, receipt.ReceiptSHA256, receipt.ArchivedAt, receipt.PullRequest,
			receipt.ReportedFindings, receipt.FixedFindings, string(stepStatsJSON), string(aggregatesJSON), cleanupPending,
		)
		if err != nil {
			return false, fmt.Errorf("insert run metric receipt: %w", err)
		}
		if beforeDelete != nil {
			targetsJSON, marshalErr := json.Marshal(targets)
			if marshalErr != nil {
				return false, fmt.Errorf("encode artifact cleanup targets: %w", marshalErr)
			}
			if _, err = tx.Exec(`INSERT INTO run_artifact_cleanup_journal (run_id, targets_json) VALUES (?, ?)`, receipt.RunID, string(targetsJSON)); err != nil {
				return false, fmt.Errorf("insert artifact cleanup journal: %w", err)
			}
		}
	}
	deleteSQL := `DELETE FROM runs WHERE id = ? AND status IN ('completed', 'failed', 'cancelled')`
	if requireUnpinned {
		deleteSQL += ` AND pinned_at IS NULL`
	}
	result, err := tx.Exec(deleteSQL, receipt.RunID)
	if err != nil {
		return false, fmt.Errorf("delete archived rich run: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read archived rich run deletion: %w", err)
	}
	if affected != 1 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit run metric archival: %w", err)
	}
	if beforeDelete != nil {
		if err := beforeDelete(); err != nil {
			return true, fmt.Errorf("clean rich run artifacts after archival: %w", err)
		}
		if err := d.markRunArtifactCleanupComplete(receipt.RunID); err != nil {
			return true, err
		}
	}
	return true, nil
}

// CleanupPendingRunArtifactsWithTargets retries filesystem cleanup durably
// owned by archived metric receipts. A successful callback is marked only
// afterward, so crashes and partial cleanup remain safely retryable.
func (d *DB) CleanupPendingRunArtifactsWithTargets(repoID string, cleanup func(string, []string) error) (int, error) {
	return d.cleanupPendingRunArtifacts(repoID, cleanup)
}

func (d *DB) cleanupPendingRunArtifacts(repoID string, cleanup func(string, []string) error) (int, error) {
	if cleanup == nil {
		return 0, nil
	}
	query := `SELECT r.run_id, COALESCE(j.targets_json, '[]') FROM run_metric_receipts r LEFT JOIN run_artifact_cleanup_journal j ON j.run_id = r.run_id WHERE r.artifact_cleanup_pending = 1`
	args := []any{}
	if repoID != "" {
		query += ` AND r.repo_id = ?`
		args = append(args, repoID)
	}
	query += ` ORDER BY r.run_created_at, r.run_id`
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return 0, fmt.Errorf("list pending run artifact cleanup: %w", err)
	}
	type pending struct {
		runID   string
		targets []string
	}
	var pendingRuns []pending
	for rows.Next() {
		var runID string
		var targetsJSON string
		if err := rows.Scan(&runID, &targetsJSON); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan pending run artifact cleanup: %w", err)
		}
		var targets []string
		if err := json.Unmarshal([]byte(targetsJSON), &targets); err != nil {
			rows.Close()
			return 0, fmt.Errorf("decode pending run artifact cleanup: %w", err)
		}
		pendingRuns = append(pendingRuns, pending{runID, targets})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate pending run artifact cleanup: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close pending run artifact cleanup: %w", err)
	}

	cleaned := 0
	var failures []error
	for _, item := range pendingRuns {
		if err := cleanup(item.runID, item.targets); err != nil {
			failures = append(failures, fmt.Errorf("clean archived run %s artifacts: %w", item.runID, err))
			continue
		}
		if err := d.markRunArtifactCleanupComplete(item.runID); err != nil {
			failures = append(failures, err)
			continue
		}
		cleaned++
	}
	return cleaned, errors.Join(failures...)
}

func (d *DB) markRunArtifactCleanupComplete(runID string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin run %s artifact cleanup completion: %w", runID, err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE run_metric_receipts SET artifact_cleanup_pending = 0 WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("mark run %s artifact cleanup complete: %w", runID, err)
	}
	if _, err := tx.Exec(`DELETE FROM run_artifact_cleanup_journal WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("delete run %s artifact cleanup journal: %w", runID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit run %s artifact cleanup completion: %w", runID, err)
	}
	return nil
}

func (d *DB) GetRunMetricReceipt(runID string) (*RunMetricReceipt, error) {
	receipt := &RunMetricReceipt{}
	var pullRequest int
	var stepStatsJSON, aggregatesJSON string
	err := d.sql.QueryRow(`
		SELECT run_id, repo_id, run_created_at, run_status, schema_version,
		       payload_json, receipt_sha256, archived_at, pull_request,
		       reported_findings, fixed_findings, step_stats_json, agent_aggregates_json
		FROM run_metric_receipts WHERE run_id = ?`, runID).Scan(
		&receipt.RunID, &receipt.RepoID, &receipt.RunCreatedAt, &receipt.RunStatus, &receipt.SchemaVersion,
		&receipt.PayloadJSON, &receipt.ReceiptSHA256, &receipt.ArchivedAt, &pullRequest,
		&receipt.ReportedFindings, &receipt.FixedFindings, &stepStatsJSON, &aggregatesJSON,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run metric receipt: %w", err)
	}
	receipt.PullRequest = pullRequest != 0
	if err := decodeAndValidateRunMetricReceipt(receipt, stepStatsJSON, aggregatesJSON); err != nil {
		return nil, err
	}
	return receipt, nil
}

func (d *DB) GetRunMetricReceipts() ([]RunMetricReceipt, error) {
	rows, err := d.sql.Query(`
		SELECT run_id, repo_id, run_created_at, run_status, schema_version,
		       payload_json, receipt_sha256, archived_at, pull_request,
		       reported_findings, fixed_findings, step_stats_json, agent_aggregates_json
		FROM run_metric_receipts ORDER BY run_created_at DESC, run_id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list run metric receipts: %w", err)
	}
	defer rows.Close()
	var receipts []RunMetricReceipt
	for rows.Next() {
		var receipt RunMetricReceipt
		var pullRequest int
		var stepStatsJSON, aggregatesJSON string
		if err := rows.Scan(
			&receipt.RunID, &receipt.RepoID, &receipt.RunCreatedAt, &receipt.RunStatus, &receipt.SchemaVersion,
			&receipt.PayloadJSON, &receipt.ReceiptSHA256, &receipt.ArchivedAt, &pullRequest,
			&receipt.ReportedFindings, &receipt.FixedFindings, &stepStatsJSON, &aggregatesJSON,
		); err != nil {
			return nil, fmt.Errorf("scan run metric receipt: %w", err)
		}
		receipt.PullRequest = pullRequest != 0
		if err := decodeAndValidateRunMetricReceipt(&receipt, stepStatsJSON, aggregatesJSON); err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run metric receipts: %w", err)
	}
	return receipts, nil
}

// HasRunMetricReceiptsForRepo reports whether an exact repository ID remains
// queryable after its registration and rich runs were removed.
func (d *DB) HasRunMetricReceiptsForRepo(repoID string) (bool, error) {
	var exists int
	if err := d.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM run_metric_receipts WHERE repo_id = ?)`, repoID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check repository metric receipts: %w", err)
	}
	return exists != 0, nil
}

func decodeAndValidateRunMetricReceipt(receipt *RunMetricReceipt, stepStatsJSON, aggregatesJSON string) error {
	digest, err := runMetricReceiptDigest(*receipt, stepStatsJSON, aggregatesJSON)
	if err != nil {
		return err
	}
	if digest != receipt.ReceiptSHA256 {
		return fmt.Errorf("run metric receipt %q failed SHA-256 verification", receipt.RunID)
	}
	if err := json.Unmarshal([]byte(stepStatsJSON), &receipt.StepStats); err != nil {
		return fmt.Errorf("decode run metric receipt step stats: %w", err)
	}
	if err := json.Unmarshal([]byte(aggregatesJSON), &receipt.AgentAggregates); err != nil {
		return fmt.Errorf("decode run metric receipt agent aggregates: %w", err)
	}
	receipt.StepStats = nonNilStepStats(receipt.StepStats)
	receipt.AgentAggregates = nonNilAgentAggregates(receipt.AgentAggregates)
	return nil
}

func runMetricReceiptDigest(receipt RunMetricReceipt, stepStatsJSON, aggregatesJSON string) (string, error) {
	canonical, err := json.Marshal(struct {
		RunID            string          `json:"run_id"`
		RepoID           string          `json:"repo_id"`
		RunCreatedAt     int64           `json:"run_created_at"`
		RunStatus        types.RunStatus `json:"run_status"`
		SchemaVersion    int             `json:"schema_version"`
		PayloadJSON      string          `json:"payload_json"`
		ArchivedAt       int64           `json:"archived_at"`
		PullRequest      bool            `json:"pull_request"`
		ReportedFindings int             `json:"reported_findings"`
		FixedFindings    int             `json:"fixed_findings"`
		StepStatsJSON    string          `json:"step_stats_json"`
		AggregatesJSON   string          `json:"agent_aggregates_json"`
	}{
		RunID: receipt.RunID, RepoID: receipt.RepoID, RunCreatedAt: receipt.RunCreatedAt, RunStatus: receipt.RunStatus,
		SchemaVersion: receipt.SchemaVersion, PayloadJSON: receipt.PayloadJSON, ArchivedAt: receipt.ArchivedAt,
		PullRequest: receipt.PullRequest, ReportedFindings: receipt.ReportedFindings, FixedFindings: receipt.FixedFindings,
		StepStatsJSON: stepStatsJSON, AggregatesJSON: aggregatesJSON,
	})
	if err != nil {
		return "", fmt.Errorf("encode run metric receipt digest: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func terminalRunStatus(status types.RunStatus) bool {
	switch status {
	case types.RunCompleted, types.RunFailed, types.RunCancelled:
		return true
	default:
		return false
	}
}

func nonNilStepStats(values []StepStats) []StepStats {
	if values == nil {
		return []StepStats{}
	}
	return values
}

func nonNilAgentAggregates(values []AgentInvocationAggregate) []AgentInvocationAggregate {
	if values == nil {
		return []AgentInvocationAggregate{}
	}
	return values
}
