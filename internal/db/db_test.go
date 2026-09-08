package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestOpenAndClose(t *testing.T) {
	d := openTestDB(t)
	if d == nil {
		t.Fatal("expected non-nil db")
	}
}

func TestOpenReadOnlyRequiresExistingDBAndDoesNotMigrate(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	if _, err := OpenReadOnly(missing); !os.IsNotExist(err) {
		t.Fatalf("OpenReadOnly missing error = %v, want not-exist", err)
	}
	path := filepath.Join(t.TempDir(), "state.sqlite")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close writable db: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	readonly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer readonly.Close()
	if _, err := readonly.GetRepos(); err != nil {
		t.Fatalf("read repos: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("read-only open changed DB size from %d to %d", before.Size(), after.Size())
	}
}

func TestOpenCreatesSchema(t *testing.T) {
	d := openTestDB(t)
	// verify tables exist by querying them
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM repos").Scan(&count); err != nil {
		t.Fatalf("repos table missing: %v", err)
	}
	if err := d.sql.QueryRow("SELECT count(*) FROM runs").Scan(&count); err != nil {
		t.Fatalf("runs table missing: %v", err)
	}
	if err := d.sql.QueryRow("SELECT count(*) FROM step_results").Scan(&count); err != nil {
		t.Fatalf("step_results table missing: %v", err)
	}
	if !hasColumn(t, d, "repos", "fork_url") {
		t.Fatal("repos.fork_url column missing from fresh schema")
	}
	for _, column := range []string{"refresh_strategy", "stacked_on", "config_sources_json", "resolved_agent_routing_json", "resolved_policy_json", "resolved_policy_digest", "intent", "intent_source", "intent_session_id", "intent_score", "submitted_head_sha", "no_mistakes_version", "no_mistakes_build_sha", "review_approved_head_sha", "last_pushed_sha", "push_target_fingerprint", "push_ref", "last_pushed_at", "push_generation", "push_active", "terminal_head_verified_at", "pr_state", "pr_state_observed_at", "ci_ready_at", "ci_ready_no_ci", "ci_rerun_state", "ci_fix_attempts", "custody_returned_at", "pr_note", "metadata"} {
		if !hasColumn(t, d, "runs", column) {
			t.Fatalf("runs.%s column missing from fresh schema", column)
		}
	}
	for _, column := range []string{"trigger_provenance", "reviewed_head_sha", "replay_config_json", "repair_failure_fingerprint", "repair_result", "resulting_head_sha", "evaluated_head_sha"} {
		if !hasColumn(t, d, "step_rounds", column) {
			t.Fatalf("step_rounds.%s column missing from fresh schema", column)
		}
	}
	for _, column := range []string{"last_activity_at", "last_activity", "agent_pid", "evidence_json", "planned_command", "skip_source"} {
		if !hasColumn(t, d, "step_results", column) {
			t.Fatalf("step_results.%s column missing from fresh schema", column)
		}
	}
	for _, column := range []string{"round_id", "delta_cache_creation_tokens", "reported_cost_usd"} {
		if !hasColumn(t, d, "agent_invocations", column) {
			t.Fatalf("agent_invocations.%s column missing from fresh schema", column)
		}
	}
	for _, table := range []string{"round_evaluations", "round_findings", "round_evaluation_artifacts", "round_decisions", "round_decision_findings", "round_repairs"} {
		if err := d.sql.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("%s table missing: %v", table, err)
		}
	}
	for _, tc := range []struct {
		table string
		index string
	}{
		{table: "command_attempts", index: "idx_command_attempts_proof_by_tested_sha"},
		{table: "agent_invocations", index: "idx_agent_invocations_round_started_id"},
	} {
		if !hasIndex(t, d, tc.table, tc.index) {
			t.Fatalf("%s index missing from fresh schema", tc.index)
		}
	}
	for _, tc := range []struct {
		table string
		index string
	}{
		{table: "command_attempts", index: "idx_command_attempts_retry_of"},
		{table: "command_attempts", index: "idx_command_attempts_output_artifact"},
		{table: "round_decision_findings", index: "idx_round_decision_findings_selection_ordinal"},
	} {
		if !hasUniquePartialIndex(t, d, tc.table, tc.index) {
			t.Fatalf("%s unique partial index missing from fresh schema", tc.index)
		}
	}
	for _, trigger := range []string{
		"validate_command_attempt_proof_state_insert",
		"validate_command_attempt_proof_state_update",
	} {
		if !hasTrigger(t, d, trigger) {
			t.Fatalf("%s trigger missing from fresh schema", trigger)
		}
	}
	if !hasColumn(t, d, "round_decision_findings", "selection_ordinal") {
		t.Fatal("round_decision_findings.selection_ordinal column missing from fresh schema")
	}
	for _, removed := range []string{"invocation_mode", "agent_observations_json", "nested_agent_count", "pricing_receipt_json"} {
		if hasColumn(t, d, "agent_invocations", removed) {
			t.Fatalf("agent_invocations.%s should be absent from fresh schema", removed)
		}
	}
}

