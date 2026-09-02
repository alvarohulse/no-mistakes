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

	for _, column := range obsoleteCommandDefinitionColumns {
		if !present[column] {
			continue
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
