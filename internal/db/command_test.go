package db

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCommandDefinitionsUseExactPortableIdentity(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/commands", "git@github.com:user/commands.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	versionA := "5.9"
	versionB := "5.10"
	first := runner.Resolved{
		Script:        "printf ' exact  bytes\\n'",
		Argv:          []string{"/bin/zsh", "-lc", "printf ' exact  bytes\\n'"},
		CommandSource: runner.SourceLinux,
		Provenance: runner.Provenance{
			SchemaVersion: runner.SchemaVersion,
			Platform:      "linux",
			Source:        runner.SourceInline,
			Executable:    "zsh",
			Args:          []string{"-lc"},
			Version:       &versionA,
		},
	}
	second := first
	second.Argv = []string{"/usr/local/bin/zsh", "-lc", first.Script}
	second.Provenance.Version = &versionB

	definitionA, err := d.EnsureCommandDefinition(run.ID, first)
	if err != nil {
		t.Fatalf("ensure first definition: %v", err)
	}
	definitionB, err := d.EnsureCommandDefinition(run.ID, second)
	if err != nil {
		t.Fatalf("ensure equivalent definition: %v", err)
	}
	if definitionA.ID != definitionB.ID {
		t.Fatalf("definition IDs differ by absolute executable path/version: %q != %q", definitionA.ID, definitionB.ID)
	}
	if definitionA.Script != first.Script {
		t.Fatalf("script = %q, want exact %q", definitionA.Script, first.Script)
	}

	changed := first
	changed.Script += " "
	definitionC, err := d.EnsureCommandDefinition(run.ID, changed)
	if err != nil {
		t.Fatalf("ensure changed definition: %v", err)
	}
	if definitionC.ID == definitionA.ID {
		t.Fatal("definition identity normalized meaningful script whitespace")
	}

	definitions, err := d.GetCommandDefinitionsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 2 {
		t.Fatalf("definitions = %d, want 2", len(definitions))
	}
	if !reflect.DeepEqual(definitionA.RunnerArgs, []string{"-lc"}) || definitionA.RunnerExecutable != "zsh" || definitionA.Platform != "linux" {
		t.Fatalf("definition provenance = %+v", definitionA)
	}
}

func TestCommandAttemptsPreserveOccurrenceProvenanceForSharedDefinition(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/provenance", "git@github.com:user/provenance.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepTest)
	round, _ := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 10)
	versionA := "5.9"
	versionB := "5.10"
	resolved := runner.Resolved{
		Script:        "go test ./...",
		CommandSource: runner.SourceBase,
		Provenance: runner.Provenance{
			SchemaVersion: runner.SchemaVersion,
			Platform:      "linux",
			Source:        runner.SourceDefault,
			Executable:    "sh",
			Args:          []string{"-c"},
			Version:       &versionA,
		},
	}
	definition, err := d.EnsureCommandDefinition(run.ID, resolved)
	if err != nil {
		t.Fatal(err)
	}
	planned := resolved
	planned.CommandSource = CommandDefinitionSourcePlanned
	planned.Provenance.Source = runner.SourcePortableDefault
	planned.Provenance.Version = &versionB
	plannedDefinition, err := d.EnsureCommandDefinition(run.ID, planned)
	if err != nil {
		t.Fatalf("reuse semantic definition with different occurrence provenance: %v", err)
	}
	if plannedDefinition.ID != definition.ID {
		t.Fatalf("definition IDs differ by occurrence provenance: %q != %q", plannedDefinition.ID, definition.ID)
	}
	definitions, err := d.GetCommandDefinitionsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 {
		t.Fatalf("definitions = %d, want one shared definition", len(definitions))
	}

	state := "git:head"
	for sequence, occurrence := range []runner.Resolved{resolved, planned} {
		attempt, err := d.StartCommandAttempt(CommandAttempt{
			RunID: run.ID, CommandID: definition.ID, StepID: step.ID, RoundID: round.ID,
			Sequence: sequence + 1, Purpose: "test", Observer: CommandObserverController,
			Trigger: "initial", BeforeSHA: "head", InputStateID: &state,
			CommandSource: occurrence.CommandSource, RunnerSchemaVersion: occurrence.Provenance.SchemaVersion,
			RunnerSource: occurrence.Provenance.Source, RunnerVersion: occurrence.Provenance.Version,
		})
		if err != nil {
			t.Fatalf("start occurrence %d: %v", sequence+1, err)
		}
		exit := 0
		if err := d.CompleteCommandAttempt(attempt.ID, CommandOutcomePass, &exit, nil, &state, stringPointer("head")); err != nil {
			t.Fatalf("complete occurrence %d: %v", sequence+1, err)
		}
	}

	attempts, err := d.GetCommandAttemptsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].CommandSource != runner.SourceBase || attempts[1].CommandSource != CommandDefinitionSourcePlanned || attempts[0].RunnerVersion == nil || *attempts[0].RunnerVersion != versionA || attempts[1].RunnerVersion == nil || *attempts[1].RunnerVersion != versionB {
		t.Fatalf("occurrence provenance = %+v", attempts)
	}
}

