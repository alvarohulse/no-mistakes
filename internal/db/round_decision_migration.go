package db

import (
	"context"
	"database/sql"
	"fmt"
)

const roundDecisionSourcesMigration = "round_decision_sources_v1"

func migrateRoundDecisionSources(sqlDB *sql.DB) error {
	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect for round decision migration: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin round decision migration: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()

	var complete bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name = ?)`, roundDecisionSourcesMigration).Scan(&complete); err != nil {
		return fmt.Errorf("read round decision migration marker: %w", err)
	}

	rows, err := conn.QueryContext(ctx, `PRAGMA table_info('round_decisions')`)
	if err != nil {
		return fmt.Errorf("read round decision columns: %w", err)
	}
	terminalSourcePresent := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan round decision column: %w", err)
		}
		if name == "terminal_source" {
			terminalSourcePresent = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate round decision columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close round decision columns: %w", err)
	}

	if !terminalSourcePresent {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE round_decisions ADD COLUMN terminal_source TEXT CHECK (terminal_source IS NULL OR terminal_source IN ('user_skipped', 'user_aborted'))`); err != nil {
			return fmt.Errorf("add round decision terminal source: %w", err)
		}
	}
	if !complete {
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations (name) VALUES (?)`, roundDecisionSourcesMigration); err != nil {
			return fmt.Errorf("record round decision migration: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit round decision migration: %w", err)
	}
	rollback = false
	return nil
}
