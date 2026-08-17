package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	ConfigSourceGlobal  = "global"
	ConfigSourceBranch  = "branch"
	ConfigSourceDefault = "default"
	// ConfigSourceGlobalOverride is a matched machine-local overrides entry
	// from the global config file; its digest is the global config file's
	// digest and its ref is the matched <owner>/<repo> key.
	ConfigSourceGlobalOverride = "global-override"
	// ConfigSourceMachine is the retired machine-local repo-config file
	// mechanism. It is kept only so recovery can refuse runs launched by
	// older binaries instead of silently dropping their launch-time overlay.
	ConfigSourceMachine = "machine-local"
)

// ConfigSource binds one effective run-config input to the exact bytes read at
// launch. Path (and, for global overrides, the matched key in Ref) is private
// recovery metadata and must not be rendered into public PR text.
type ConfigSource struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	Ref    string `json:"ref,omitempty"`
	Path   string `json:"path,omitempty"`
}

// Run represents a pipeline run.
type Run struct {
	ID              string
	RepoID          string
	Branch          string
	HeadSHA         string
	BaseSHA         string
	RefreshStrategy types.RefreshStrategy
	StackedOn       string
	ConfigSources   []ConfigSource
	// ResolvedAgentRouting is a private run-scoped snapshot used only to restore
	// launch-time concrete agent/model routes after daemon restart. NULL marks a
	// pre-migration run; an empty string marks a new run whose launch did not
	// finish persisting routing and must fail closed during recovery.
	ResolvedAgentRouting *string
	// ResolvedPolicy and ResolvedPolicyDigest are the private canonical launch
	// contract and its SHA-256 digest. NULL marks a legacy run; explicit empty
	// markers make an interrupted new-run setup fail closed during recovery.
	ResolvedPolicy       *string
	ResolvedPolicyDigest *string
	SubmittedHeadSHA     *string
	// NoMistakesVersion and NoMistakesBuildSHA identify the binary that created
	// this run. They remain nil only for runs recorded before these fields.
	NoMistakesVersion  *string
	NoMistakesBuildSHA *string
	// ReviewApprovedHeadSHA is the exact commit approved by the last
	// successfully completed full review. It is nil for legacy runs and until
	// review completes; mutable run/worktree heads never infer this authority.
	ReviewApprovedHeadSHA  *string
	Status                 types.RunStatus
	PinnedAt               *int64
	PRURL                  *string
	PRState                *string
	PRStateObservedAt      *int64
	CIReadyAt              *int64
	CIReadyNoCI            bool
	LastPushedSHA          *string
	PushTargetKind         *string
	PushTargetFingerprint  *string
	PushRef                *string
	LastPushedAt           *int64
	PushGeneration         *int64
	PushActive             bool
	TerminalHeadVerifiedAt *int64
	// CustodyReturnedAt is non-nil once a guarded branch-sync recovery
	// explicitly ended this run's ownership of an unpublished pipeline head
	// (terminal run whose head was never successfully pushed, or moved after
	// the last push). It never changes push provenance; it only records that
	// the operator worktree took the branch back.
	CustodyReturnedAt *int64
	Error             *string
	// AwaitingAgentSince is the unix-seconds timestamp at which the run parked
	// awaiting the driving agent. Most parks are awaiting_approval or fix_review
	// steps; a launch-time environmental failure can park before step records
	// exist. It is nil whenever the run is not parked.
	AwaitingAgentSince *int64
	// ParkedMS accumulates the run's total parked wall time in milliseconds
	// (local performance telemetry; step duration_ms values exclude this time).
	ParkedMS        int64
	Intent          *string
	IntentSource    *string
	IntentSessionID *string
	IntentScore     *float64
	// PRNote is optional author-supplied content set per run via
	// `axi run --pr-note` or `--pr-note-file`.
	PRNote *string
	// Metadata is opaque operator-supplied text. A non-nil empty string records
	// an explicit clear on rerun; no parser assigns structure to it.
	Metadata  *string
	CreatedAt int64
	UpdatedAt int64
}

const runColumns = `id, repo_id, branch, head_sha, base_sha, COALESCE(refresh_strategy, 'rebase'), COALESCE(stacked_on, ''), COALESCE(config_sources_json, '[]'), resolved_agent_routing_json, resolved_policy_json, resolved_policy_digest, submitted_head_sha, no_mistakes_version, no_mistakes_build_sha, review_approved_head_sha, status, pinned_at, pr_url, pr_state, pr_state_observed_at, ci_ready_at, COALESCE(ci_ready_no_ci, 0), last_pushed_sha, push_target_kind, push_target_fingerprint, push_ref, last_pushed_at, push_generation, COALESCE(push_active, 0), terminal_head_verified_at, custody_returned_at, error, awaiting_agent_since, COALESCE(parked_ms, 0), intent, intent_source, intent_session_id, intent_score, pr_note, metadata, created_at, updated_at`

