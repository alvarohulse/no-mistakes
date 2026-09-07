package db

const schemaSQL = `
CREATE TABLE IF NOT EXISTS repos (
    id             TEXT PRIMARY KEY,
    working_path   TEXT NOT NULL UNIQUE,
    upstream_url   TEXT NOT NULL,
    fork_url       TEXT,
    default_branch TEXT NOT NULL DEFAULT 'main',
    created_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS repo_eject_claims (
    repo_id     TEXT PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    claimed_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    id                   TEXT PRIMARY KEY,
    repo_id              TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch               TEXT NOT NULL,
    head_sha                TEXT NOT NULL,
    base_sha                TEXT NOT NULL,
    refresh_strategy        TEXT NOT NULL DEFAULT 'rebase',
    stacked_on              TEXT,
    config_sources_json     TEXT NOT NULL DEFAULT '[]',
    resolved_agent_routing_json TEXT,
    resolved_policy_json    TEXT,
    resolved_policy_digest  TEXT,
    intent                 TEXT,
    intent_source          TEXT,
    intent_session_id      TEXT,
    intent_score           REAL,
    submitted_head_sha      TEXT,
    no_mistakes_version     TEXT,
    no_mistakes_build_sha   TEXT,
    review_approved_head_sha TEXT,
    status                  TEXT NOT NULL DEFAULT 'pending',
	pinned_at               INTEGER,
    pr_url                  TEXT,
    pr_state                TEXT,
    pr_state_observed_at    INTEGER,
    ci_ready_at             INTEGER,
    ci_ready_no_ci          INTEGER NOT NULL DEFAULT 0,
    ci_rerun_state          TEXT,
    ci_fix_attempts         INTEGER,
    last_pushed_sha         TEXT,
    push_target_kind        TEXT,
    push_target_fingerprint TEXT,
    push_ref                TEXT,
    last_pushed_at          INTEGER,
    push_generation         INTEGER,
    push_active             INTEGER NOT NULL DEFAULT 0,
    terminal_head_verified_at INTEGER,
    custody_returned_at     INTEGER,
    error                   TEXT,
    awaiting_agent_since INTEGER,
    parked_ms            INTEGER,
	metadata             TEXT,
    pr_note             TEXT,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);

CREATE TRIGGER IF NOT EXISTS prevent_run_during_repo_eject
BEFORE INSERT ON runs
WHEN EXISTS (SELECT 1 FROM repo_eject_claims WHERE repo_id = NEW.repo_id)
BEGIN
    SELECT RAISE(ABORT, 'repository eject in progress');
END;

CREATE TABLE IF NOT EXISTS step_results (
    id               TEXT PRIMARY KEY,
    run_id           TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name        TEXT NOT NULL,
    step_order       INTEGER NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending',
    skip_source      TEXT,
    exit_code        INTEGER,
    duration_ms      INTEGER,
    log_path         TEXT,
    findings_json    TEXT,
    evidence_json    TEXT,
    planned_command  TEXT,
    error            TEXT,
    started_at       INTEGER,
    completed_at     INTEGER,
    last_activity_at INTEGER,
    last_activity    TEXT,
    agent_pid        INTEGER,
    auto_fix_limit   INTEGER
);

CREATE TABLE IF NOT EXISTS step_rounds (
    id                   TEXT PRIMARY KEY,
    step_result_id       TEXT NOT NULL REFERENCES step_results(id) ON DELETE CASCADE,
    round                INTEGER NOT NULL,
    trigger_type         TEXT NOT NULL,
    status               TEXT NOT NULL DEFAULT 'completed',
    trigger_provenance   TEXT,
    findings_json        TEXT,
    reviewed_head_sha    TEXT,
    starting_head_sha    TEXT,
    trusted_config_sha   TEXT,
    replay_config_json   BLOB,
    global_config_yaml   BLOB,
    repo_config_yaml     BLOB,
    user_findings_json   TEXT,
    selected_finding_ids TEXT,
    selection_source     TEXT,
    fix_summary          TEXT,
    repair_failure_fingerprint TEXT,
    repair_result        TEXT,
    resulting_head_sha   TEXT,
    evaluated_head_sha   TEXT,
    duration_ms          INTEGER NOT NULL,
    created_at           INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS round_evaluations (
    id                 TEXT PRIMARY KEY,
    run_id             TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    round_id           TEXT NOT NULL UNIQUE REFERENCES step_rounds(id) ON DELETE CASCADE,
    kind               TEXT NOT NULL CHECK (kind IN ('initial_review', 'rereview', 'validation', 'revalidation', 'documentation')),
    summary            TEXT NOT NULL DEFAULT '',
    tested_json        TEXT NOT NULL DEFAULT '[]',
    testing_summary    TEXT NOT NULL DEFAULT '',
    risk_level         TEXT NOT NULL DEFAULT '',
    risk_rationale     TEXT NOT NULL DEFAULT '',
    risk_scope         TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_round_evaluations_run_created_id
    ON round_evaluations (run_id, created_at, id);

CREATE TABLE IF NOT EXISTS round_findings (
    id                    TEXT PRIMARY KEY,
    run_id                TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    evaluation_id         TEXT NOT NULL REFERENCES round_evaluations(id) ON DELETE CASCADE,
    ordinal               INTEGER NOT NULL CHECK (ordinal >= 0),
    external_id           TEXT NOT NULL,
    severity              TEXT NOT NULL,
    file                  TEXT NOT NULL DEFAULT '',
    line                  INTEGER NOT NULL DEFAULT 0,
    description           TEXT NOT NULL,
    action                TEXT NOT NULL,
    source                TEXT NOT NULL DEFAULT 'agent',
    user_instructions     TEXT NOT NULL DEFAULT '',
    review_scope          TEXT NOT NULL DEFAULT '',
    requires_human_review INTEGER NOT NULL DEFAULT 0,
    UNIQUE (evaluation_id, ordinal),
    UNIQUE (evaluation_id, external_id)
);

CREATE INDEX IF NOT EXISTS idx_round_findings_evaluation_ordinal
    ON round_findings (evaluation_id, ordinal);

CREATE TABLE IF NOT EXISTS round_evaluation_artifacts (
    id             TEXT PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    evaluation_id  TEXT NOT NULL REFERENCES round_evaluations(id) ON DELETE CASCADE,
    ordinal        INTEGER NOT NULL CHECK (ordinal >= 0),
    kind           TEXT NOT NULL DEFAULT '',
    label          TEXT NOT NULL DEFAULT '',
    path           TEXT NOT NULL DEFAULT '',
    url            TEXT NOT NULL DEFAULT '',
    content        TEXT NOT NULL DEFAULT '',
    UNIQUE (evaluation_id, ordinal)
);

CREATE INDEX IF NOT EXISTS idx_round_evaluation_artifacts_evaluation_ordinal
    ON round_evaluation_artifacts (evaluation_id, ordinal);

CREATE TABLE IF NOT EXISTS round_decisions (
    id             TEXT PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    round_id       TEXT NOT NULL UNIQUE REFERENCES step_rounds(id) ON DELETE CASCADE,
    source         TEXT NOT NULL CHECK (source IN ('user', 'auto_fix', 'user_declined', 'user_skipped', 'user_aborted')),
    terminal_source TEXT CHECK (terminal_source IS NULL OR terminal_source IN ('user_skipped', 'user_aborted')),
    explicit_empty INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS round_decision_findings (
    decision_id       TEXT NOT NULL REFERENCES round_decisions(id) ON DELETE CASCADE,
    finding_id        TEXT NOT NULL REFERENCES round_findings(id) ON DELETE CASCADE,
    ordinal           INTEGER NOT NULL CHECK (ordinal >= 0),
    selection_ordinal INTEGER CHECK (selection_ordinal IS NULL OR selection_ordinal >= 0),
    state             TEXT NOT NULL CHECK (state IN ('selected', 'unselected')),
    user_instructions TEXT NOT NULL DEFAULT '',
    edited            INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (decision_id, finding_id),
    UNIQUE (decision_id, ordinal)
);

CREATE INDEX IF NOT EXISTS idx_round_decision_findings_finding
    ON round_decision_findings (finding_id);

CREATE TABLE IF NOT EXISTS round_repairs (
    id                    TEXT PRIMARY KEY,
    run_id                TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    round_id              TEXT NOT NULL UNIQUE REFERENCES step_rounds(id) ON DELETE CASCADE,
    fix_summary           TEXT,
    failure_fingerprint   TEXT,
    result                TEXT CHECK (result IS NULL OR result IN ('attempted', 'resolved', 'stopped_no_progress', 'stopped_repeated_failure', 'stopped_attempt_limit')),
    resulting_head_sha    TEXT,
    created_at            INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_round_repairs_run_created_id
    ON round_repairs (run_id, created_at, id);

CREATE TABLE IF NOT EXISTS command_definitions (
    run_id                TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    id                    TEXT NOT NULL,
    script                TEXT NOT NULL,
    platform              TEXT NOT NULL,
    runner_executable     TEXT NOT NULL,
    runner_args_json      TEXT NOT NULL,
    PRIMARY KEY (run_id, id)
);

CREATE TABLE IF NOT EXISTS command_attempts (
    id                    TEXT PRIMARY KEY,
    run_id                TEXT NOT NULL,
    command_id            TEXT NOT NULL,
    step_id               TEXT NOT NULL REFERENCES step_results(id) ON DELETE CASCADE,
    round_id              TEXT NOT NULL REFERENCES step_rounds(id) ON DELETE CASCADE,
    sequence              INTEGER NOT NULL,
    purpose               TEXT NOT NULL,
    observer              TEXT NOT NULL,
    trigger_type          TEXT NOT NULL,
    before_sha            TEXT NOT NULL,
    tested_sha            TEXT,
    command_source        TEXT NOT NULL,
    runner_schema_version INTEGER NOT NULL,
    runner_source         TEXT NOT NULL,
    runner_version        TEXT,
    input_state_id        TEXT,
    result_state_id       TEXT,
    started_at            INTEGER NOT NULL,
    completed_at          INTEGER,
    duration_ms           INTEGER,
    outcome               TEXT,
    exit_code             INTEGER,
    signal                TEXT,
	    retry_of_attempt_id   TEXT REFERENCES command_attempts(id),
	    retry_reason          TEXT,
	    output_artifact_id    TEXT REFERENCES artifacts(id),
	    accepted_as_proof     INTEGER NOT NULL DEFAULT 0 CHECK (accepted_as_proof IN (0, 1)),
	    proof_reason          TEXT,
	    FOREIGN KEY (run_id, command_id) REFERENCES command_definitions(run_id, id) ON DELETE CASCADE,
	    CHECK ((accepted_as_proof = 0 AND proof_reason IS NULL) OR (accepted_as_proof = 1 AND proof_reason IS NOT NULL)),
	    UNIQUE (round_id, sequence)
);

CREATE INDEX IF NOT EXISTS idx_command_attempts_run_started_id
    ON command_attempts (run_id, started_at, id);

CREATE TABLE IF NOT EXISTS artifacts (
    id                     TEXT PRIMARY KEY,
    run_id                 TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_id                TEXT REFERENCES step_results(id) ON DELETE CASCADE,
    round_id               TEXT REFERENCES step_rounds(id) ON DELETE CASCADE,
    invocation_id          TEXT REFERENCES agent_invocations(id) ON DELETE SET NULL,
    command_attempt_id     TEXT UNIQUE REFERENCES command_attempts(id) ON DELETE CASCADE,
    purpose                TEXT NOT NULL,
    label                  TEXT NOT NULL,
    description            TEXT,
    storage_root           TEXT NOT NULL CHECK (storage_root IN ('run', 'evidence')),
    relative_path          TEXT NOT NULL,
    kind                   TEXT NOT NULL,
    media_type             TEXT NOT NULL,
    encoding               TEXT NOT NULL,
    sha256                 TEXT NOT NULL,
    source_bytes           INTEGER NOT NULL CHECK (source_bytes >= 0),
    state                  TEXT NOT NULL,
    reason                 TEXT,
    publication_state      TEXT,
    publication_url        TEXT,
    publication_commit_sha TEXT,
    created_at             INTEGER NOT NULL,
    UNIQUE (storage_root, relative_path)
);

CREATE INDEX IF NOT EXISTS idx_artifacts_run_created_id
    ON artifacts (run_id, created_at, id);

-- Operations are a run-scoped indexed collection. Variant tables carry their
-- typed facts so Push can join this collection later without duplicating the
-- shared identity, timing, or reference edges.
CREATE TABLE IF NOT EXISTS operations (
    id                     TEXT PRIMARY KEY,
    run_id                 TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind                   TEXT NOT NULL CHECK (kind IN ('refresh', 'push')),
    step_id                TEXT NOT NULL REFERENCES step_results(id) ON DELETE CASCADE,
    round_id               TEXT NOT NULL REFERENCES step_rounds(id) ON DELETE CASCADE,
    started_at             INTEGER NOT NULL CHECK (started_at > 0),
    completed_at           INTEGER NOT NULL CHECK (completed_at >= started_at),
    duration_ms            INTEGER NOT NULL CHECK (duration_ms >= 0),
    diagnostic_artifact_id TEXT,
    UNIQUE (run_id, id),
    FOREIGN KEY (run_id, diagnostic_artifact_id) REFERENCES artifacts(run_id, id)
);

CREATE INDEX IF NOT EXISTS idx_operations_run_started_id
    ON operations (run_id, started_at, id);

CREATE TABLE IF NOT EXISTS refresh_operations (
    operation_id            TEXT PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
    strategy                TEXT NOT NULL CHECK (strategy IN ('rebase', 'merge')),
    source_ref              TEXT NOT NULL,
    destination_ref         TEXT NOT NULL,
    authoritative_base_ref  TEXT NOT NULL,
    authoritative_base_sha  TEXT,
    starting_head_sha       TEXT,
    decision                TEXT NOT NULL CHECK (decision IN ('skipped', 'fast-forwarded', 'rebased', 'merged', 'conflicted', 'repaired', 'refused', 'error')),
    resulting_head_sha      TEXT,
    conflict_state          TEXT NOT NULL CHECK (conflict_state IN ('none', 'detected', 'resolved')),
    repair_state            TEXT NOT NULL CHECK (repair_state IN ('not_needed', 'not_attempted', 'succeeded', 'failed'))
);

CREATE TABLE IF NOT EXISTS operation_command_attempts (
    operation_id TEXT NOT NULL,
    run_id       TEXT NOT NULL,
    attempt_id   TEXT NOT NULL,
    sequence     INTEGER NOT NULL CHECK (sequence > 0),
    PRIMARY KEY (operation_id, sequence),
    UNIQUE (operation_id, attempt_id),
    FOREIGN KEY (run_id, operation_id) REFERENCES operations(run_id, id) ON DELETE CASCADE,
    FOREIGN KEY (run_id, attempt_id) REFERENCES command_attempts(run_id, id) ON DELETE CASCADE
);

CREATE TRIGGER IF NOT EXISTS validate_refresh_operation_scope_insert
BEFORE INSERT ON operations
WHEN NEW.kind = 'refresh' AND NOT EXISTS (
    SELECT 1
    FROM step_rounds r
    JOIN step_results s ON s.id = r.step_result_id
    WHERE r.id = NEW.round_id AND s.id = NEW.step_id AND s.run_id = NEW.run_id AND s.step_name = 'refresh'
)
BEGIN
    SELECT RAISE(ABORT, 'refresh operation step and round must belong to the same refresh run');
END;

CREATE TRIGGER IF NOT EXISTS validate_refresh_operation_scope_update
BEFORE UPDATE OF run_id, kind, step_id, round_id ON operations
WHEN NEW.kind = 'refresh' AND NOT EXISTS (
    SELECT 1
    FROM step_rounds r
    JOIN step_results s ON s.id = r.step_result_id
    WHERE r.id = NEW.round_id AND s.id = NEW.step_id AND s.run_id = NEW.run_id AND s.step_name = 'refresh'
)
BEGIN
    SELECT RAISE(ABORT, 'refresh operation step and round must belong to the same refresh run');
END;

CREATE TRIGGER IF NOT EXISTS validate_refresh_operation_diagnostic_insert
BEFORE INSERT ON operations
WHEN NEW.diagnostic_artifact_id IS NOT NULL AND NOT EXISTS (
    SELECT 1
    FROM artifacts
    WHERE id = NEW.diagnostic_artifact_id AND run_id = NEW.run_id AND step_id = NEW.step_id AND round_id = NEW.round_id
)
BEGIN
    SELECT RAISE(ABORT, 'refresh operation diagnostic artifact must belong to the same step round');
END;

CREATE TRIGGER IF NOT EXISTS validate_refresh_operation_diagnostic_update
BEFORE UPDATE OF run_id, step_id, round_id, diagnostic_artifact_id ON operations
WHEN NEW.diagnostic_artifact_id IS NOT NULL AND NOT EXISTS (
    SELECT 1
    FROM artifacts
    WHERE id = NEW.diagnostic_artifact_id AND run_id = NEW.run_id AND step_id = NEW.step_id AND round_id = NEW.round_id
)
BEGIN
    SELECT RAISE(ABORT, 'refresh operation diagnostic artifact must belong to the same step round');
END;

CREATE TRIGGER IF NOT EXISTS validate_operation_command_attempt_insert
BEFORE INSERT ON operation_command_attempts
WHEN NOT EXISTS (
    SELECT 1
    FROM operations o
    JOIN command_attempts a ON a.id = NEW.attempt_id
    WHERE o.id = NEW.operation_id AND o.run_id = NEW.run_id AND a.run_id = NEW.run_id
      AND a.step_id = o.step_id AND a.round_id = o.round_id
)
BEGIN
    SELECT RAISE(ABORT, 'operation command attempt must belong to the same operation step round');
END;

CREATE TRIGGER IF NOT EXISTS validate_operation_command_attempt_update
BEFORE UPDATE OF operation_id, run_id, attempt_id ON operation_command_attempts
WHEN NOT EXISTS (
    SELECT 1
    FROM operations o
    JOIN command_attempts a ON a.id = NEW.attempt_id
    WHERE o.id = NEW.operation_id AND o.run_id = NEW.run_id AND a.run_id = NEW.run_id
      AND a.step_id = o.step_id AND a.round_id = o.round_id
)
BEGIN
    SELECT RAISE(ABORT, 'operation command attempt must belong to the same operation step round');
END;

CREATE TABLE IF NOT EXISTS agent_invocations (
    id                    TEXT PRIMARY KEY,
    run_id                TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name             TEXT NOT NULL,
    round                 INTEGER NOT NULL,
    round_id              TEXT REFERENCES step_rounds(id) ON DELETE SET NULL,
    purpose               TEXT NOT NULL,
    agent                 TEXT NOT NULL,
	usage_coverage        TEXT NOT NULL DEFAULT 'unknown',
    model                 TEXT,
    effort                TEXT,
    model_provider        TEXT,
	review_candidate_pool_json TEXT,
    session_mode          TEXT NOT NULL,
    session_key           TEXT,
    fallback_reason       TEXT,
    started_at            INTEGER NOT NULL,
    completed_at          INTEGER NOT NULL,
    duration_ms           INTEGER NOT NULL,
    subprocess_wait_ms    INTEGER,
    exit_status           TEXT NOT NULL,
    failure_category      TEXT,
    input_tokens          INTEGER,
    output_tokens         INTEGER,
    cache_read_tokens     INTEGER,
    cache_creation_tokens INTEGER,
    fresh_input_tokens    INTEGER,
    reasoning_tokens      INTEGER,
    delta_input_tokens    INTEGER,
    delta_output_tokens   INTEGER,
	delta_cache_read_tokens INTEGER,
	delta_cache_creation_tokens INTEGER,
	reported_cost_usd       REAL,
	model_roundtrips      INTEGER,
    tool_calls            INTEGER,
    tool_wait_calls       INTEGER,
    tool_test_lint_calls  INTEGER,
    tool_edit_calls       INTEGER,
    tool_read_calls       INTEGER,
    tool_git_calls        INTEGER,
    tool_other_calls      INTEGER,
    workload_files        INTEGER,
    workload_lines        INTEGER,
    finding_count         INTEGER
);

CREATE INDEX IF NOT EXISTS idx_agent_invocations_run_started_id
    ON agent_invocations (run_id, started_at, id);

CREATE TABLE IF NOT EXISTS run_narratives (
    run_id                  TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    source                  TEXT NOT NULL CHECK (source IN ('agent', 'fallback')),
    drafting_invocation_id  TEXT REFERENCES agent_invocations(id),
    drafted_at              INTEGER NOT NULL,
    base_sha                TEXT NOT NULL,
    head_sha                TEXT NOT NULL,
    title_mode              TEXT NOT NULL CHECK (title_mode IN ('agent', 'fallback', 'preserved')),
    title_text              TEXT NOT NULL,
    summary                 TEXT NOT NULL,
    what_changed            TEXT NOT NULL,
    CHECK ((source = 'agent' AND drafting_invocation_id IS NOT NULL) OR
           (source = 'fallback' AND drafting_invocation_id IS NULL)),
    CHECK ((source = 'agent' AND title_mode IN ('agent', 'preserved')) OR
           (source = 'fallback' AND title_mode IN ('fallback', 'preserved')))
);

CREATE TABLE IF NOT EXISTS run_agent_sessions (
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    agent      TEXT NOT NULL,
    session_id TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (run_id, role)
);

-- Long-lived content-free metric receipts intentionally have no foreign key.
-- Rich run rows and repository registrations may be deleted without cascading
-- away the historical facts that power stats.
CREATE TABLE IF NOT EXISTS run_metric_receipts (
    run_id                 TEXT PRIMARY KEY,
    repo_id                TEXT NOT NULL,
    run_created_at         INTEGER NOT NULL,
    run_status             TEXT NOT NULL,
    schema_version         INTEGER NOT NULL,
    payload_json           TEXT NOT NULL,
    receipt_sha256         TEXT NOT NULL,
    archived_at            INTEGER NOT NULL,
    pull_request           INTEGER NOT NULL DEFAULT 0,
    reported_findings      INTEGER NOT NULL DEFAULT 0,
    fixed_findings         INTEGER NOT NULL DEFAULT 0,
    step_stats_json        TEXT NOT NULL DEFAULT '[]',
    agent_aggregates_json  TEXT NOT NULL DEFAULT '[]',
	artifact_cleanup_pending INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_run_metric_receipts_repo_created
    ON run_metric_receipts (repo_id, run_created_at DESC, run_id DESC);

CREATE INDEX IF NOT EXISTS idx_run_metric_receipts_status_created
    ON run_metric_receipts (run_status, run_created_at DESC, run_id DESC);

CREATE TABLE IF NOT EXISTS schema_migrations (
    name TEXT PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS run_artifact_cleanup_journal (
    run_id TEXT PRIMARY KEY,
    targets_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS intent_cache (
    cache_key   TEXT PRIMARY KEY,
    summary     TEXT NOT NULL,
    agent_name  TEXT NOT NULL,
    session_id  TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);

-- Per-branch range of pipeline-authored commits whose re-review did not
-- complete. The next run's initial review reads this so it is not cold on
-- uncertified fixer commits. PRIMARY KEY per branch: the latest uncertified
-- HEAD replaces an older range.
CREATE TABLE IF NOT EXISTS uncertified_pipeline_ranges (
    repo_id       TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch        TEXT NOT NULL,
    from_sha      TEXT NOT NULL,
    to_sha        TEXT NOT NULL,
    source_run_id TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (repo_id, branch)
);
`

