package db

import (
	"context"
	"database/sql"
	"fmt"
)

const roundDecisionSourcesMigration = "round_decision_sources_v1"

func migrateRoundDecisionSources(sqlDB *sql.DB) (migrationErr error) {
	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect for round decision migration: %w", err)
	}
	defer conn.Close()

	var complete bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name = ?)`, roundDecisionSourcesMigration).Scan(&complete); err != nil {
		return fmt.Errorf("read round decision migration marker: %w", err)
	}
	if complete {
		return nil
	}

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for round decision migration: %w", err)
	}
	defer func() {
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil && migrationErr == nil {
			migrationErr = fmt.Errorf("restore foreign keys after round decision migration: %w", err)
		}
	}()

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin round decision migration: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()

	for _, statement := range []string{
		`CREATE TABLE round_decisions_rebuilt (
			id             TEXT PRIMARY KEY,
			run_id         TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
			round_id       TEXT NOT NULL UNIQUE REFERENCES step_rounds(id) ON DELETE CASCADE,
			source         TEXT NOT NULL CHECK (source IN ('user', 'auto_fix', 'user_declined', 'user_skipped', 'user_aborted')),
			explicit_empty INTEGER NOT NULL DEFAULT 0,
			created_at     INTEGER NOT NULL
		)`,
		`INSERT INTO round_decisions_rebuilt (id, run_id, round_id, source, explicit_empty, created_at)
			SELECT id, run_id, round_id, source, explicit_empty, created_at FROM round_decisions`,
		`DROP TABLE round_decisions`,
		`ALTER TABLE round_decisions_rebuilt RENAME TO round_decisions`,
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rebuild round decisions: %w", err)
		}
	}

	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check round decision migration foreign keys: %w", err)
	}
	if rows.Next() {
		var table, parent string
		var rowID, foreignKeyID int
		err = rows.Scan(&table, &rowID, &parent, &foreignKeyID)
		rows.Close()
		if err != nil {
			return fmt.Errorf("scan round decision migration foreign key check: %w", err)
		}
		return fmt.Errorf("round decision migration foreign key violation in %s row %d referencing %s key %d", table, rowID, parent, foreignKeyID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate round decision migration foreign key check: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close round decision migration foreign key check: %w", err)
	}

	if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations (name) VALUES (?)`, roundDecisionSourcesMigration); err != nil {
		return fmt.Errorf("record round decision migration: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit round decision migration: %w", err)
	}
	rollback = false
	return nil
}