func scanRun(row interface {
	Scan(...any) error
}, r *Run) error {
	var configSourcesJSON string
	if err := row.Scan(
		&r.ID, &r.RepoID, &r.Branch, &r.HeadSHA, &r.BaseSHA, &r.RefreshStrategy, &r.StackedOn, &configSourcesJSON, &r.ResolvedAgentRouting, &r.ResolvedPolicy, &r.ResolvedPolicyDigest, &r.SubmittedHeadSHA, &r.NoMistakesVersion, &r.NoMistakesBuildSHA, &r.ReviewApprovedHeadSHA, &r.Status, &r.PinnedAt,
		&r.PRURL, &r.PRState, &r.PRStateObservedAt, &r.CIReadyAt, &r.CIReadyNoCI,
		&r.LastPushedSHA, &r.PushTargetKind, &r.PushTargetFingerprint, &r.PushRef,
		&r.LastPushedAt, &r.PushGeneration, &r.PushActive, &r.TerminalHeadVerifiedAt,
		&r.CustodyReturnedAt, &r.Error, &r.AwaitingAgentSince, &r.ParkedMS,
		&r.Intent, &r.IntentSource, &r.IntentSessionID, &r.IntentScore,
		&r.PRNote, &r.Metadata,
		&r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(configSourcesJSON), &r.ConfigSources); err != nil {
		return fmt.Errorf("decode config sources: %w", err)
	}
	return nil
}

// InsertRun creates a new run record.
func (d *DB) InsertRun(repoID, branch, headSHA, baseSHA string) (*Run, error) {
	return d.InsertRunWithOptions(repoID, branch, headSHA, baseSHA, RunOptions{})
}

// InsertRunWithPRNote atomically creates a run with its optional PR note.
func (d *DB) InsertRunWithPRNote(repoID, branch, headSHA, baseSHA, prNote string) (*Run, error) {
	return d.InsertRunWithOptions(repoID, branch, headSHA, baseSHA, RunOptions{PRNote: prNote})
}

// RunOptions are immutable selections stamped onto a run at creation.
type RunOptions struct {
	PRNote          string
	Metadata        *string
	RefreshStrategy types.RefreshStrategy
	StackedOn       string
	Intent          *RunIntent
	// LegacyResolvedPolicy is reserved for migration fixtures that represent a
	// row created before the resolved-policy columns existed. New runs leave it
	// false and receive explicit fail-closed launch markers atomically.
	LegacyResolvedPolicy bool
}

func (d *DB) InsertRunWithIntent(repoID, branch, headSHA, baseSHA string, intent *RunIntent) (*Run, error) {
	return d.InsertRunWithOptions(repoID, branch, headSHA, baseSHA, RunOptions{Intent: intent})
}

// InsertRunWithOptions atomically creates a run with its refresh selection and
// optional PR note.
func (d *DB) InsertRunWithOptions(repoID, branch, headSHA, baseSHA string, opts RunOptions) (*Run, error) {
	return d.insertRunWithOptions(repoID, branch, headSHA, baseSHA, opts, types.RunPending, nil)
}

// InsertFailedRunWithOptions atomically records a pre-execution failure. It is
// reserved for launch validation that has enough identity to audit the push
// but must not create a worktree or start the pipeline.
func (d *DB) InsertFailedRunWithOptions(repoID, branch, headSHA, baseSHA string, opts RunOptions, failure string) (*Run, error) {
	if strings.TrimSpace(failure) == "" {
		return nil, fmt.Errorf("failed run error is empty")
	}
	return d.insertRunWithOptions(repoID, branch, headSHA, baseSHA, opts, types.RunFailed, &failure)
}

