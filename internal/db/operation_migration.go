package db

import (
	"database/sql"
	"fmt"
	"strings"
)

func migrateRefreshOperationSchema(sqlDB *sql.DB) error {
	var createSQL string
	if err := sqlDB.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'refresh_operations'`).Scan(&createSQL); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("inspect refresh operation schema: %w", err)
	}

	var hasResolvedTargetHead bool
	rows, err := sqlDB.Query(`PRAGMA table_info(refresh_operations)`)
	if err != nil {
		return fmt.Errorf("inspect refresh operation columns: %w", err)
	}
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan refresh operation columns: %w", err)
		}
		if name == "resolved_target_head_sha" {
			hasResolvedTargetHead = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate refresh operation columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close refresh operation columns: %w", err)
	}
	if hasResolvedTargetHead && strings.Contains(strings.ToLower(createSQL), "'cancelled'") {
		return nil
	}

	tx, err := sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("begin refresh operation migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE refresh_operations_new (
		operation_id TEXT PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
		strategy TEXT NOT NULL CHECK (strategy IN ('rebase', 'merge')),
		source_ref TEXT NOT NULL,
		destination_ref TEXT NOT NULL,
		authoritative_base_ref TEXT NOT NULL,
		authoritative_base_sha TEXT,
		starting_head_sha TEXT,
		resolved_target_head_sha TEXT,
		decision TEXT NOT NULL CHECK (decision IN ('skipped', 'fast-forwarded', 'rebased', 'merged', 'conflicted', 'repaired', 'refused', 'error', 'cancelled')),
		resulting_head_sha TEXT,
		conflict_state TEXT NOT NULL CHECK (conflict_state IN ('none', 'detected', 'resolved')),
		repair_state TEXT NOT NULL CHECK (repair_state IN ('not_needed', 'not_attempted', 'succeeded', 'failed'))
	)`); err != nil {
		return fmt.Errorf("create refreshed operation table: %w", err)
	}
	resolvedColumn := "NULL"
	if hasResolvedTargetHead {
		resolvedColumn = "resolved_target_head_sha"
	}
	if _, err := tx.Exec(`INSERT INTO refresh_operations_new
		(operation_id, strategy, source_ref, destination_ref, authoritative_base_ref, authoritative_base_sha,
		 starting_head_sha, resolved_target_head_sha, decision, resulting_head_sha, conflict_state, repair_state)
		SELECT operation_id, strategy, source_ref, destination_ref, authoritative_base_ref, authoritative_base_sha,
		 starting_head_sha, ` + resolvedColumn + `, decision, resulting_head_sha, conflict_state, repair_state
		FROM refresh_operations`); err != nil {
		return fmt.Errorf("copy refresh operation rows: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE refresh_operations`); err != nil {
		return fmt.Errorf("drop old refresh operation table: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE refresh_operations_new RENAME TO refresh_operations`); err != nil {
		return fmt.Errorf("rename refreshed operation table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit refresh operation migration: %w", err)
	}
	return nil
}
