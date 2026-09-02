package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

type runMetricReceiptMigrationRow struct {
	receipt        RunMetricReceipt
	stepStatsJSON  string
	aggregatesJSON string
}

func migrateRunMetricReceipts(sqlDB *sql.DB) error {
	tx, err := sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("begin run metric receipt migration: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT run_id, repo_id, run_created_at, run_status, schema_version,
		       payload_json, receipt_sha256, archived_at, pull_request,
		       reported_findings, fixed_findings, step_stats_json, agent_aggregates_json
		FROM run_metric_receipts ORDER BY run_id`)
	if err != nil {
		return fmt.Errorf("read run metric receipts for migration: %w", err)
	}
	var receipts []runMetricReceiptMigrationRow
	for rows.Next() {
		var row runMetricReceiptMigrationRow
		var pullRequest int
		if err := rows.Scan(
			&row.receipt.RunID, &row.receipt.RepoID, &row.receipt.RunCreatedAt, &row.receipt.RunStatus, &row.receipt.SchemaVersion,
			&row.receipt.PayloadJSON, &row.receipt.ReceiptSHA256, &row.receipt.ArchivedAt, &pullRequest,
			&row.receipt.ReportedFindings, &row.receipt.FixedFindings, &row.stepStatsJSON, &row.aggregatesJSON,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan run metric receipt for migration: %w", err)
		}
		row.receipt.PullRequest = pullRequest != 0
		receipts = append(receipts, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate run metric receipts for migration: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close run metric receipts for migration: %w", err)
	}

	for _, row := range receipts {
		digest, err := runMetricReceiptDigest(row.receipt, row.stepStatsJSON, row.aggregatesJSON)
		if err != nil {
			return err
		}
		if digest != row.receipt.ReceiptSHA256 {
			return fmt.Errorf("run metric receipt %q failed SHA-256 verification", row.receipt.RunID)
		}
		targetVersion := row.receipt.SchemaVersion
		switch {
		case row.receipt.SchemaVersion == 1:
		case row.receipt.SchemaVersion >= 2 && row.receipt.SchemaVersion < RunMetricReceiptSchemaVersion:
			targetVersion = RunMetricReceiptSchemaVersion
		case row.receipt.SchemaVersion == RunMetricReceiptSchemaVersion:
		default:
			continue
		}

		payload, changed, err := sanitizeRunMetricReceiptPayload(row.receipt.PayloadJSON, targetVersion)
		if err != nil {
			return fmt.Errorf("sanitize run metric receipt %q: %w", row.receipt.RunID, err)
		}
		if row.receipt.SchemaVersion != targetVersion {
			changed = true
		}
		if !changed {
			continue
		}

		migrated := row.receipt
		migrated.SchemaVersion = targetVersion
		migrated.PayloadJSON = payload
		migrated.ReceiptSHA256, err = runMetricReceiptDigest(migrated, row.stepStatsJSON, row.aggregatesJSON)
		if err != nil {
			return err
		}
		result, err := tx.Exec(`
			UPDATE run_metric_receipts
			SET schema_version = ?, payload_json = ?, receipt_sha256 = ?
			WHERE run_id = ? AND receipt_sha256 = ?`,
			migrated.SchemaVersion, migrated.PayloadJSON, migrated.ReceiptSHA256,
			migrated.RunID, row.receipt.ReceiptSHA256,
		)
		if err != nil {
			return fmt.Errorf("update run metric receipt %q: %w", migrated.RunID, err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read run metric receipt %q migration result: %w", migrated.RunID, err)
		}
		if updated != 1 {
			return fmt.Errorf("update run metric receipt %q: expected 1 row, updated %d", migrated.RunID, updated)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit run metric receipt migration: %w", err)
	}
	return nil
}

func sanitizeRunMetricReceiptPayload(payloadJSON string, schemaVersion int) (string, bool, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return "", false, fmt.Errorf("decode payload: %w", err)
	}
	if payload == nil {
		return "", false, fmt.Errorf("decode payload: top-level object is required")
	}

	changed := false
	var embeddedVersion int
	if raw, exists := payload["schema_version"]; !exists || json.Unmarshal(raw, &embeddedVersion) != nil || embeddedVersion != schemaVersion {
		versionJSON, err := json.Marshal(schemaVersion)
		if err != nil {
			return "", false, fmt.Errorf("encode schema version: %w", err)
		}
		payload["schema_version"] = versionJSON
		changed = true
	}
	if _, exists := payload["costs"]; exists {
		delete(payload, "costs")
		changed = true
	}

	if rawInvocations, exists := payload["invocations"]; exists {
		var invocations []json.RawMessage
		if err := json.Unmarshal(rawInvocations, &invocations); err != nil {
			return "", false, fmt.Errorf("decode invocations: %w", err)
		}
		invocationsChanged := false
		for index, rawInvocation := range invocations {
			var invocation map[string]json.RawMessage
			if err := json.Unmarshal(rawInvocation, &invocation); err != nil {
				return "", false, fmt.Errorf("decode invocation %d: %w", index+1, err)
			}
			if invocation == nil {
				continue
			}
			if _, exists := invocation["costs"]; !exists {
				continue
			}
			delete(invocation, "costs")
			encoded, err := json.Marshal(invocation)
			if err != nil {
				return "", false, fmt.Errorf("encode invocation %d: %w", index+1, err)
			}
			invocations[index] = encoded
			invocationsChanged = true
		}
		if invocationsChanged {
			encoded, err := json.Marshal(invocations)
			if err != nil {
				return "", false, fmt.Errorf("encode invocations: %w", err)
			}
			payload["invocations"] = encoded
			changed = true
		}
	}

	if !changed {
		return payloadJSON, false, nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", false, fmt.Errorf("encode payload: %w", err)
	}
	return string(encoded), true, nil
}