func TestOpenClassifiesFreshSchemaInsideImmediateTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent-open.sqlite")
	hookCalled := false
	var competitorStarted bool
	beforeFreshSchemaInstall := func() error {
		hookCalled = true
		competitorSQL, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
		if err != nil {
			return fmt.Errorf("open competing database: %w", err)
		}
		defer competitorSQL.Close()
		competitorSQL.SetMaxOpenConns(1)

		ctx := context.Background()
		competitorConn, err := competitorSQL.Conn(ctx)
		if err != nil {
			return fmt.Errorf("connect competing database: %w", err)
		}
		defer competitorConn.Close()

		if _, err := competitorConn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			if !strings.Contains(err.Error(), "SQLITE_BUSY") {
				return fmt.Errorf("begin competing transaction: %w", err)
			}
			return nil
		}
		competitorStarted = true
		if _, err := competitorConn.ExecContext(ctx, `
			CREATE TABLE repos (
				id TEXT PRIMARY KEY,
				working_path TEXT NOT NULL UNIQUE,
				upstream_url TEXT NOT NULL,
				default_branch TEXT NOT NULL DEFAULT 'main',
				created_at INTEGER NOT NULL
			)`); err != nil {
			_, _ = competitorConn.ExecContext(ctx, "ROLLBACK")
			return fmt.Errorf("create competing legacy schema: %w", err)
		}
		if _, err := competitorConn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("commit competing legacy schema: %w", err)
		}
		return nil
	}

	d, err := open(path, beforeFreshSchemaInstall)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if !hookCalled {
		t.Fatal("fresh-schema hook was not called")
	}
	if competitorStarted {
		t.Fatal("competing opener acquired the schema transaction")
	}
	if !hasColumn(t, d, "repos", "fork_url") {
		t.Fatal("repos.fork_url column missing after concurrent open")
	}

	ordinaryTx, err := d.sql.Begin()
	if err != nil {
		t.Fatalf("begin ordinary transaction: %v", err)
	}
	competitorSQL, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		ordinaryTx.Rollback()
		t.Fatalf("open ordinary transaction competitor: %v", err)
	}
	competitorSQL.SetMaxOpenConns(1)
	t.Cleanup(func() { competitorSQL.Close() })
	competitorConn, err := competitorSQL.Conn(context.Background())
	if err != nil {
		ordinaryTx.Rollback()
		t.Fatalf("connect ordinary transaction competitor: %v", err)
	}
	t.Cleanup(func() { competitorConn.Close() })
	if _, err := competitorConn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		ordinaryTx.Rollback()
		t.Fatalf("ordinary transaction unexpectedly acquired an immediate lock: %v", err)
	}
	if _, err := competitorConn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		ordinaryTx.Rollback()
		t.Fatalf("rollback ordinary transaction competitor: %v", err)
	}
	if err := ordinaryTx.Rollback(); err != nil {
		t.Fatalf("rollback ordinary transaction: %v", err)
	}
}

