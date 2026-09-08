package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func migrateOperationKinds(sqlDB *sql.DB) (returnErr error) {
	var createSQL string
	if err := sqlDB.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'operations'`).Scan(&createSQL); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("inspect operation schema: %w", err)
	}
	if strings.Contains(strings.ToLower(createSQL), "'push'") {
		return nil
	}

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect for operation migration: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for operation migration: %w", err)
	}
	defer func() {
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("reenable foreign keys after operation migration: %w", err)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin operation migration: %w", err)
	}
	defer tx.Rollback()
	statements := []string{
		`DROP TRIGGER IF EXISTS validate_refresh_operation_scope_insert`,
		`DROP TRIGGER IF EXISTS validate_refresh_operation_scope_update`,
		`DROP TRIGGER IF EXISTS validate_push_operation_scope_insert`,
		`DROP TRIGGER IF EXISTS validate_push_operation_scope_update`,
		`DROP TRIGGER IF EXISTS validate_refresh_operation_diagnostic_insert`,
		`DROP TRIGGER IF EXISTS validate_refresh_operation_diagnostic_update`,
		`DROP TRIGGER IF EXISTS validate_artifact_operation_scope_insert`,
		`DROP TRIGGER IF EXISTS validate_artifact_operation_scope_update`,
		`DROP TRIGGER IF EXISTS validate_operation_command_attempt_insert`,
		`DROP TRIGGER IF EXISTS validate_operation_command_attempt_update`,
		`CREATE TABLE operations_push_migration (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
			kind TEXT NOT NULL CHECK (kind IN ('refresh', 'push')),
			step_id TEXT NOT NULL REFERENCES step_results(id) ON DELETE CASCADE,
			round_id TEXT NOT NULL REFERENCES step_rounds(id) ON DELETE CASCADE,
			started_at INTEGER NOT NULL CHECK (started_at > 0),
			completed_at INTEGER NOT NULL CHECK (completed_at >= started_at),
			duration_ms INTEGER NOT NULL CHECK (duration_ms >= 0),
			diagnostic_artifact_id TEXT,
			UNIQUE (run_id, id),
			FOREIGN KEY (run_id, diagnostic_artifact_id) REFERENCES artifacts(run_id, id)
		)`,
		`INSERT INTO operations_push_migration
			(id, run_id, kind, step_id, round_id, started_at, completed_at, duration_ms, diagnostic_artifact_id)
		 SELECT id, run_id, kind, step_id, round_id, started_at, completed_at, duration_ms, diagnostic_artifact_id
		 FROM operations`,
		`DROP TABLE operations`,
		`ALTER TABLE operations_push_migration RENAME TO operations`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rebuild operation schema: %w", err)
		}
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check operation migration foreign keys: %w", err)
	}
	if rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var foreignKeyID int
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			rows.Close()
			return fmt.Errorf("scan operation migration foreign key violation: %w", err)
		}
		rows.Close()
		return fmt.Errorf("operation migration left a foreign key violation in %s referencing %s", table, parent)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate operation migration foreign key check: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close operation migration foreign key check: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit operation migration: %w", err)
	}
	return nil
}

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