// freshSchemaObjectsSQL contains objects whose columns were introduced by
// compatibility migrations. Keep them out of schemaSQL so opening an older
// database can install the base schema before those migrations add the columns.
const freshSchemaObjectsSQL = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_command_attempts_retry_of
    ON command_attempts (retry_of_attempt_id) WHERE retry_of_attempt_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_command_attempts_output_artifact
    ON command_attempts (output_artifact_id) WHERE output_artifact_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_command_attempts_proof_by_tested_sha
    ON command_attempts (run_id, tested_sha) WHERE accepted_as_proof = 1;

CREATE TRIGGER IF NOT EXISTS validate_command_attempt_proof_state_insert
BEFORE INSERT ON command_attempts
WHEN NEW.accepted_as_proof NOT IN (0, 1)
  OR (NEW.accepted_as_proof = 0 AND NEW.proof_reason IS NOT NULL)
  OR (NEW.accepted_as_proof = 1 AND NEW.proof_reason IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'command attempt proof state must pair acceptance and reason');
END;

CREATE TRIGGER IF NOT EXISTS validate_command_attempt_proof_state_update
BEFORE UPDATE OF accepted_as_proof, proof_reason ON command_attempts
WHEN NEW.accepted_as_proof NOT IN (0, 1)
  OR (NEW.accepted_as_proof = 0 AND NEW.proof_reason IS NOT NULL)
  OR (NEW.accepted_as_proof = 1 AND NEW.proof_reason IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'command attempt proof state must pair acceptance and reason');