func (d *DB) insertRunWithOptions(repoID, branch, headSHA, baseSHA string, opts RunOptions, status types.RunStatus, runError *string) (*Run, error) {
	ts := now()
	var terminalHeadVerifiedAt *int64
	if runError != nil && status == types.RunFailed {
		terminalHeadVerifiedAt = &ts
	}
	strategy := opts.RefreshStrategy.OrDefault()
	stackedOn := strings.TrimSpace(opts.StackedOn)
	routingMarker := ""
	emptyPolicy := ""
	emptyDigest := ""
	policyMarker := &emptyPolicy
	policyDigestMarker := &emptyDigest
	if opts.LegacyResolvedPolicy {
		policyMarker = nil
		policyDigestMarker = nil
	}
	version := buildinfo.CurrentVersion()
	buildSHA := buildinfo.Commit
	r := &Run{
		ID:                     newID(),
		RepoID:                 repoID,
		Branch:                 branch,
		HeadSHA:                headSHA,
		BaseSHA:                baseSHA,
		RefreshStrategy:        strategy,
		StackedOn:              stackedOn,
		ResolvedAgentRouting:   &routingMarker,
		ResolvedPolicy:         policyMarker,
		ResolvedPolicyDigest:   policyDigestMarker,
		SubmittedHeadSHA:       &headSHA,
		NoMistakesVersion:      &version,
		NoMistakesBuildSHA:     &buildSHA,
		Status:                 status,
		Error:                  runError,
		TerminalHeadVerifiedAt: terminalHeadVerifiedAt,
		CreatedAt:              ts,
		UpdatedAt:              ts,
	}
	if opts.Intent != nil {
		r.Intent = &opts.Intent.Summary
		r.IntentSource = &opts.Intent.Source
		r.IntentSessionID = &opts.Intent.SessionID
		r.IntentScore = &opts.Intent.Score
	}
	var notePtr *string
	if opts.PRNote != "" {
		note := opts.PRNote
		r.PRNote = &note
		notePtr = &note
	}
	r.Metadata = opts.Metadata
	_, err := d.sql.Exec(
		`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, refresh_strategy, stacked_on, resolved_agent_routing_json, resolved_policy_json, resolved_policy_digest, submitted_head_sha, no_mistakes_version, no_mistakes_build_sha, status, pr_state, parked_ms, intent, intent_source, intent_session_id, intent_score, created_at, updated_at, pr_note, metadata, error, terminal_head_verified_at) VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, 'none', 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.RepoID, r.Branch, r.HeadSHA, r.BaseSHA, r.RefreshStrategy, nullableString(stackedOn), policyMarker, policyDigestMarker, headSHA, r.NoMistakesVersion, r.NoMistakesBuildSHA, r.Status, r.Intent, r.IntentSource, r.IntentSessionID, r.IntentScore, r.CreatedAt, r.UpdatedAt, notePtr, opts.Metadata, runError, terminalHeadVerifiedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert run: %w", err)
	}
	return r, nil
}

// GetRunParkedMS returns the nullable stored parked duration without applying
// the legacy GetRun projection's COALESCE. New runs store a concrete zero;
// NULL therefore remains an honest pre-instrumentation unknown.
func (d *DB) GetRunParkedMS(id string) (*int64, error) {
	var parkedMS *int64
	err := d.sql.QueryRow(`SELECT parked_ms FROM runs WHERE id = ?`, id).Scan(&parkedMS)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run parked duration: %w", err)
	}
	return parkedMS, nil
}

// UpdateRunResolvedAgentRouting stores the resolved launch-time routing
// identity before any hook or pipeline step executes. The snapshot is private
// recovery state and is intentionally separate from public config provenance.
func (d *DB) UpdateRunResolvedAgentRouting(id, snapshot string) error {
	if strings.TrimSpace(snapshot) == "" {
		return fmt.Errorf("resolved agent routing snapshot is empty")
	}
	if _, err := d.sql.Exec(`UPDATE runs SET resolved_agent_routing_json = ?, updated_at = ? WHERE id = ?`, snapshot, now(), id); err != nil {
		return fmt.Errorf("update run resolved agent routing: %w", err)
	}
	return nil
}

// UpdateRunResolvedPolicy stores the canonical launch contract and digest
// before any hook or pipeline step executes.
func (d *DB) UpdateRunResolvedPolicy(id, snapshot, digest string) error {
	if strings.TrimSpace(snapshot) == "" {
		return fmt.Errorf("resolved policy snapshot is empty")
	}
	if strings.TrimSpace(digest) == "" {
		return fmt.Errorf("resolved policy digest is empty")
	}
	if _, err := d.sql.Exec(`UPDATE runs SET resolved_policy_json = ?, resolved_policy_digest = ?, updated_at = ? WHERE id = ?`, snapshot, digest, now(), id); err != nil {
		return fmt.Errorf("update run resolved policy: %w", err)
	}
	return nil
}

// UpdateRunRefreshSelection persists the strategy resolved from trusted config
// before pipeline execution starts.
func (d *DB) UpdateRunRefreshSelection(id string, strategy types.RefreshStrategy, stackedOn string) error {
	strategy = strategy.OrDefault()
	stackedOn = strings.TrimSpace(stackedOn)
	_, err := d.sql.Exec(`UPDATE runs SET refresh_strategy = ?, stacked_on = ?, updated_at = ? WHERE id = ?`, strategy, nullableString(stackedOn), now(), id)
	if err != nil {
		return fmt.Errorf("update run refresh selection: %w", err)
	}
	return nil
}

// UpdateRunConfigSources records the ordered config inputs that contributed to
// the effective run. It is written before any hook or pipeline step executes.
func (d *DB) UpdateRunConfigSources(id string, sources []ConfigSource) error {
	if sources == nil {
		sources = []ConfigSource{}
	}
	encoded, err := json.Marshal(sources)
	if err != nil {
		return fmt.Errorf("encode config sources: %w", err)
	}
	if _, err := d.sql.Exec(`UPDATE runs SET config_sources_json = ?, updated_at = ? WHERE id = ?`, string(encoded), now(), id); err != nil {
		return fmt.Errorf("update run config sources: %w", err)
	}
	return nil
}

// GetRun returns a run by ID.
func (d *DB) GetRun(id string) (*Run, error) {
	r := &Run{}
	err := scanRun(d.sql.QueryRow(`SELECT `+runColumns+` FROM runs WHERE id = ?`, id), r)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	return r, nil
}

// SetRunPinned explicitly includes or excludes a run from rich-data pruning.
// Pinning is idempotent and preserves the original pin timestamp.
func (d *DB) SetRunPinned(id string, pinned bool) (*Run, error) {
	ts := now()
	result, err := d.sql.Exec(
		`UPDATE runs SET pinned_at = CASE WHEN ? THEN COALESCE(pinned_at, ?) ELSE NULL END, updated_at = ? WHERE id = ?`,
		pinned, ts, ts, id,
	)
	if err != nil {
		return nil, fmt.Errorf("set run pin: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read run pin result: %w", err)
	}
	if changed == 0 {
		return nil, fmt.Errorf("run %q not found", id)
	}
	run, err := d.GetRun(id)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("run %q not found", id)
	}
	return run, nil
}

// GetRunsByRepo returns all runs for a repo, newest first.
func (d *DB) GetRunsByRepo(repoID string) ([]*Run, error) {
	rows, err := d.sql.Query(`SELECT `+runColumns+` FROM runs WHERE repo_id = ? ORDER BY created_at DESC, id DESC`, repoID)
	if err != nil {
		return nil, fmt.Errorf("get runs by repo: %w", err)
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		r := &Run{}
		if err := scanRun(rows, r); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// GetRunsByRepoHead returns the runs for a repo matching an exact branch and
// head SHA, newest first. It lets a caller detect the run created by a specific
// push without scanning (and rebuilding step data for) the repo's entire run
// history, so the cost stays bounded to the handful of runs for one head.
func (d *DB) GetRunsByRepoHead(repoID, branch, headSHA string) ([]*Run, error) {
	rows, err := d.sql.Query(
		`SELECT `+runColumns+` FROM runs WHERE repo_id = ? AND branch = ? AND head_sha = ? ORDER BY created_at DESC, id DESC`,
		repoID, branch, headSHA,
	)
	if err != nil {
		return nil, fmt.Errorf("get runs by repo head: %w", err)
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		r := &Run{}
		if err := scanRun(rows, r); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// GetActiveRun returns the currently active run (pending or running) for a repo,
// if any. When branch is non-empty, only a run on that exact branch is returned
// - the setup wizard relies on this to decide whether a new run is needed for
// the current branch. When branch is empty, returns the most recently created
// active run across any branch.
func (d *DB) GetActiveRun(repoID, branch string) (*Run, error) {
	r := &Run{}
	var err error
	if branch == "" {
		err = scanRun(d.sql.QueryRow(
			`SELECT `+runColumns+` FROM runs WHERE repo_id = ? AND status IN ('pending', 'running') ORDER BY created_at DESC, id DESC LIMIT 1`, repoID,
		), r)
	} else {
		err = scanRun(d.sql.QueryRow(
			`SELECT `+runColumns+` FROM runs WHERE repo_id = ? AND branch = ? AND status IN ('pending', 'running') ORDER BY created_at DESC, id DESC LIMIT 1`, repoID, branch,
		), r)
	}
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get active run: %w", err)
	}
	return r, nil
}

// GetActiveRuns returns all pending or running runs across all repos, newest first.
func (d *DB) GetActiveRuns() ([]*Run, error) {
	rows, err := d.sql.Query(
		`SELECT `+runColumns+` FROM runs WHERE status IN (?, ?) ORDER BY created_at DESC, id DESC`,
		types.RunPending, types.RunRunning,
	)
	if err != nil {
		return nil, fmt.Errorf("get active runs: %w", err)
	}
	defer rows.Close()

	var runs []*Run
	for rows.Next() {
		r := &Run{}
		if err := scanRun(rows, r); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// UpdateRunStatus updates a run's status and updated_at timestamp.
func (d *DB) UpdateRunStatus(id string, status types.RunStatus) error {
	_, err := d.sql.Exec(`UPDATE runs SET status = ?, push_active = CASE WHEN ? IN ('completed', 'failed', 'cancelled') THEN 0 ELSE push_active END, terminal_head_verified_at = NULL, updated_at = ? WHERE id = ?`, status, status, now(), id)
	if err != nil {
		return fmt.Errorf("update run status: %w", err)
	}
	return nil
}

// UpdateRunPRURL sets the PR URL on a run. A delayed PR-step write must not
// regress terminal lifecycle truth already observed by the CI monitor.
func (d *DB) UpdateRunPRURL(id, prURL string) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET pr_url = ?, pr_state = CASE WHEN pr_state IN ('merged', 'closed') THEN pr_state ELSE 'open' END, pr_state_observed_at = ?, updated_at = ? WHERE id = ?`, prURL, ts, ts, id)
	if err != nil {
		return fmt.Errorf("update run pr url: %w", err)
	}
	return nil
}