func TestOpenMigratesCIFixAttemptsWithoutGrantingLegacyRunsAFreshBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-ci-fix.sqlite")
	before, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := before.InsertRepo("/home/user/legacy-ci-fix", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	legacyRun, err := before.InsertRun(repo.ID, "legacy", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE runs DROP COLUMN ci_fix_attempts`); err != nil {
		raw.Close()
		t.Fatalf("remove post-legacy column: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { after.Close() })
	var migrated sql.NullInt64
	if err := after.sql.QueryRow(`SELECT ci_fix_attempts FROM runs WHERE id = ?`, legacyRun.ID).Scan(&migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.Valid {
		t.Fatalf("migrated legacy CI fix attempts = %d, want unknown", migrated.Int64)
	}

	newRun, err := after.InsertRun(repo.ID, "new", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	var initialized sql.NullInt64
	if err := after.sql.QueryRow(`SELECT ci_fix_attempts FROM runs WHERE id = ?`, newRun.ID).Scan(&initialized); err != nil {
		t.Fatal(err)
	}
	if !initialized.Valid || initialized.Int64 != 0 {
		t.Fatalf("new run CI fix attempts = %+v, want known zero", initialized)
	}
}

func TestOpenMigratesPRContractV3PersistenceColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE runs (
			id TEXT PRIMARY KEY, repo_id TEXT NOT NULL, branch TEXT NOT NULL,
			head_sha TEXT NOT NULL, base_sha TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
			pr_url TEXT, error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		);
		CREATE TABLE step_results (
			id TEXT PRIMARY KEY, run_id TEXT NOT NULL, step_name TEXT NOT NULL,
			step_order INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'pending', exit_code INTEGER,
			duration_ms INTEGER, log_path TEXT, findings_json TEXT, error TEXT,
			started_at INTEGER, completed_at INTEGER
		);
		CREATE TABLE agent_invocations (
			id TEXT PRIMARY KEY, run_id TEXT NOT NULL, step_name TEXT NOT NULL, round INTEGER NOT NULL,
			purpose TEXT NOT NULL, agent TEXT NOT NULL, invocation_mode TEXT NOT NULL DEFAULT 'harness_cli',
			agent_observations_json TEXT, nested_agent_count INTEGER, model TEXT, model_provider TEXT, session_mode TEXT NOT NULL,
			session_key TEXT, fallback_reason TEXT, started_at INTEGER NOT NULL, completed_at INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL, subprocess_wait_ms INTEGER, exit_status TEXT NOT NULL,
			failure_category TEXT, input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER,
			cache_creation_tokens INTEGER, fresh_input_tokens INTEGER, reasoning_tokens INTEGER,
			delta_input_tokens INTEGER, delta_output_tokens INTEGER, delta_cache_read_tokens INTEGER,
			model_roundtrips INTEGER, tool_calls INTEGER, tool_wait_calls INTEGER,
			tool_test_lint_calls INTEGER, tool_edit_calls INTEGER, tool_read_calls INTEGER,
			tool_git_calls INTEGER, tool_other_calls INTEGER, workload_files INTEGER,
			workload_lines INTEGER, finding_count INTEGER
		);
	`)
	if err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	for table, columns := range map[string][]string{
		"runs":              {"metadata"},
		"step_results":      {"evidence_json", "planned_command", "skip_source"},
		"agent_invocations": {"delta_cache_creation_tokens", "reported_cost_usd"},
	} {
		for _, column := range columns {
			if !hasColumn(t, database, table, column) {
				t.Fatalf("%s.%s was not migrated", table, column)
			}
		}
	}
	for _, removed := range []string{"invocation_mode", "agent_observations_json", "nested_agent_count", "pricing_receipt_json"} {
		if hasColumn(t, database, "agent_invocations", removed) {
			t.Fatalf("agent_invocations.%s was not removed", removed)
		}
	}
}

