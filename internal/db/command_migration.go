package db

import (
	"database/sql"
	"fmt"
)

const commandDefinitionProvenanceRemovalMigration = "command_definition_provenance_removal_v1"

var obsoleteCommandDefinitionColumns = []string{
	"source",
	"runner_schema_version",
	"runner_source",
	"runner_version",
}

// migrateCommandDefinitionProvenanceColumns removes the early Issue 112
// definition-level provenance fields exactly once. Occurrence provenance now
// belongs to command_attempts, and the marker prevents later opens from
// repeatedly rewriting the definition table.
func migrateCommandDefinitionProvenanceColumns(sqlDB *sql.DB) error {
	tx, err := sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("begin command definition provenance migration: %w", err)
	}
	defer tx.Rollback()

	var completed bool
	if err := tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name = ?)`,
		commandDefinitionProvenanceRemovalMigration,
	).Scan(&completed); err != nil {
		return fmt.Errorf("read command definition provenance migration marker: %w", err)
	}
	if completed {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit completed command definition provenance migration check: %w", err)
		}
		return nil
	}

	rows, err := tx.Query(`PRAGMA table_info('command_definitions')`)
	if err != nil {
		return fmt.Errorf("read command definition columns: %w", err)
	}
	present := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan command definition column: %w", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate command definition columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close command definition columns: %w", err)
	}

	backfills := map[string]string{
		"source": `UPDATE command_attempts
			SET command_source = (SELECT source FROM command_definitions
				WHERE command_definitions.run_id = command_attempts.run_id AND command_definitions.id = command_attempts.command_id)
			WHERE command_source = 'legacy' AND EXISTS (SELECT 1 FROM command_definitions
				WHERE command_definitions.run_id = command_attempts.run_id AND command_definitions.id = command_attempts.command_id)`,
		"runner_schema_version": `UPDATE command_attempts
			SET runner_schema_version = (SELECT runner_schema_version FROM command_definitions
				WHERE command_definitions.run_id = command_attempts.run_id AND command_definitions.id = command_attempts.command_id)
			WHERE runner_schema_version = 1 AND EXISTS (SELECT 1 FROM command_definitions
				WHERE command_definitions.run_id = command_attempts.run_id AND command_definitions.id = command_attempts.command_id)`,
		"runner_source": `UPDATE command_attempts
			SET runner_source = (SELECT runner_source FROM command_definitions
				WHERE command_definitions.run_id = command_attempts.run_id AND command_definitions.id = command_attempts.command_id)
			WHERE runner_source = 'legacy' AND EXISTS (SELECT 1 FROM command_definitions
				WHERE command_definitions.run_id = command_attempts.run_id AND command_definitions.id = command_attempts.command_id)`,
		"runner_version": `UPDATE command_attempts
			SET runner_version = (SELECT runner_version FROM command_definitions
				WHERE command_definitions.run_id = command_attempts.run_id AND command_definitions.id = command_attempts.command_id)
			WHERE runner_version IS NULL AND EXISTS (SELECT 1 FROM command_definitions
				WHERE command_definitions.run_id = command_attempts.run_id AND command_definitions.id = command_attempts.command_id)`,
	}
	for _, column := range obsoleteCommandDefinitionColumns {
		if !present[column] {
			continue
		}
		if _, err := tx.Exec(backfills[column]); err != nil {
			return fmt.Errorf("backfill command attempt column %s: %w", column, err)
		}
		if _, err := tx.Exec(`ALTER TABLE command_definitions DROP COLUMN ` + column); err != nil {
			return fmt.Errorf("drop command definition column %s: %w", column, err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (name) VALUES (?)`,
		commandDefinitionProvenanceRemovalMigration,
	); err != nil {
		return fmt.Errorf("record command definition provenance migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit command definition provenance migration: %w", err)
	}
	return nil
}