// PushBinding records the exact target and commit proven by a successful
// pipeline-owned push. TargetFingerprint is a one-way digest and must never be
// a raw URL.
type PushBinding struct {
	HeadSHA           string
	TargetKind        string
	TargetFingerprint string
	Ref               string
}

// UpdateRunPushBinding advances a run's successful-push provenance and
// increments its generation. It is called for both a completed push and a
// freshly verified already-up-to-date push.
func (d *DB) UpdateRunPushBinding(id string, binding PushBinding) error {
	ts := now()
	_, err := d.sql.Exec(
		`UPDATE runs SET last_pushed_sha = ?, push_target_kind = ?, push_target_fingerprint = ?, push_ref = ?, last_pushed_at = ?, push_generation = COALESCE(push_generation, 0) + 1, updated_at = ? WHERE id = ?`,
		binding.HeadSHA, binding.TargetKind, binding.TargetFingerprint, binding.Ref, ts, ts, id,
	)
	if err != nil {
		return fmt.Errorf("update run push binding: %w", err)
	}
	return nil
}

// SetRunCustodyReturned stamps the moment a guarded recovery explicitly
// returned custody of this run's branch to the operator worktree. Stamping is
// idempotent: the first timestamp wins so the record keeps the original
// recovery moment.
func (d *DB) SetRunCustodyReturned(id string) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET custody_returned_at = COALESCE(custody_returned_at, ?), updated_at = ? WHERE id = ?`, ts, ts, id)
	if err != nil {
		return fmt.Errorf("set run custody returned: %w", err)
	}
	return nil
}

// SetRunPushActive marks whether a pipeline phase currently owns a possible
// branch-head update. Sync refuses while this marker is set.
func (d *DB) SetRunPushActive(id string, active bool) error {
	_, err := d.sql.Exec(`UPDATE runs SET push_active = ?, updated_at = ? WHERE id = ?`, active, now(), id)
	if err != nil {
		return fmt.Errorf("set run push active: %w", err)
	}
	return nil
}

// UpdateRunPRState persists normalized lifecycle truth independently of logs.
// A merged or closed PR is also the terminal outcome of the final CI monitor
// step, so the PR observation and active-run finalization are committed in one
// transaction. This makes the database authoritative even if execution stops
// before the executor's ordinary follow-up completion write.
func (d *DB) UpdateRunPRState(id, state string) error {
	state = strings.ToLower(strings.TrimSpace(state))
	ts := now()
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("update run PR state: begin transaction: %w", err)
	}
	defer tx.Rollback()

	var current sql.NullString
	if err := tx.QueryRow(`SELECT pr_state FROM runs WHERE id = ?`, id).Scan(&current); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("update run PR state: read current state: %w", err)
	}
	state = monotonicPRState(current.String, state)
	if _, err := tx.Exec(`UPDATE runs SET pr_state = ?, pr_state_observed_at = ?, updated_at = ? WHERE id = ?`, state, ts, ts, id); err != nil {
		return fmt.Errorf("update run PR state: %w", err)
	}
	if terminalPRState(state) {
		if err := finalizeTerminalPRRun(tx, id, ts); err != nil {
			return fmt.Errorf("update run PR state: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update run PR state: commit: %w", err)
	}
	return nil
}

// ReconcileTerminalPRRuns repairs active rows written by an older or
// interrupted daemon after terminal PR truth became durable but before the
// separate run completion write. It is called during exclusive daemon startup
// before parked-run planning and generic crash recovery.
func (d *DB) ReconcileTerminalPRRuns() (int, error) {
	ts := now()
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("reconcile terminal PR runs: begin transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id FROM runs WHERE status IN (?, ?) AND pr_state IN ('merged', 'closed')`, types.RunPending, types.RunRunning)
	if err != nil {
		return 0, fmt.Errorf("reconcile terminal PR runs: list runs: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("reconcile terminal PR runs: scan run: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("reconcile terminal PR runs: close rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reconcile terminal PR runs: list runs: %w", err)
	}

	for _, id := range ids {
		if err := finalizeTerminalPRRun(tx, id, ts); err != nil {
			return 0, fmt.Errorf("reconcile terminal PR runs: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("reconcile terminal PR runs: commit: %w", err)
	}
	return len(ids), nil
}

func monotonicPRState(current, observed string) string {
	current = strings.ToLower(strings.TrimSpace(current))
	observed = strings.ToLower(strings.TrimSpace(observed))
	switch {
	case current == "merged":
		return current
	case observed == "merged":
		return observed
	case current == "closed":
		return current
	default:
		return observed
	}
}

func terminalPRState(state string) bool {
	return state == "merged" || state == "closed"
}

func finalizeTerminalPRRun(tx *sql.Tx, id string, ts int64) error {
	if _, err := tx.Exec(
		`UPDATE step_results SET status = ?, exit_code = COALESCE(exit_code, 0), completed_at = COALESCE(completed_at, ?),
			last_activity_at = ?, last_activity = ?, agent_pid = NULL
		 WHERE run_id = ? AND step_name = ? AND status IN (?, ?, ?, ?)
		   AND EXISTS (SELECT 1 FROM runs WHERE id = ? AND status IN (?, ?))`,
		types.StepStatusCompleted, ts, ts, "status: completed", id, types.StepCI,
		types.StepStatusRunning, types.StepStatusAwaitingApproval, types.StepStatusFixing, types.StepStatusFixReview,
		id, types.RunPending, types.RunRunning,
	); err != nil {
		return fmt.Errorf("complete terminal CI step: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE runs SET
			status = CASE WHEN status IN (?, ?) THEN ? ELSE status END,
			push_active = 0,
			parked_ms = COALESCE(parked_ms, 0) + CASE
				WHEN awaiting_agent_since IS NOT NULL AND ? > awaiting_agent_since
				THEN (? - awaiting_agent_since) * 1000 ELSE 0 END,
			awaiting_agent_since = NULL, updated_at = ?
		 WHERE id = ?`,
		types.RunPending, types.RunRunning, types.RunCompleted, ts, ts, ts, id,
	); err != nil {
		return fmt.Errorf("finalize terminal PR run: %w", err)
	}
	return nil
}

// SetRunCIReady persists checks-passed readiness so fresh TUI and AXI attaches
// do not depend on receiving a historical log line.
func (d *DB) SetRunCIReady(id string, ready bool) error {
	return d.SetRunCIReadyWithReason(id, ready, false)
}

func (d *DB) SetRunCIReadyWithReason(id string, ready, declaredNoCI bool) error {
	readyValue := 0
	declaredValue := 0
	var readyAt any
	if ready {
		readyValue = 1
		readyAt = now()
		if declaredNoCI {
			declaredValue = 1
		}
	}
	_, err := d.sql.Exec(`UPDATE runs SET ci_ready_at = ?, ci_ready_no_ci = ?, updated_at = ? WHERE id = ? AND ((ci_ready_at IS NULL AND ? = 1) OR (ci_ready_at IS NOT NULL AND ? = 0) OR (COALESCE(ci_ready_no_ci, 0) != ?))`, readyAt, declaredValue, now(), id, readyValue, readyValue, declaredValue)
	if err != nil {
		return fmt.Errorf("set run CI ready: %w", err)
	}
	return nil
}

// UpdateRunReviewApprovedHeadSHA replaces the run's review authority with the
// exact commit approved by the latest successfully completed full review.
func (d *DB) UpdateRunReviewApprovedHeadSHA(id, headSHA string) error {
	_, err := d.sql.Exec(`UPDATE runs SET review_approved_head_sha = ?, updated_at = ? WHERE id = ?`, headSHA, now(), id)
	if err != nil {
		return fmt.Errorf("update run review-approved head sha: %w", err)
	}
	return nil
}

// UpdateRunHeadSHA updates the run head SHA and timestamp.
func (d *DB) UpdateRunHeadSHA(id, headSHA string) error {
	_, err := d.sql.Exec(`UPDATE runs SET head_sha = ?, updated_at = ? WHERE id = ?`, headSHA, now(), id)
	if err != nil {
		return fmt.Errorf("update run head sha: %w", err)
	}
	return nil
}

// UpdateRunError sets the error message on a run.
func (d *DB) UpdateRunError(id, errMsg string) error {
	return d.UpdateRunErrorStatus(id, errMsg, types.RunFailed)
}

// UpdateRunErrorStatus sets the error message and terminal status on a run.
func (d *DB) UpdateRunErrorStatus(id, errMsg string, status types.RunStatus) error {
	_, err := d.sql.Exec(`UPDATE runs SET error = ?, status = ?, push_active = 0, terminal_head_verified_at = NULL, updated_at = ? WHERE id = ?`, errMsg, status, now(), id)
	if err != nil {
		return fmt.Errorf("update run error: %w", err)
	}
	return nil
}

func (d *DB) UpdateRunErrorStatusWithVerifiedHead(id, errMsg string, status types.RunStatus, headSHA string) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET error = ?, status = ?, head_sha = ?, push_active = 0, terminal_head_verified_at = ?, updated_at = ? WHERE id = ?`, errMsg, status, headSHA, ts, ts, id)
	if err != nil {
		return fmt.Errorf("update run error with verified head: %w", err)
	}
	return nil
}

func (d *DB) UpdateRunStatusWithVerifiedHead(id string, status types.RunStatus, headSHA string) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET status = ?, head_sha = ?, push_active = 0, terminal_head_verified_at = ?, updated_at = ? WHERE id = ?`, status, headSHA, ts, ts, id)
	if err != nil {
		return fmt.Errorf("update run status with verified head: %w", err)
	}
	return nil
}

// RunIntentSourceAgent is the intent_source value stamped when the driving
// agent supplied the intent explicitly via `axi run --intent`. It marks an
// authoritative, author-stated goal (score 1) as opposed to a transcript
// inference (whose source is the matched agent name: "claude", "codex", ...).
// Prompt-construction code branches on this to frame an explicit intent as
// authoritative acceptance criteria rather than a low-confidence hint.
const RunIntentSourceAgent = "agent"

// RunIntentSourceRerun marks an authoritative intent inherited from the run
// selected for a rerun. It remains authoritative, but the distinct value keeps
// inherited intent inspectable instead of confusing it with a new override.
const RunIntentSourceRerun = "rerun"

// IsAuthoritativeRunIntentSource reports whether a run's intent came from an
// explicit operator/agent contract, either directly or through rerun
// inheritance.
func IsAuthoritativeRunIntentSource(source string) bool {
	return source == RunIntentSourceAgent || source == RunIntentSourceRerun
}

// RunIntent carries the four intent-related columns persisted on a run.
type RunIntent struct {
	Summary   string
	Source    string
	SessionID string
	Score     float64
}

// UpdateRunIntent persists the inferred user intent for a run.
func (d *DB) UpdateRunIntent(id string, intent RunIntent) error {
	_, err := d.sql.Exec(
		`UPDATE runs SET intent = ?, intent_source = ?, intent_session_id = ?, intent_score = ?, updated_at = ? WHERE id = ?`,
		intent.Summary, intent.Source, intent.SessionID, intent.Score, now(), id,
	)
	if err != nil {
		return fmt.Errorf("update run intent: %w", err)
	}
	return nil
}

// ParkRunForEnvironmentFailure records a non-terminal launch-time environment
// failure. It keeps the run active, publishes the diagnostic, and stamps the
// same awaiting-agent marker used by step approval gates.
func (d *DB) ParkRunForEnvironmentFailure(id, errMsg string) error {
	ts := now()
	_, err := d.sql.Exec(
		`UPDATE runs SET status = ?, error = ?, awaiting_agent_since = ?, updated_at = ? WHERE id = ?`,
		types.RunRunning, errMsg, ts, ts, id,
	)
	if err != nil {
		return fmt.Errorf("park run for environment failure: %w", err)
	}
	return nil
}

// SetRunAwaitingAgent marks a run as parked awaiting the driving agent,
// stamping awaiting_agent_since with the current time. Called by the executor
// when a step enters a gate (awaiting_approval / fix_review). Launch-time
// environment failures use ParkRunForEnvironmentFailure so their diagnostic
// and marker become observable atomically.
func (d *DB) SetRunAwaitingAgent(id string) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET awaiting_agent_since = ?, updated_at = ? WHERE id = ?`, ts, ts, id)
	if err != nil {
		return fmt.Errorf("set run awaiting agent: %w", err)
	}
	return nil
}

// ClearRunAwaitingAgent clears the awaiting-agent marker on a run. Called by the
// executor the moment the agent responds (or the approval wait is cancelled) and
// the run resumes.
func (d *DB) ClearRunAwaitingAgent(id string) error {
	_, err := d.sql.Exec(`UPDATE runs SET awaiting_agent_since = NULL, updated_at = ? WHERE id = ?`, now(), id)
	if err != nil {
		return fmt.Errorf("clear run awaiting agent: %w", err)
	}
	return nil
}

// AddRunParkedDuration accumulates parked-at-gate wall time onto a run's
// total. Called by the executor when a gate wait ends.
func (d *DB) AddRunParkedDuration(id string, ms int64) error {
	if ms <= 0 {
		return nil
	}
	_, err := d.sql.Exec(`UPDATE runs SET parked_ms = COALESCE(parked_ms, 0) + ?, updated_at = ? WHERE id = ?`, ms, now(), id)
	if err != nil {
		return fmt.Errorf("add run parked duration: %w", err)
	}
	return nil
}

func (d *DB) CompleteRunAwaitingAgent(id string, ms int64) error {
	if ms < 0 {
		ms = 0
	}
	_, err := d.sql.Exec(
		`UPDATE runs SET awaiting_agent_since = NULL,
			parked_ms = COALESCE(parked_ms, 0) + CASE WHEN awaiting_agent_since IS NOT NULL THEN ? ELSE 0 END,
			updated_at = ? WHERE id = ?`,
		ms, now(), id,
	)
	if err != nil {
		return fmt.Errorf("complete run awaiting agent: %w", err)
	}
	return nil
}

// RecoverStaleRuns marks any runs stuck in pending/running status as failed
// and fails any in-progress steps. This is called at daemon startup to clean
// up after a previous crash. Returns the number of recovered runs.
func (d *DB) RecoverStaleRuns(errMsg string) (int, error) {
	return d.RecoverStaleRunsExcept(errMsg, nil)
}

// RecoverStaleRunsExcept marks active runs as failed unless their IDs appear
// in preserved. Callers use preserved only after independently proving a run
// can be reconstructed safely.
func (d *DB) RecoverStaleRunsExcept(errMsg string, preserved map[string]struct{}) (int, error) {
	ts := now()

	tx, err := d.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	placeholders, args := recoveryExclusionClause(preserved)
	stepArgs := []any{
		types.StepStatusFailed, errMsg, ts,
		types.StepStatusRunning, types.StepStatusAwaitingApproval, types.StepStatusFixing, types.StepStatusFixReview,
		types.RunPending, types.RunRunning,
	}
	stepArgs = append(stepArgs, args...)
	_, err = tx.Exec(
		`UPDATE step_results SET status = ?, error = ?, completed_at = ?
		 WHERE status IN (?, ?, ?, ?) AND run_id IN (
			SELECT id FROM runs WHERE status IN (?, ?)`+placeholders+`
		 )`,
		stepArgs...,
	)
	if err != nil {
		return 0, fmt.Errorf("recover stale steps: %w", err)
	}

	// Fail stale runs. Clear any awaiting-agent marker so a recovered (now
	// failed) run is never reported as still parked awaiting the agent,
	// accumulating the marker's elapsed time into the run's parked total so
	// the parked evidence survives the crash.
	runArgs := []any{types.RunFailed, errMsg, ts, ts, ts, types.RunPending, types.RunRunning}
	runArgs = append(runArgs, args...)
	result, err := tx.Exec(
		`UPDATE runs SET status = ?, error = ?, push_active = 0,
			parked_ms = COALESCE(parked_ms, 0) + CASE
				WHEN awaiting_agent_since IS NOT NULL AND ? > awaiting_agent_since
				THEN (? - awaiting_agent_since) * 1000 ELSE 0 END,
			awaiting_agent_since = NULL, updated_at = ? WHERE status IN (?, ?)`+placeholders,
		runArgs...,
	)
	if err != nil {
		return 0, fmt.Errorf("recover stale runs: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}
	return int(count), nil
}

func recoveryExclusionClause(preserved map[string]struct{}) (string, []any) {
	if len(preserved) == 0 {
		return "", nil
	}
	args := make([]any, 0, len(preserved))
	placeholders := make([]string, 0, len(preserved))
	for id := range preserved {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	return " AND id NOT IN (" + strings.Join(placeholders, ", ") + ")", args
}

// GetRunCIRerunState returns the CI step's persisted rerun budget for a run, or
// the empty string when the run has never spent one. The payload is opaque
// here: the CI step owns its shape, and the database only guarantees that what
// was written survives a restart.
func (d *DB) GetRunCIRerunState(id string) (string, error) {
	var state sql.NullString
	err := d.sql.QueryRow(`SELECT ci_rerun_state FROM runs WHERE id = ?`, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get run ci rerun state: %w", err)
	}
	return state.String, nil
}

// SetRunCIRerunState persists the CI step's rerun budget. The CI step calls
// this before asking the provider to re-run a check, so a crash between the
// reservation and the request costs the budget instead of handing the recovered
// run a rerun the limit already accounted for.
func (d *DB) SetRunCIRerunState(id, state string) error {
	_, err := d.sql.Exec(`UPDATE runs SET ci_rerun_state = ?, updated_at = ? WHERE id = ?`, state, now(), id)
	if err != nil {
		return fmt.Errorf("set run ci rerun state: %w", err)
	}
	return nil
}