func TestCommandAttemptsRetainIdenticalExecutionsAndValidateRetries(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/attempts", "git@github.com:user/attempts.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepTest)
	round, _ := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 10)
	definition, err := d.EnsureCommandDefinition(run.ID, runner.Resolved{
		Script:        "go test ./internal/pipeline/...",
		CommandSource: runner.SourceBase,
		Provenance:    runner.Provenance{SchemaVersion: runner.SchemaVersion, Platform: "linux", Source: runner.SourceDefault, Executable: "sh", Args: []string{"-c"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := d.StartCommandAttempt(CommandAttempt{
		RunID: run.ID, CommandID: definition.ID, StepID: step.ID, RoundID: round.ID,
		Sequence: 1, Purpose: "test", Observer: CommandObserverController,
		Trigger: "initial", BeforeSHA: "head", InputStateID: stringPointer("git:head"),
		CommandSource: runner.SourceBase, RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDefault,
	})
	if err != nil {
		t.Fatalf("start first attempt: %v", err)
	}
	exitOne := 1
	if err := d.CompleteCommandAttempt(first.ID, CommandOutcomeFail, &exitOne, nil, stringPointer("git:head"), stringPointer("head")); err != nil {
		t.Fatalf("complete first attempt: %v", err)
	}

	retryReason := CommandRetryReasonUnchangedAfterRepair
	second, err := d.StartCommandAttempt(CommandAttempt{
		RunID: run.ID, CommandID: definition.ID, StepID: step.ID, RoundID: round.ID,
		Sequence: 2, Purpose: "test", Observer: CommandObserverController,
		Trigger: "initial", BeforeSHA: "head", InputStateID: stringPointer("git:head"),
		CommandSource: runner.SourceBase, RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDefault,
		RetryOfAttemptID: &first.ID, RetryReason: &retryReason,
	})
	if err != nil {
		t.Fatalf("start retry: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("identical executions were deduplicated")
	}
	exitZero := 0
	if err := d.CompleteCommandAttempt(second.ID, CommandOutcomePass, &exitZero, nil, stringPointer("git:head"), stringPointer("head")); err != nil {
		t.Fatalf("complete retry: %v", err)
	}

	attempts, err := d.GetCommandAttemptsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
	if attempts[0].Outcome == nil || *attempts[0].Outcome != CommandOutcomeFail || attempts[1].Outcome == nil || *attempts[1].Outcome != CommandOutcomePass {
		t.Fatalf("outcomes = %+v, %+v", attempts[0].Outcome, attempts[1].Outcome)
	}
	if attempts[1].RetryOfAttemptID == nil || *attempts[1].RetryOfAttemptID != first.ID || attempts[1].RetryReason == nil || *attempts[1].RetryReason != retryReason {
		t.Fatalf("retry provenance = %+v", attempts[1])
	}
	if attempts[0].StartedAt == 0 || attempts[0].CompletedAt == nil || attempts[0].DurationMS == nil {
		t.Fatalf("attempt timing = %+v", attempts[0])
	}

	_, err = d.StartCommandAttempt(CommandAttempt{
		RunID: run.ID, CommandID: definition.ID, StepID: step.ID, RoundID: round.ID,
		Sequence: 3, Purpose: "test", Observer: CommandObserverController,
		Trigger: "initial", BeforeSHA: "head", InputStateID: stringPointer("git:head"),
		CommandSource: runner.SourceBase, RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDefault,
		RetryOfAttemptID: &second.ID, RetryReason: &retryReason,
	})
	if err == nil || !strings.Contains(err.Error(), "outcome is not retryable") {
		t.Fatalf("passing-attempt retry error = %v", err)
	}

	third, err := d.StartCommandAttempt(CommandAttempt{
		RunID: run.ID, CommandID: definition.ID, StepID: step.ID, RoundID: round.ID,
		Sequence: 3, Purpose: "test", Observer: CommandObserverController,
		Trigger: "initial", BeforeSHA: "head", InputStateID: stringPointer("git:head"),
		CommandSource: runner.SourceBase, RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDefault,
	})
	if err != nil {
		t.Fatalf("start third attempt: %v", err)
	}
	if err := d.CompleteCommandAttempt(third.ID, CommandOutcomeTimeout, nil, nil, stringPointer("git:dirty"), nil); err != nil {
		t.Fatalf("complete third attempt: %v", err)
	}

	invalidReason := "operator_retry"
	_, err = d.StartCommandAttempt(CommandAttempt{
		RunID: run.ID, CommandID: definition.ID, StepID: step.ID, RoundID: round.ID,
		Sequence: 4, Purpose: "test", Observer: CommandObserverController,
		Trigger: "initial", BeforeSHA: "head", InputStateID: stringPointer("git:head"),
		CommandSource: runner.SourceBase, RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDefault,
		RetryOfAttemptID: &third.ID, RetryReason: &invalidReason,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported retry reason") {
		t.Fatalf("invalid retry reason error = %v", err)
	}

	_, err = d.StartCommandAttempt(CommandAttempt{
		RunID: run.ID, CommandID: definition.ID, StepID: step.ID, RoundID: round.ID,
		Sequence: 4, Purpose: "test", Observer: CommandObserverController,
		Trigger: "initial", BeforeSHA: "different-head", InputStateID: stringPointer("git:different-head"),
		CommandSource: runner.SourceBase, RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDefault,
		RetryOfAttemptID: &third.ID, RetryReason: &retryReason,
	})
	if err == nil || !strings.Contains(err.Error(), "unchanged subject") {
		t.Fatalf("changed-subject retry error = %v", err)
	}

	_, err = d.StartCommandAttempt(CommandAttempt{
		RunID: run.ID, CommandID: definition.ID, StepID: step.ID, RoundID: round.ID,
		Sequence: 4, Purpose: "test", Observer: CommandObserverController,
		Trigger: "initial", BeforeSHA: "head", InputStateID: stringPointer("git:head"),
		CommandSource: runner.SourceBase, RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDefault,
		RetryOfAttemptID: &third.ID, RetryReason: &retryReason,
	})
	if err == nil || !strings.Contains(err.Error(), "unchanged input state") {
		t.Fatalf("mutated-input retry error = %v", err)
	}
}

func TestOpenMigratesCommandReceiptTablesWithoutBackfillingLegacyRuns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE repos (id TEXT PRIMARY KEY, working_path TEXT NOT NULL UNIQUE, upstream_url TEXT NOT NULL, default_branch TEXT NOT NULL DEFAULT 'main', created_at INTEGER NOT NULL);
		CREATE TABLE runs (id TEXT PRIMARY KEY, repo_id TEXT NOT NULL, branch TEXT NOT NULL, head_sha TEXT NOT NULL, base_sha TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
		CREATE TABLE step_results (id TEXT PRIMARY KEY, run_id TEXT NOT NULL, step_name TEXT NOT NULL, step_order INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'pending');
		CREATE TABLE step_rounds (id TEXT PRIMARY KEY, step_result_id TEXT NOT NULL, round INTEGER NOT NULL, trigger_type TEXT NOT NULL, duration_ms INTEGER NOT NULL, created_at INTEGER NOT NULL);
		CREATE TABLE command_definitions (
			run_id TEXT NOT NULL, id TEXT NOT NULL, script TEXT NOT NULL, platform TEXT NOT NULL,
			runner_executable TEXT NOT NULL, runner_args_json TEXT NOT NULL,
			source TEXT NOT NULL, runner_schema_version INTEGER NOT NULL,
			runner_source TEXT NOT NULL, runner_version TEXT NOT NULL,
			PRIMARY KEY (run_id, id)
		);
		CREATE TABLE command_attempts (
			id TEXT PRIMARY KEY, run_id TEXT NOT NULL, command_id TEXT NOT NULL,
			step_id TEXT NOT NULL, round_id TEXT NOT NULL, sequence INTEGER NOT NULL,
			purpose TEXT NOT NULL, observer TEXT NOT NULL, trigger_type TEXT NOT NULL,
			before_sha TEXT NOT NULL, tested_sha TEXT, command_source TEXT NOT NULL,
			runner_schema_version INTEGER NOT NULL, runner_source TEXT NOT NULL,
			runner_version TEXT, input_state_id TEXT, result_state_id TEXT,
			started_at INTEGER NOT NULL, completed_at INTEGER, duration_ms INTEGER,
			outcome TEXT, exit_code INTEGER, signal TEXT, retry_of_attempt_id TEXT,
			retry_reason TEXT,
			FOREIGN KEY (run_id, command_id) REFERENCES command_definitions(run_id, id)
		);
		INSERT INTO repos VALUES ('repo', '/tmp/legacy-command-repo', 'upstream', 'main', 1);
		INSERT INTO runs VALUES ('run', 'repo', 'branch', 'head', 'base', 'pending', 1, 1);
		INSERT INTO step_results VALUES ('step', 'run', 'test', 1, 'pending');
		INSERT INTO step_rounds VALUES ('round', 'step', 1, 'initial', 1, 1);
		INSERT INTO command_definitions VALUES ('run', 'definition', 'printf test', 'linux', 'sh', '["-c"]', 'base', 1, 'default', '5.9');
		INSERT INTO command_attempts VALUES ('attempt', 'run', 'definition', 'step', 'round', 1, 'test', 'controller', 'initial', 'head', NULL, 'base', 1, 'default', '5.9', 'git:head', NULL, 1, NULL, NULL, NULL, NULL, NULL, NULL, NULL);
	`); err != nil {
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
	for _, table := range []string{"command_definitions", "command_attempts"} {
		var count int
		if err := database.sql.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("%s missing after migration: %v", table, err)
		}
	}
	var script string
	if err := database.sql.QueryRow(`SELECT script FROM command_definitions WHERE id = 'definition'`).Scan(&script); err != nil {
		t.Fatalf("migrated definition missing: %v", err)
	}
	if script != "printf test" {
		t.Fatalf("migrated definition script = %q", script)
	}
	var attemptCount int
	if err := database.sql.QueryRow(`SELECT count(*) FROM command_attempts WHERE command_id = 'definition'`).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if attemptCount != 1 {
		t.Fatalf("migrated attempts = %d, want 1", attemptCount)
	}
	var obsoleteCount int
	if err := database.sql.QueryRow(`SELECT count(*) FROM pragma_table_info('command_definitions') WHERE name IN ('source', 'runner_schema_version', 'runner_source', 'runner_version')`).Scan(&obsoleteCount); err != nil {
		t.Fatal(err)
	}
	if obsoleteCount != 0 {
		t.Fatalf("obsolete definition columns = %d, want 0", obsoleteCount)
	}
	var foreignKeyCount int
	if err := database.sql.QueryRow(`SELECT count(*) FROM pragma_foreign_key_list('command_attempts') WHERE "table" = 'command_definitions'`).Scan(&foreignKeyCount); err != nil {
		t.Fatal(err)
	}
	if foreignKeyCount == 0 {
		t.Fatal("command attempt foreign key was not preserved")
	}
	if _, err := Open(dbPath); err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
}

func stringPointer(value string) *string { return &value }