func TestOpenMigratesRunSyncProvenanceWithoutBackfillingMutableHead(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE repos (id TEXT PRIMARY KEY, working_path TEXT NOT NULL UNIQUE, upstream_url TEXT NOT NULL, default_branch TEXT NOT NULL DEFAULT 'main', created_at INTEGER NOT NULL);
		CREATE TABLE runs (id TEXT PRIMARY KEY, repo_id TEXT NOT NULL, branch TEXT NOT NULL, head_sha TEXT NOT NULL, base_sha TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', pr_url TEXT, error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
		INSERT INTO repos VALUES ('repo-1', '/work/repo', 'https://example.com/repo.git', 'main', 1);
		INSERT INTO runs VALUES ('run-1', 'repo-1', 'feature', 'mutable-head', 'base', 'completed', NULL, NULL, 1, 1);
	`)
	if err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	run, err := d.GetRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.HeadSHA != "mutable-head" {
		t.Fatalf("migrated run = %#v", run)
	}
	if run.SubmittedHeadSHA != nil || run.NoMistakesVersion != nil || run.NoMistakesBuildSHA != nil || run.ReviewApprovedHeadSHA != nil || run.LastPushedSHA != nil || run.PushGeneration != nil || run.PushTargetFingerprint != nil {
		t.Fatalf("legacy provenance, build identity, or review authority was inferred from mutable head: %#v", run)
	}
	if run.CustodyReturnedAt != nil {
		t.Fatalf("legacy run gained a custody-return stamp: %#v", run)
	}
	if run.RefreshStrategy != "rebase" || run.StackedOn != "" {
		t.Fatalf("legacy refresh selection = (%q, %q), want default rebase", run.RefreshStrategy, run.StackedOn)
	}
	if len(run.ConfigSources) != 0 {
		t.Fatalf("legacy run config sources = %#v, want empty", run.ConfigSources)
	}
	if run.ResolvedAgentRouting != nil {
		t.Fatalf("legacy run gained resolved routing snapshot: %q", *run.ResolvedAgentRouting)
	}
	if run.ResolvedPolicy != nil || run.ResolvedPolicyDigest != nil {
		t.Fatalf("legacy run gained resolved policy: policy %v digest %v", run.ResolvedPolicy, run.ResolvedPolicyDigest)
	}
	if run.Metadata != nil {
		t.Fatalf("legacy run gained metadata: %q", *run.Metadata)
	}
}

func TestOpenCreatesStepRoundsTable(t *testing.T) {
	d := openTestDB(t)
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM step_rounds").Scan(&count); err != nil {
		t.Fatalf("step_rounds table missing: %v", err)
	}
}

func TestOpenMigratesExistingStepRoundsColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")

	legacyDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE step_rounds (
			id TEXT PRIMARY KEY,
			step_result_id TEXT NOT NULL,
			round INTEGER NOT NULL,
			trigger_type TEXT NOT NULL,
			findings_json TEXT,
			duration_ms INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		);
	`); err != nil {
		legacyDB.Close()
		t.Fatalf("create legacy step_rounds table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	rows, err := d.sql.Query(`PRAGMA table_info(step_rounds)`)
	if err != nil {
		t.Fatalf("pragma table_info(step_rounds): %v", err)
	}
	defer rows.Close()

	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name string
		var colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info: %v", err)
	}

	for _, name := range []string{"selected_finding_ids", "selection_source", "fix_summary", "reviewed_head_sha", "replay_config_json", "repair_failure_fingerprint", "repair_result"} {
		if !columns[name] {
			t.Fatalf("expected migrated column %q to exist", name)
		}
	}
}

func TestOpenMigratesReposForkURLColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")

	legacyDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE repos (
			id TEXT PRIMARY KEY,
			working_path TEXT NOT NULL UNIQUE,
			upstream_url TEXT NOT NULL,
			default_branch TEXT NOT NULL DEFAULT 'main',
			created_at INTEGER NOT NULL
		);
		INSERT INTO repos (id, working_path, upstream_url, default_branch, created_at)
		VALUES ('repo-1', '/work/repo', 'git@github.com:parent/repo.git', 'main', 123);
	`); err != nil {
		legacyDB.Close()
		t.Fatalf("create legacy repos table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if !hasColumn(t, d, "repos", "fork_url") {
		t.Fatal("expected migrated fork_url column")
	}
	repo, err := d.GetRepo("repo-1")
	if err != nil {
		t.Fatalf("get migrated repo: %v", err)
	}
	if repo == nil {
		t.Fatal("expected migrated repo")
	}
	if repo.ForkURL != "" {
		t.Fatalf("fork url = %q, want empty", repo.ForkURL)
	}
	updated, err := d.UpdateRepoForkURL(repo.ID, "git@github.com:fork/repo.git")
	if err != nil {
		t.Fatalf("update migrated fork URL: %v", err)
	}
	if updated.ForkURL != "git@github.com:fork/repo.git" {
		t.Fatalf("fork url after update = %q, want fork URL", updated.ForkURL)
	}
}

func TestOpenMigratesStepActivityColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")

	legacyDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE step_results (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			step_name TEXT NOT NULL,
			step_order INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			exit_code INTEGER,
			duration_ms INTEGER,
			log_path TEXT,
			findings_json TEXT,
			error TEXT,
			started_at INTEGER,
			completed_at INTEGER
		);
	`); err != nil {
		legacyDB.Close()
		t.Fatalf("create legacy step_results table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	for _, column := range []string{"last_activity_at", "last_activity", "agent_pid"} {
		if !hasColumn(t, d, "step_results", column) {
			t.Fatalf("expected migrated column %q", column)
		}
	}
}