END;

CREATE UNIQUE INDEX IF NOT EXISTS idx_round_decision_findings_selection_ordinal
    ON round_decision_findings (decision_id, selection_ordinal)
    WHERE selection_ordinal IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_agent_invocations_round_started_id
    ON agent_invocations (round_id, started_at, id);
`

// migrationStatements hold additive schema changes applied to databases that
// were created before the referenced columns existed. Each statement must be
// idempotent via its error being tolerated when the column already exists.
var migrationStatements = []string{
	`CREATE TABLE IF NOT EXISTS run_narratives (
		run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
		source TEXT NOT NULL CHECK (source IN ('agent', 'fallback')),
		drafting_invocation_id TEXT REFERENCES agent_invocations(id),
		drafted_at INTEGER NOT NULL,
		base_sha TEXT NOT NULL,
		head_sha TEXT NOT NULL,
		title_mode TEXT NOT NULL CHECK (title_mode IN ('agent', 'fallback', 'preserved')),
		title_text TEXT NOT NULL,
		summary TEXT NOT NULL,
		what_changed TEXT NOT NULL,
		CHECK ((source = 'agent' AND drafting_invocation_id IS NOT NULL) OR
		       (source = 'fallback' AND drafting_invocation_id IS NULL))
	)`,
	`CREATE TABLE IF NOT EXISTS command_definitions (
		run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		id TEXT NOT NULL,
		script TEXT NOT NULL,
		platform TEXT NOT NULL,
		runner_executable TEXT NOT NULL,
		runner_args_json TEXT NOT NULL,
		PRIMARY KEY (run_id, id)
	)`,
	`CREATE TABLE IF NOT EXISTS command_attempts (
		id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL,
		command_id TEXT NOT NULL,
		step_id TEXT NOT NULL REFERENCES step_results(id) ON DELETE CASCADE,
		round_id TEXT NOT NULL REFERENCES step_rounds(id) ON DELETE CASCADE,
		sequence INTEGER NOT NULL,
		purpose TEXT NOT NULL,
		observer TEXT NOT NULL,
		trigger_type TEXT NOT NULL,
		before_sha TEXT NOT NULL,
		tested_sha TEXT,
		command_source TEXT NOT NULL,
		runner_schema_version INTEGER NOT NULL,
		runner_source TEXT NOT NULL,
		runner_version TEXT,
		input_state_id TEXT,
		result_state_id TEXT,
		started_at INTEGER NOT NULL,
		completed_at INTEGER,
		duration_ms INTEGER,
		outcome TEXT,
		exit_code INTEGER,
		signal TEXT,
			retry_of_attempt_id TEXT REFERENCES command_attempts(id),
			retry_reason TEXT,
			accepted_as_proof INTEGER NOT NULL DEFAULT 0 CHECK (accepted_as_proof IN (0, 1)),
			proof_reason TEXT,
			FOREIGN KEY (run_id, command_id) REFERENCES command_definitions(run_id, id) ON DELETE CASCADE,
			CHECK ((accepted_as_proof = 0 AND proof_reason IS NULL) OR (accepted_as_proof = 1 AND proof_reason IS NOT NULL)),
			UNIQUE (round_id, sequence)
		)`,
	`CREATE INDEX IF NOT EXISTS idx_command_attempts_run_started_id ON command_attempts (run_id, started_at, id)`,
	`CREATE TABLE IF NOT EXISTS artifacts (
		id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		step_id TEXT REFERENCES step_results(id) ON DELETE CASCADE,
		round_id TEXT REFERENCES step_rounds(id) ON DELETE CASCADE,
		invocation_id TEXT REFERENCES agent_invocations(id) ON DELETE SET NULL,
		command_attempt_id TEXT UNIQUE REFERENCES command_attempts(id) ON DELETE CASCADE,
		purpose TEXT NOT NULL,
		label TEXT NOT NULL,
		description TEXT,
		storage_root TEXT NOT NULL CHECK (storage_root IN ('run', 'evidence')),
		relative_path TEXT NOT NULL,
		kind TEXT NOT NULL,
		media_type TEXT NOT NULL,
		encoding TEXT NOT NULL,
		sha256 TEXT NOT NULL,
		source_bytes INTEGER NOT NULL CHECK (source_bytes >= 0),
		state TEXT NOT NULL,
		reason TEXT,
		publication_state TEXT,
		publication_url TEXT,
		publication_commit_sha TEXT,
		created_at INTEGER NOT NULL,
		UNIQUE (storage_root, relative_path)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_artifacts_run_created_id ON artifacts (run_id, created_at, id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_command_attempts_run_id ON command_attempts (run_id, id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_run_id ON artifacts (run_id, id)`,
	`CREATE TABLE IF NOT EXISTS operations (
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
	`CREATE INDEX IF NOT EXISTS idx_operations_run_started_id ON operations (run_id, started_at, id)`,
	`CREATE TABLE IF NOT EXISTS refresh_operations (
		operation_id TEXT PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
		strategy TEXT NOT NULL CHECK (strategy IN ('rebase', 'merge')),
		source_ref TEXT NOT NULL,
		destination_ref TEXT NOT NULL,
		authoritative_base_ref TEXT NOT NULL,
		authoritative_base_sha TEXT,
		starting_head_sha TEXT,
		decision TEXT NOT NULL CHECK (decision IN ('skipped', 'fast-forwarded', 'rebased', 'merged', 'conflicted', 'repaired', 'refused', 'error')),
		resulting_head_sha TEXT,
		conflict_state TEXT NOT NULL CHECK (conflict_state IN ('none', 'detected', 'resolved')),
		repair_state TEXT NOT NULL CHECK (repair_state IN ('not_needed', 'not_attempted', 'succeeded', 'failed'))
	)`,
	`CREATE TABLE IF NOT EXISTS operation_command_attempts (
		operation_id TEXT NOT NULL,
		run_id TEXT NOT NULL,
		attempt_id TEXT NOT NULL,
		sequence INTEGER NOT NULL CHECK (sequence > 0),
		PRIMARY KEY (operation_id, sequence),
		UNIQUE (operation_id, attempt_id),
		FOREIGN KEY (run_id, operation_id) REFERENCES operations(run_id, id) ON DELETE CASCADE,
		FOREIGN KEY (run_id, attempt_id) REFERENCES command_attempts(run_id, id) ON DELETE CASCADE
	)`,
	`CREATE TRIGGER IF NOT EXISTS validate_refresh_operation_scope_insert
	BEFORE INSERT ON operations
	WHEN NEW.kind = 'refresh' AND NOT EXISTS (
		SELECT 1
		FROM step_rounds r
		JOIN step_results s ON s.id = r.step_result_id
		WHERE r.id = NEW.round_id AND s.id = NEW.step_id AND s.run_id = NEW.run_id AND s.step_name = 'refresh'
	)
	BEGIN
		SELECT RAISE(ABORT, 'refresh operation step and round must belong to the same refresh run');
	END`,
	`CREATE TRIGGER IF NOT EXISTS validate_refresh_operation_scope_update
	BEFORE UPDATE OF run_id, kind, step_id, round_id ON operations
	WHEN NEW.kind = 'refresh' AND NOT EXISTS (
		SELECT 1
		FROM step_rounds r
		JOIN step_results s ON s.id = r.step_result_id
		WHERE r.id = NEW.round_id AND s.id = NEW.step_id AND s.run_id = NEW.run_id AND s.step_name = 'refresh'
	)
	BEGIN
		SELECT RAISE(ABORT, 'refresh operation step and round must belong to the same refresh run');
	END`,
	`CREATE TRIGGER IF NOT EXISTS validate_refresh_operation_diagnostic_insert
	BEFORE INSERT ON operations
	WHEN NEW.diagnostic_artifact_id IS NOT NULL AND NOT EXISTS (
		SELECT 1
		FROM artifacts
		WHERE id = NEW.diagnostic_artifact_id AND run_id = NEW.run_id AND step_id = NEW.step_id AND round_id = NEW.round_id
	)
	BEGIN
		SELECT RAISE(ABORT, 'refresh operation diagnostic artifact must belong to the same step round');
	END`,
	`CREATE TRIGGER IF NOT EXISTS validate_refresh_operation_diagnostic_update
	BEFORE UPDATE OF run_id, step_id, round_id, diagnostic_artifact_id ON operations
	WHEN NEW.diagnostic_artifact_id IS NOT NULL AND NOT EXISTS (
		SELECT 1
		FROM artifacts
		WHERE id = NEW.diagnostic_artifact_id AND run_id = NEW.run_id AND step_id = NEW.step_id AND round_id = NEW.round_id
	)
	BEGIN
		SELECT RAISE(ABORT, 'refresh operation diagnostic artifact must belong to the same step round');
	END`,
	`CREATE TRIGGER IF NOT EXISTS validate_operation_command_attempt_insert
	BEFORE INSERT ON operation_command_attempts
	WHEN NOT EXISTS (
		SELECT 1
		FROM operations o
		JOIN command_attempts a ON a.id = NEW.attempt_id
		WHERE o.id = NEW.operation_id AND o.run_id = NEW.run_id AND a.run_id = NEW.run_id
		  AND a.step_id = o.step_id AND a.round_id = o.round_id
	)
	BEGIN
		SELECT RAISE(ABORT, 'operation command attempt must belong to the same operation step round');
	END`,
	`CREATE TRIGGER IF NOT EXISTS validate_operation_command_attempt_update
	BEFORE UPDATE OF operation_id, run_id, attempt_id ON operation_command_attempts
	WHEN NOT EXISTS (
		SELECT 1
		FROM operations o
		JOIN command_attempts a ON a.id = NEW.attempt_id
		WHERE o.id = NEW.operation_id AND o.run_id = NEW.run_id AND a.run_id = NEW.run_id
		  AND a.step_id = o.step_id AND a.round_id = o.round_id
	)
	BEGIN
		SELECT RAISE(ABORT, 'operation command attempt must belong to the same operation step round');
	END`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_command_attempts_retry_of ON command_attempts (retry_of_attempt_id) WHERE retry_of_attempt_id IS NOT NULL`,
	`ALTER TABLE command_attempts ADD COLUMN output_artifact_id TEXT REFERENCES artifacts(id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_command_attempts_output_artifact ON command_attempts (output_artifact_id) WHERE output_artifact_id IS NOT NULL`,
	`ALTER TABLE command_attempts ADD COLUMN command_source TEXT NOT NULL DEFAULT 'legacy'`,
	`ALTER TABLE command_attempts ADD COLUMN runner_schema_version INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE command_attempts ADD COLUMN runner_source TEXT NOT NULL DEFAULT 'legacy'`,
	`ALTER TABLE command_attempts ADD COLUMN runner_version TEXT`,
	`ALTER TABLE command_attempts ADD COLUMN input_state_id TEXT`,
	`ALTER TABLE command_attempts ADD COLUMN result_state_id TEXT`,
	`ALTER TABLE command_attempts ADD COLUMN accepted_as_proof INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE command_attempts ADD COLUMN proof_reason TEXT`,
	`CREATE INDEX IF NOT EXISTS idx_command_attempts_proof_by_tested_sha ON command_attempts (run_id, tested_sha) WHERE accepted_as_proof = 1`,
	`CREATE TRIGGER IF NOT EXISTS validate_command_attempt_proof_state_insert
	BEFORE INSERT ON command_attempts
	WHEN NEW.accepted_as_proof NOT IN (0, 1)
	  OR (NEW.accepted_as_proof = 0 AND NEW.proof_reason IS NOT NULL)
	  OR (NEW.accepted_as_proof = 1 AND NEW.proof_reason IS NULL)
	BEGIN
		SELECT RAISE(ABORT, 'command attempt proof state must pair acceptance and reason');
	END`,
	`CREATE TRIGGER IF NOT EXISTS validate_command_attempt_proof_state_update
	BEFORE UPDATE OF accepted_as_proof, proof_reason ON command_attempts
	WHEN NEW.accepted_as_proof NOT IN (0, 1)
	  OR (NEW.accepted_as_proof = 0 AND NEW.proof_reason IS NOT NULL)
	  OR (NEW.accepted_as_proof = 1 AND NEW.proof_reason IS NULL)
	BEGIN
		SELECT RAISE(ABORT, 'command attempt proof state must pair acceptance and reason');
	END`,
	`ALTER TABLE run_metric_receipts ADD COLUMN artifact_cleanup_pending INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE IF NOT EXISTS run_artifact_cleanup_journal (run_id TEXT PRIMARY KEY, targets_json TEXT NOT NULL)`,
	`ALTER TABLE repos ADD COLUMN fork_url TEXT`,
	`ALTER TABLE runs ADD COLUMN refresh_strategy TEXT NOT NULL DEFAULT 'rebase'`,
	`ALTER TABLE runs ADD COLUMN stacked_on TEXT`,
	`ALTER TABLE runs ADD COLUMN config_sources_json TEXT NOT NULL DEFAULT '[]'`,
	// NULL marks a pre-migration run that retains legacy recovery behavior.
	// New runs explicitly insert an empty marker until launch-time resolution
	// persists the complete routing snapshot.
	`ALTER TABLE runs ADD COLUMN resolved_agent_routing_json TEXT`,
	`ALTER TABLE runs ADD COLUMN resolved_policy_json TEXT`,
	`ALTER TABLE runs ADD COLUMN resolved_policy_digest TEXT`,
	`ALTER TABLE runs ADD COLUMN pinned_at INTEGER`,
	`ALTER TABLE step_rounds ADD COLUMN selected_finding_ids TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN selection_source TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN fix_summary TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN user_findings_json TEXT`,
	// A parked round may retain the reviewed commit as a non-authoritative
	// candidate. Only atomic review completion promotes it onto the run.
	`ALTER TABLE step_rounds ADD COLUMN reviewed_head_sha TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN starting_head_sha TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN trusted_config_sha TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN replay_config_json BLOB`,
	`ALTER TABLE step_rounds ADD COLUMN global_config_yaml BLOB`,
	`ALTER TABLE step_rounds ADD COLUMN repo_config_yaml BLOB`,
	// Repair audit retains only a normalized hash and low-cardinality result;
	// prompts, output, diffs, paths, and tool arguments stay out of this table.
	`ALTER TABLE step_rounds ADD COLUMN repair_failure_fingerprint TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN repair_result TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN trigger_provenance TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN resulting_head_sha TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN evaluated_head_sha TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN status TEXT NOT NULL DEFAULT 'completed'`,
	`CREATE TABLE IF NOT EXISTS round_evaluations (
		id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		round_id TEXT NOT NULL UNIQUE REFERENCES step_rounds(id) ON DELETE CASCADE,
		kind TEXT NOT NULL CHECK (kind IN ('initial_review', 'rereview', 'validation', 'revalidation', 'documentation')),
		summary TEXT NOT NULL DEFAULT '',
		tested_json TEXT NOT NULL DEFAULT '[]',
		testing_summary TEXT NOT NULL DEFAULT '',
		risk_level TEXT NOT NULL DEFAULT '',
		risk_rationale TEXT NOT NULL DEFAULT '',
		risk_scope TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_round_evaluations_run_created_id ON round_evaluations (run_id, created_at, id)`,
	`CREATE TABLE IF NOT EXISTS round_findings (
		id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		evaluation_id TEXT NOT NULL REFERENCES round_evaluations(id) ON DELETE CASCADE,
		ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
		external_id TEXT NOT NULL,
		severity TEXT NOT NULL,
		file TEXT NOT NULL DEFAULT '',
		line INTEGER NOT NULL DEFAULT 0,
		description TEXT NOT NULL,
		action TEXT NOT NULL,
		source TEXT NOT NULL DEFAULT 'agent',
		user_instructions TEXT NOT NULL DEFAULT '',
		review_scope TEXT NOT NULL DEFAULT '',
		requires_human_review INTEGER NOT NULL DEFAULT 0,
		UNIQUE (evaluation_id, ordinal),
		UNIQUE (evaluation_id, external_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_round_findings_evaluation_ordinal ON round_findings (evaluation_id, ordinal)`,
	`CREATE TABLE IF NOT EXISTS round_evaluation_artifacts (
		id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		evaluation_id TEXT NOT NULL REFERENCES round_evaluations(id) ON DELETE CASCADE,
		ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
		kind TEXT NOT NULL DEFAULT '',
		label TEXT NOT NULL DEFAULT '',
		path TEXT NOT NULL DEFAULT '',
		url TEXT NOT NULL DEFAULT '',
		content TEXT NOT NULL DEFAULT '',
		UNIQUE (evaluation_id, ordinal)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_round_evaluation_artifacts_evaluation_ordinal ON round_evaluation_artifacts (evaluation_id, ordinal)`,
	`CREATE TABLE IF NOT EXISTS round_decisions (
		id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		round_id TEXT NOT NULL UNIQUE REFERENCES step_rounds(id) ON DELETE CASCADE,
		source TEXT NOT NULL CHECK (source IN ('user', 'auto_fix', 'user_declined', 'user_skipped', 'user_aborted')),
		terminal_source TEXT CHECK (terminal_source IS NULL OR terminal_source IN ('user_skipped', 'user_aborted')),
		explicit_empty INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS round_decision_findings (
		decision_id TEXT NOT NULL REFERENCES round_decisions(id) ON DELETE CASCADE,
		finding_id TEXT NOT NULL REFERENCES round_findings(id) ON DELETE CASCADE,
		ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
		selection_ordinal INTEGER CHECK (selection_ordinal IS NULL OR selection_ordinal >= 0),
		state TEXT NOT NULL CHECK (state IN ('selected', 'unselected')),
		user_instructions TEXT NOT NULL DEFAULT '',
		edited INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (decision_id, finding_id),
		UNIQUE (decision_id, ordinal)
	)`,
	`ALTER TABLE round_decision_findings ADD COLUMN selection_ordinal INTEGER`,
	`CREATE INDEX IF NOT EXISTS idx_round_decision_findings_finding ON round_decision_findings (finding_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_round_decision_findings_selection_ordinal ON round_decision_findings (decision_id, selection_ordinal) WHERE selection_ordinal IS NOT NULL`,
	`CREATE TABLE IF NOT EXISTS round_repairs (
		id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		round_id TEXT NOT NULL UNIQUE REFERENCES step_rounds(id) ON DELETE CASCADE,
		fix_summary TEXT,
		failure_fingerprint TEXT,
		result TEXT CHECK (result IS NULL OR result IN ('attempted', 'resolved', 'stopped_no_progress', 'stopped_repeated_failure', 'stopped_attempt_limit')),
		resulting_head_sha TEXT,
		created_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_round_repairs_run_created_id ON round_repairs (run_id, created_at, id)`,
	`ALTER TABLE agent_invocations ADD COLUMN round_id TEXT REFERENCES step_rounds(id) ON DELETE SET NULL`,
	`CREATE INDEX IF NOT EXISTS idx_agent_invocations_round_started_id ON agent_invocations (round_id, started_at, id)`,
	`ALTER TABLE runs ADD COLUMN intent TEXT`,
	`ALTER TABLE runs ADD COLUMN intent_source TEXT`,
	`ALTER TABLE runs ADD COLUMN intent_session_id TEXT`,
	`ALTER TABLE runs ADD COLUMN intent_score REAL`,
	`ALTER TABLE runs ADD COLUMN awaiting_agent_since INTEGER`,
	`ALTER TABLE runs ADD COLUMN parked_ms INTEGER`,
	// The CI step's per-check rerun budget. It is durable because a run
	// recovered after a daemon restart would otherwise get a fresh budget and
	// could issue reruns beyond the documented limit; the reservation is
	// written before the provider call, so a crash mid-request spends the
	// budget rather than silently granting a free retry.
	`ALTER TABLE runs ADD COLUMN ci_rerun_state TEXT`,
	// CI performs its automatic fixes inside one executor round, so round
	// selections cannot reconstruct the spent budget after a daemon restart.
	// NULL deliberately preserves historical runs as unknown (and therefore
	// exhausted); new runs stamp zero, then reserve before invoking the fix agent.
	`ALTER TABLE runs ADD COLUMN ci_fix_attempts INTEGER`,
	// Branch synchronization provenance is intentionally nullable. Historical
	// rows stay unbound because mutable head_sha cannot prove a successful push.
	`ALTER TABLE runs ADD COLUMN submitted_head_sha TEXT`,
	// Build identity is nullable for historical records. New runs record the
	// version and embedded build SHA used by the running binary.
	`ALTER TABLE runs ADD COLUMN no_mistakes_version TEXT`,
	`ALTER TABLE runs ADD COLUMN no_mistakes_build_sha TEXT`,
	// Review authority is nullable and never backfilled. A historical mutable
	// head_sha cannot prove which exact commit a completed review approved.
	`ALTER TABLE runs ADD COLUMN review_approved_head_sha TEXT`,
	`ALTER TABLE runs ADD COLUMN last_pushed_sha TEXT`,
	`ALTER TABLE runs ADD COLUMN push_target_kind TEXT`,
	`ALTER TABLE runs ADD COLUMN push_target_fingerprint TEXT`,
	`ALTER TABLE runs ADD COLUMN push_ref TEXT`,
	`ALTER TABLE runs ADD COLUMN last_pushed_at INTEGER`,
	`ALTER TABLE runs ADD COLUMN push_generation INTEGER`,
	`ALTER TABLE runs ADD COLUMN push_active INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE runs ADD COLUMN terminal_head_verified_at INTEGER`,
	`ALTER TABLE runs ADD COLUMN pr_state TEXT`,
	`ALTER TABLE runs ADD COLUMN pr_state_observed_at INTEGER`,
	`ALTER TABLE runs ADD COLUMN ci_ready_at INTEGER`,
	`ALTER TABLE runs ADD COLUMN ci_ready_no_ci INTEGER NOT NULL DEFAULT 0`,
	// Custody return is nullable: NULL means the pipeline still owns any
	// unpublished head this run produced; a timestamp means an explicit
	// guarded recovery ended that ownership (internal/branchsync).
	`ALTER TABLE runs ADD COLUMN custody_returned_at INTEGER`,
	`ALTER TABLE step_results ADD COLUMN last_activity_at INTEGER`,
	`ALTER TABLE step_results ADD COLUMN last_activity TEXT`,
	`ALTER TABLE step_results ADD COLUMN agent_pid INTEGER`,
	`ALTER TABLE step_results ADD COLUMN auto_fix_limit INTEGER`,
	`ALTER TABLE step_results ADD COLUMN skip_source TEXT`,
	// Session-fidelity telemetry columns (all nullable so pre-existing rows read
	// back as unknown, never a fabricated zero).
	`ALTER TABLE agent_invocations ADD COLUMN effort TEXT`,
	`ALTER TABLE agent_invocations ADD COLUMN model_provider TEXT`,
	`ALTER TABLE agent_invocations ADD COLUMN review_candidate_pool_json TEXT`,
	`ALTER TABLE agent_invocations ADD COLUMN fallback_reason TEXT`,
	`ALTER TABLE agent_invocations ADD COLUMN subprocess_wait_ms INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN fresh_input_tokens INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN reasoning_tokens INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN delta_input_tokens INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN delta_output_tokens INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN delta_cache_read_tokens INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN delta_cache_creation_tokens INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN reported_cost_usd REAL`,
	`ALTER TABLE agent_invocations ADD COLUMN model_roundtrips INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_wait_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_test_lint_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_edit_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_read_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_git_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_other_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN workload_files INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN workload_lines INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN finding_count INTEGER`,
	// Historical rows predate adapter-authored coverage and therefore remain
	// unknown; migration never infers completeness from their token values.
	`ALTER TABLE agent_invocations ADD COLUMN usage_coverage TEXT NOT NULL DEFAULT 'unknown'`,
	`ALTER TABLE runs ADD COLUMN pr_note TEXT`,
	`ALTER TABLE runs ADD COLUMN metadata TEXT`,
	`ALTER TABLE step_results ADD COLUMN evidence_json TEXT`,
	`ALTER TABLE step_results ADD COLUMN planned_command TEXT`,
}

// removalMigrationStatements delete telemetry fields that asserted nested
// attribution the adapters cannot prove. Missing-column errors are expected on
// fresh databases and subsequent opens.
var removalMigrationStatements = []string{
	`ALTER TABLE agent_invocations DROP COLUMN invocation_mode`,
	`ALTER TABLE agent_invocations DROP COLUMN agent_observations_json`,
	`ALTER TABLE agent_invocations DROP COLUMN nested_agent_count`,
	`ALTER TABLE agent_invocations DROP COLUMN pricing_receipt_json`,
}
