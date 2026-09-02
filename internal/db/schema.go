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
    ci_fix_attempts         INTEGER,
    last_pushed_sha         TEXT,
    push_target_kind        TEXT,
    push_target_fingerprint TEXT,
    push_ref                TEXT,
    last_pushed_at          INTEGER,
    push_generation         INTEGER,
    push_active             INTEGER NOT NULL DEFAULT 0,
    terminal_head_verified_at INTEGER,
    error                   TEXT,
    awaiting_agent_since INTEGER,
    parked_ms            INTEGER,
	metadata             TEXT,
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
    duration_ms          INTEGER NOT NULL,
    created_at           INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_invocations (
    id                    TEXT PRIMARY KEY,
    run_id                TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name             TEXT NOT NULL,
    round                 INTEGER NOT NULL,
    purpose               TEXT NOT NULL,
    agent                 TEXT NOT NULL,
	usage_coverage        TEXT NOT NULL DEFAULT 'unknown',
    model                 TEXT,
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