func hasColumn(t *testing.T, d *DB, table, column string) bool {
	t.Helper()
	rows, err := d.sql.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info: %v", err)
	}
	return false
}

func hasUniquePartialIndex(t *testing.T, d *DB, table, index string) bool {
	t.Helper()
	rows, err := d.sql.Query(`SELECT name, "unique", partial FROM pragma_index_list(?)`, table)
	if err != nil {
		t.Fatalf("pragma index_list(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var unique, partial int
		if err := rows.Scan(&name, &unique, &partial); err != nil {
			t.Fatalf("scan index_list: %v", err)
		}
		if name == index && unique != 0 && partial != 0 {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index_list: %v", err)
	}
	return false
}

func hasIndex(t *testing.T, d *DB, table, index string) bool {
	t.Helper()
	rows, err := d.sql.Query(`SELECT name FROM pragma_index_list(?)`, table)
	if err != nil {
		t.Fatalf("pragma index_list(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index_list: %v", err)
		}
		if name == index {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index_list: %v", err)
	}
	return false
}

func hasTrigger(t *testing.T, d *DB, trigger string) bool {
	t.Helper()
	var count int
	if err := d.sql.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`, trigger).Scan(&count); err != nil {
		t.Fatalf("find trigger %s: %v", trigger, err)
	}
	return count != 0
}

func TestOpenWaitsForTransientMigrationLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	locker, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open locker db: %v", err)
	}
	defer locker.Close()
	if _, err := locker.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("begin exclusive lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		d, err := Open(dbPath)
		if err == nil {
			err = d.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Open returned before the migration lock was released")
		}
		t.Fatalf("Open should wait for a transient migration lock, got: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := locker.Exec("COMMIT"); err != nil {
		t.Fatalf("commit exclusive lock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Open after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open did not finish after the migration lock was released")
	}
}
