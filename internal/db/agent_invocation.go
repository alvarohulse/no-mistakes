package db

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	maxReviewCandidateAgentBytes  = 128
	maxReviewCandidateModelBytes  = 256
	maxReviewCandidateVendorBytes = 128
)

// ReviewCandidateReceipt is the content-free route identity persisted with a
// cold full-review invocation. It contains no prompt, output, path, or command.
type ReviewCandidateReceipt struct {
	Agent    string `json:"agent"`
	Model    string `json:"model"`
	Vendor   string `json:"vendor"`
	Optional bool   `json:"optional,omitempty"`
}

// Agent invocation session modes recorded for local performance telemetry.
const (
	InvocationModeCold     = "cold"     // no durable session involved
	InvocationModeStarted  = "started"  // began a new durable session
	InvocationModeResumed  = "resumed"  // resumed an existing durable session
	InvocationModeFallback = "fallback" // fresh session after a failed resume
)

// Fallback reasons classify why a resume failed and forced a fresh-session
// retry. They are low-cardinality and content-free (never the error text, which
// can embed agent output), so a silent-fallback regression is both countable
// and diagnosable from telemetry alone.
const (
	FallbackReasonTransient   = "transient"   // retryable provider/transport error
	FallbackReasonParse       = "parse"       // could not parse the resumed output
	FallbackReasonExit        = "exit"        // resumed process exited non-zero
	FallbackReasonSpawn       = "spawn"       // resumed process failed to start
	FallbackReasonUnsupported = "unsupported" // adapter rejected a resume flag
	FallbackReasonOther       = "other"       // anything else
)

// AgentInvocation is one agent process invocation's local performance
// evidence. It stores identity, timing, session mode, bounded activity counts,
// and token usage only - never prompts, model outputs, diffs, raw command
// arguments, or credentials - and it stays local: no per-invocation record is
// ever sent to remote telemetry.
//
// Fields typed as pointers are nullable: a nil value means the datum was not
// reported for this invocation and is recorded as unknown, never a fabricated
// zero. Pre-existing rows created before these columns existed read back as
// nil, so they too report unknown honestly.
type AgentInvocation struct {
	ID       string
	RunID    string
	StepName string
	Round    int
	// Purpose is the pipeline duty served: review, review-fix,
	// test-evidence, document, pr, intent, a
	// `<step>-plan` read-only command plan, or a step-derived default.
	Purpose string
	Agent   string
	// UsageCoverage is the adapter's explicit statement about whether its
	// top-level usage totals account for all work in this invocation.
	UsageCoverage agent.UsageCoverage
	// InvocationMode is how the top-level adapter was invoked. Pipeline agent
	// processes use harness_cli; nested event-stream observations use
	// subagent_tool in AgentObservations.
	InvocationMode types.AgentInvocationMode
	// AgentObservations is the ordered list of nested agent invocations exposed
	// by the adapter stream. AgentObservationsReported distinguishes a supported
	// stream with no nested invocations from an adapter that exposes no such
	// evidence.
	AgentObservations         []types.AgentObservation
	AgentObservationsReported bool
	// NestedAgentCount is the exact unique child count when the adapter reports
	// nesting. Nil means unsupported; a non-nil zero means supported and none.
	NestedAgentCount *int
	Model            string
	// ModelProvider is the provider that served the model (openai, anthropic,
	// ...). Nil when the adapter cannot report it.
	ModelProvider *string
	// ReviewCandidatePool is the final usable pool considered for this full
	// review. Nil for non-review invocations and legacy rows.
	ReviewCandidatePool []ReviewCandidateReceipt
	// SessionMode is one of the InvocationMode constants.
	SessionMode string
	// SessionKey is a privacy-safe fingerprint (truncated SHA-256) of the
	// adapter-native session identity, so session reuse is auditable without
	// storing the raw resumable identity in a second place.
	SessionKey string
	// FallbackReason classifies why a fallback invocation happened (one of the
	// FallbackReason constants). Nil unless SessionMode is fallback.
	FallbackReason  *string
	StartedAt       int64
	CompletedAt     int64
	DurationMS      int64
	ExitStatus      string // started | ok | error | cancelled
	FailureCategory string // parse | exit | spawn | cancelled | other ("" when ok)
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
	// CacheCreationTokens is the provider's cache-creation cost. Nil when the
	// provider does not surface it (codex), distinguishing "not reported" from a
	// genuine zero.
	CacheCreationTokens *int
	// FreshInputTokens is the adapter's canonical uncached-input meter. Nil
	// when the CLI's cache relationship is not verified.
	FreshInputTokens *int
	// ReasoningTokens is the model's hidden-reasoning output tokens, when the
	// provider reports them. Nil when not reported.
	ReasoningTokens *int
	// SubprocessWaitMS is the wall-clock this invocation spent inside tool
	// subprocesses; DurationMS minus it is model/reasoning time. Nil when the
	// adapter reported no activity metrics.
	SubprocessWaitMS *int64
	// Delta* are the per-round token amounts for resumed durable sessions whose
	// raw counters are cumulative: current cumulative minus the same session's
	// prior cumulative. For cold/started/fallback rows they equal the raw
	// counters. Nil when no usage was reported.
	DeltaInputTokens         *int
	DeltaOutputTokens        *int
	DeltaCacheReadTokens     *int
	DeltaCacheCreationTokens *int
	// ReportedCostUSD is the cost emitted by the agent CLI, when available.
	ReportedCostUSD *float64
	// PricingReceiptJSON retains immutable three-class cost receipts written by
	// older producers. New rows leave it nil; readers must never recalculate it.
	PricingReceiptJSON *string
	// ModelRoundtrips is the count of model-authored items (messages + tool
	// calls) - a live-stream proxy for productive model round-trips. Nil when
	// the adapter reported no activity metrics.
	ModelRoundtrips *int
	// ToolCalls is the count of whole tool invocations. Nil when unknown.
	ToolCalls *int
	// Tool*Calls is the bounded per-category sub-command histogram. Because a
	// compound command counts once per sub-command, their sum can exceed
	// ToolCalls. Nil when the adapter reported no activity metrics.
	ToolWaitCalls     *int
	ToolTestLintCalls *int
	ToolEditCalls     *int
	ToolReadCalls     *int
	ToolGitCalls      *int
	ToolOtherCalls    *int
	// WorkloadFiles and WorkloadLines record the bounded size of the change this
	// invocation worked over. Nil for invocations with no meaningful workload
	// (or steps that do not supply it).
	WorkloadFiles *int
	WorkloadLines *int
	// FindingCount is the number of findings in this invocation's structured
	// output. Nil when the output is not findings-shaped.
	FindingCount *int
}

// agentInvocationColumns is the canonical column order shared by insert and
// select so the placeholder list and scan destinations cannot drift apart.
const agentInvocationColumns = `id, run_id, step_name, round, purpose, agent, usage_coverage, invocation_mode, agent_observations_json, nested_agent_count, model, model_provider, review_candidate_pool_json,
	session_mode, session_key, fallback_reason,
	started_at, completed_at, duration_ms, subprocess_wait_ms, exit_status, failure_category,
	input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
	fresh_input_tokens, reasoning_tokens,
	delta_input_tokens, delta_output_tokens, delta_cache_read_tokens, delta_cache_creation_tokens, reported_cost_usd, pricing_receipt_json,
	model_roundtrips, tool_calls,
	tool_wait_calls, tool_test_lint_calls, tool_edit_calls, tool_read_calls, tool_git_calls, tool_other_calls,
	workload_files, workload_lines, finding_count`

// agentInvocationInsertPlaceholders has one '?' per agentInvocationColumns entry.
const agentInvocationInsertPlaceholders = `?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?,
	?, ?,
	?, ?, ?,
	?, ?,
	?, ?, ?, ?, ?, ?,
	?, ?, ?`

// InsertAgentInvocation records an agent invocation. Review routing may insert
// a started row before launching the selected harness, then finalize it with
// UpdateAgentInvocation. Nil pointer fields are stored as SQL NULL
// (database/sql dereferences non-nil pointers).
func (d *DB) InsertAgentInvocation(inv AgentInvocation) (*AgentInvocation, error) {
	if err := normalizeUsageCoverage(&inv); err != nil {
		return nil, err
	}
	if inv.InvocationMode == "" {
		inv.InvocationMode = types.AgentInvocationModeHarnessCLI
	}
	observationsJSON, err := encodeAgentObservations(inv)
	if err != nil {
		return nil, fmt.Errorf("encode agent observations: %w", err)
	}
	reviewCandidatePoolJSON, err := encodeReviewCandidatePool(inv.ReviewCandidatePool)
	if err != nil {
		return nil, fmt.Errorf("encode review candidate pool: %w", err)
	}
	inv.ID = newID()
	_, err = d.sql.Exec(
		`INSERT INTO agent_invocations (`+agentInvocationColumns+`)
		 VALUES (`+agentInvocationInsertPlaceholders+`)`,
		inv.ID, inv.RunID, inv.StepName, inv.Round, inv.Purpose, inv.Agent, inv.UsageCoverage, inv.InvocationMode, observationsJSON, inv.NestedAgentCount, inv.Model, inv.ModelProvider, reviewCandidatePoolJSON,
		inv.SessionMode, inv.SessionKey, inv.FallbackReason,
		inv.StartedAt, inv.CompletedAt, inv.DurationMS, inv.SubprocessWaitMS, inv.ExitStatus, inv.FailureCategory,
		inv.InputTokens, inv.OutputTokens, inv.CacheReadTokens, inv.CacheCreationTokens,
		inv.FreshInputTokens, inv.ReasoningTokens,
		inv.DeltaInputTokens, inv.DeltaOutputTokens, inv.DeltaCacheReadTokens, inv.DeltaCacheCreationTokens, inv.ReportedCostUSD, inv.PricingReceiptJSON,
		inv.ModelRoundtrips, inv.ToolCalls,
		inv.ToolWaitCalls, inv.ToolTestLintCalls, inv.ToolEditCalls, inv.ToolReadCalls, inv.ToolGitCalls, inv.ToolOtherCalls,
		inv.WorkloadFiles, inv.WorkloadLines, inv.FindingCount,
	)
	if err != nil {
		return nil, fmt.Errorf("insert agent invocation: %w", err)
	}
	return &inv, nil
}

// UpdateAgentInvocation finalizes a previously inserted invocation while
// retaining its stable receipt identity.
func (d *DB) UpdateAgentInvocation(inv AgentInvocation) (*AgentInvocation, error) {
	if strings.TrimSpace(inv.ID) == "" {
		return nil, fmt.Errorf("update agent invocation: id is required")
	}
	if err := normalizeUsageCoverage(&inv); err != nil {
		return nil, err
	}
	if inv.InvocationMode == "" {
		inv.InvocationMode = types.AgentInvocationModeHarnessCLI
	}
	observationsJSON, err := encodeAgentObservations(inv)
	if err != nil {
		return nil, fmt.Errorf("encode agent observations: %w", err)
	}
	reviewCandidatePoolJSON, err := encodeReviewCandidatePool(inv.ReviewCandidatePool)
	if err != nil {
		return nil, fmt.Errorf("encode review candidate pool: %w", err)
	}
	result, err := d.sql.Exec(`UPDATE agent_invocations SET
		run_id = ?, step_name = ?, round = ?, purpose = ?, agent = ?, usage_coverage = ?, invocation_mode = ?, agent_observations_json = ?, nested_agent_count = ?, model = ?, model_provider = ?, review_candidate_pool_json = ?,
		session_mode = ?, session_key = ?, fallback_reason = ?,
		started_at = ?, completed_at = ?, duration_ms = ?, subprocess_wait_ms = ?, exit_status = ?, failure_category = ?,
		input_tokens = ?, output_tokens = ?, cache_read_tokens = ?, cache_creation_tokens = ?,
		fresh_input_tokens = ?, reasoning_tokens = ?,
		delta_input_tokens = ?, delta_output_tokens = ?, delta_cache_read_tokens = ?, delta_cache_creation_tokens = ?, reported_cost_usd = ?,
		pricing_receipt_json = COALESCE(pricing_receipt_json, ?),
		model_roundtrips = ?, tool_calls = ?,
		tool_wait_calls = ?, tool_test_lint_calls = ?, tool_edit_calls = ?, tool_read_calls = ?, tool_git_calls = ?, tool_other_calls = ?,
		workload_files = ?, workload_lines = ?, finding_count = ?
		WHERE id = ?`,
		inv.RunID, inv.StepName, inv.Round, inv.Purpose, inv.Agent, inv.UsageCoverage, inv.InvocationMode, observationsJSON, inv.NestedAgentCount, inv.Model, inv.ModelProvider, reviewCandidatePoolJSON,
		inv.SessionMode, inv.SessionKey, inv.FallbackReason,
		inv.StartedAt, inv.CompletedAt, inv.DurationMS, inv.SubprocessWaitMS, inv.ExitStatus, inv.FailureCategory,
		inv.InputTokens, inv.OutputTokens, inv.CacheReadTokens, inv.CacheCreationTokens,
		inv.FreshInputTokens, inv.ReasoningTokens,
		inv.DeltaInputTokens, inv.DeltaOutputTokens, inv.DeltaCacheReadTokens, inv.DeltaCacheCreationTokens, inv.ReportedCostUSD, inv.PricingReceiptJSON,
		inv.ModelRoundtrips, inv.ToolCalls,
		inv.ToolWaitCalls, inv.ToolTestLintCalls, inv.ToolEditCalls, inv.ToolReadCalls, inv.ToolGitCalls, inv.ToolOtherCalls,
		inv.WorkloadFiles, inv.WorkloadLines, inv.FindingCount,
		inv.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("update agent invocation: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("update agent invocation rows affected: %w", err)
	}
	if updated != 1 {
		return nil, fmt.Errorf("update agent invocation: expected 1 row, updated %d", updated)
	}
	return &inv, nil
}

// GetAgentInvocationsByRun returns a run's invocations in execution order.
func (d *DB) GetAgentInvocationsByRun(runID string) ([]AgentInvocation, error) {
	rows, err := d.sql.Query(
		`SELECT `+agentInvocationColumns+` FROM agent_invocations WHERE run_id = ? ORDER BY started_at, id`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get agent invocations: %w", err)
	}
	defer rows.Close()

	var invocations []AgentInvocation
	for rows.Next() {
		inv, err := scanAgentInvocation(rows)
		if err != nil {
			return nil, err
		}
		invocations = append(invocations, inv)
	}
	return invocations, rows.Err()
}

// AgentInvocationAuditTotals is an independent SQL aggregate used to verify
// that a run audit's emitted invocation rows and totals agree.
type AgentInvocationAuditTotals struct {
	Rows                       int
	DeltaInputReported         int
	DeltaInputSum              *int64
	DeltaOutputReported        int
	DeltaOutputSum             *int64
	DeltaCacheReadReported     int
	DeltaCacheReadSum          *int64
	DeltaCacheCreationReported int
	DeltaCacheCreationSum      *int64
	ReportedCostReported       int
	ReportedCostSum            *float64
}

// GetAgentInvocationAuditTotals returns nullable per-round-meter aggregates.
// SUM remains nullable: no reported rows is unknown, not zero.
func (d *DB) GetAgentInvocationAuditTotals(runID string) (AgentInvocationAuditTotals, error) {
	var totals AgentInvocationAuditTotals
	err := d.sql.QueryRow(`
		SELECT COUNT(*),
		       COUNT(delta_input_tokens), SUM(delta_input_tokens),
		       COUNT(delta_output_tokens), SUM(delta_output_tokens),
		       COUNT(delta_cache_read_tokens), SUM(delta_cache_read_tokens),
		       COUNT(delta_cache_creation_tokens), SUM(delta_cache_creation_tokens),
		       COUNT(reported_cost_usd), SUM(reported_cost_usd)
		FROM agent_invocations
		WHERE run_id = ?`, runID,
	).Scan(
		&totals.Rows,
		&totals.DeltaInputReported, &totals.DeltaInputSum,
		&totals.DeltaOutputReported, &totals.DeltaOutputSum,
		&totals.DeltaCacheReadReported, &totals.DeltaCacheReadSum,
		&totals.DeltaCacheCreationReported, &totals.DeltaCacheCreationSum,
		&totals.ReportedCostReported, &totals.ReportedCostSum,
	)
	if err != nil {
		return AgentInvocationAuditTotals{}, fmt.Errorf("get agent invocation audit totals: %w", err)
	}
	return totals, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanAgentInvocation(row scanner) (AgentInvocation, error) {
	var inv AgentInvocation
	var observationsJSON *string
	var reviewCandidatePoolJSON *string
	if err := row.Scan(
		&inv.ID, &inv.RunID, &inv.StepName, &inv.Round, &inv.Purpose, &inv.Agent, &inv.UsageCoverage, &inv.InvocationMode, &observationsJSON, &inv.NestedAgentCount, &inv.Model, &inv.ModelProvider, &reviewCandidatePoolJSON,
		&inv.SessionMode, &inv.SessionKey, &inv.FallbackReason,
		&inv.StartedAt, &inv.CompletedAt, &inv.DurationMS, &inv.SubprocessWaitMS, &inv.ExitStatus, &inv.FailureCategory,
		&inv.InputTokens, &inv.OutputTokens, &inv.CacheReadTokens, &inv.CacheCreationTokens,
		&inv.FreshInputTokens, &inv.ReasoningTokens,
		&inv.DeltaInputTokens, &inv.DeltaOutputTokens, &inv.DeltaCacheReadTokens, &inv.DeltaCacheCreationTokens, &inv.ReportedCostUSD, &inv.PricingReceiptJSON,
		&inv.ModelRoundtrips, &inv.ToolCalls,
		&inv.ToolWaitCalls, &inv.ToolTestLintCalls, &inv.ToolEditCalls, &inv.ToolReadCalls, &inv.ToolGitCalls, &inv.ToolOtherCalls,
		&inv.WorkloadFiles, &inv.WorkloadLines, &inv.FindingCount,
	); err != nil {
		return AgentInvocation{}, fmt.Errorf("scan agent invocation: %w", err)
	}
	if observationsJSON != nil {
		inv.AgentObservationsReported = true
		if err := json.Unmarshal([]byte(*observationsJSON), &inv.AgentObservations); err != nil {
			return AgentInvocation{}, fmt.Errorf("decode agent observations: %w", err)
		}
	}
	if reviewCandidatePoolJSON != nil {
		if err := json.Unmarshal([]byte(*reviewCandidatePoolJSON), &inv.ReviewCandidatePool); err != nil {
			return AgentInvocation{}, fmt.Errorf("decode review candidate pool: %w", err)
		}
		if err := validateReviewCandidatePool(inv.ReviewCandidatePool); err != nil {
			return AgentInvocation{}, fmt.Errorf("decode review candidate pool: %w", err)
		}
	}
	return inv, nil
}

func normalizeUsageCoverage(inv *AgentInvocation) error {
	if inv.UsageCoverage == "" {
		inv.UsageCoverage = agent.UsageCoverageUnknown
	}
	if !inv.UsageCoverage.Valid() {
		return fmt.Errorf("agent invocation usage coverage %q is invalid", inv.UsageCoverage)
	}
	return nil
}

func encodeReviewCandidatePool(pool []ReviewCandidateReceipt) (*string, error) {
	if pool == nil {
		return nil, nil
	}
	if err := validateReviewCandidatePool(pool); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(pool)
	if err != nil {
		return nil, err
	}
	value := string(encoded)
	return &value, nil
}

func validateReviewCandidatePool(pool []ReviewCandidateReceipt) error {
	if len(pool) == 0 {
		return fmt.Errorf("review candidate pool must not be empty")
	}
	seen := make(map[string]bool, len(pool))
	for i, candidate := range pool {
		if err := validateReviewCandidateIdentity("agent", candidate.Agent, maxReviewCandidateAgentBytes); err != nil {
			return fmt.Errorf("review candidate %d: %w", i+1, err)
		}
		if err := validateReviewCandidateIdentity("model", candidate.Model, maxReviewCandidateModelBytes); err != nil {
			return fmt.Errorf("review candidate %d: %w", i+1, err)
		}
		if err := validateReviewCandidateIdentity("vendor", candidate.Vendor, maxReviewCandidateVendorBytes); err != nil {
			return fmt.Errorf("review candidate %d: %w", i+1, err)
		}
		key := candidate.Agent + "\x00" + candidate.Model + "\x00" + candidate.Vendor
		if seen[key] {
			return fmt.Errorf("review candidate %d duplicates %s/%s", i+1, candidate.Agent, candidate.Model)
		}
		seen[key] = true
	}
	return nil
}

func validateReviewCandidateIdentity(field, value string, maxBytes int) error {
	if value == "" || value != strings.TrimSpace(value) {
		return fmt.Errorf("candidate %s must be a non-empty trimmed identity", field)
	}
	if len(value) > maxBytes || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("candidate %s is not a bounded printable identity", field)
	}
	return nil
}

func encodeAgentObservations(inv AgentInvocation) (*string, error) {
	if !inv.AgentObservationsReported {
		return nil, nil
	}
	observations := inv.AgentObservations
	if observations == nil {
		observations = []types.AgentObservation{}
	}
	encoded, err := json.Marshal(observations)
	if err != nil {
		return nil, err
	}
	value := string(encoded)
	return &value, nil
}

// LatestSessionCumulative returns the most recent prior invocation's cumulative
// token counters for the same run and non-empty session key. It is how the
// pipeline computes a resumed session's per-round delta (current cumulative
// minus this prior). found is false when the session has no prior invocation
// (cold, started, or a fresh fallback), in which case the current counters are
// already per-round.
func (d *DB) LatestSessionCumulative(runID, sessionKey string) (input, output, cacheRead int, found bool) {
	input, output, cacheRead, _, found = d.LatestSessionCumulativeWithCacheCreation(runID, sessionKey)
	return
}

func (d *DB) LatestSessionCumulativeWithCacheCreation(runID, sessionKey string) (input, output, cacheRead, cacheCreation int, found bool) {
	meters, found := d.LatestSessionCumulativeMeters(runID, sessionKey)
	if !found {
		return 0, 0, 0, 0, false
	}
	value := func(meter *int) int {
		if meter == nil {
			return 0
		}
		return *meter
	}
	return value(meters.Input), value(meters.Output), value(meters.CacheRead), value(meters.CacheCreation), true
}

type SessionCumulativeMeters struct {
	Input         *int
	Output        *int
	CacheRead     *int
	CacheCreation *int
}

func (d *DB) LatestSessionCumulativeMeters(runID, sessionKey string) (SessionCumulativeMeters, bool) {
	if sessionKey == "" {
		return SessionCumulativeMeters{}, false
	}
	var meters SessionCumulativeMeters
	err := d.sql.QueryRow(
		`SELECT CASE WHEN delta_input_tokens IS NOT NULL THEN input_tokens END,
		        CASE WHEN delta_output_tokens IS NOT NULL THEN output_tokens END,
		        CASE WHEN delta_cache_read_tokens IS NOT NULL THEN cache_read_tokens END,
		        CASE WHEN delta_cache_creation_tokens IS NOT NULL THEN cache_creation_tokens END
		 FROM agent_invocations
		 WHERE run_id = ? AND session_key = ?
		 ORDER BY started_at DESC, id DESC LIMIT 1`,
		runID, sessionKey,
	).Scan(&meters.Input, &meters.Output, &meters.CacheRead, &meters.CacheCreation)
	if err != nil {
		return SessionCumulativeMeters{}, false
	}
	return meters, true
}

// AgentInvocationAggregate summarizes invocations for one purpose, powering
// the read-only performance report. Nullable sums preserve unknown when no row
// reported that metric. MetricsRows reports activity-metric coverage.
type AgentInvocationAggregate struct {
	Purpose             string
	Count               int
	TotalDurationMS     int64
	AvgDurationMS       int64
	SubprocessWaitMS    *int64
	Cold                int
	Started             int
	Resumed             int
	Fallback            int
	Errors              int
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens *int64
	FreshInputTokens    *int64
	ReasoningTokens     *int64
	ModelRoundtrips     *int64
	ToolCalls           *int64
	ToolWaitCalls       *int64
	ToolTestLintCalls   *int64
	ToolEditCalls       *int64
	ToolReadCalls       *int64
	ToolGitCalls        *int64
	ToolOtherCalls      *int64
	// MetricsRows counts invocations in the group whose adapter reported
	// activity metrics (model_roundtrips is non-NULL).
	MetricsRows int
}

// AgentInvocationAggregates returns per-purpose aggregates across all runs,
// largest total duration first.
func (d *DB) AgentInvocationAggregates() ([]AgentInvocationAggregate, error) {
	rows, err := d.sql.Query(`
		SELECT purpose,
		       COUNT(*),
		       COALESCE(SUM(duration_ms), 0),
		       CASE WHEN COUNT(subprocess_wait_ms) = COUNT(*) THEN SUM(subprocess_wait_ms) END,
		       COALESCE(SUM(CASE WHEN session_mode = 'cold' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN session_mode = 'started' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN session_mode = 'resumed' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN session_mode = 'fallback' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN exit_status != 'ok' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0),
		       CASE WHEN COUNT(cache_creation_tokens) = COUNT(*) THEN SUM(cache_creation_tokens) END,
		       CASE WHEN COUNT(fresh_input_tokens) = COUNT(*) THEN SUM(fresh_input_tokens) END,
		       CASE WHEN COUNT(reasoning_tokens) = COUNT(*) THEN SUM(reasoning_tokens) END,
		       CASE WHEN COUNT(model_roundtrips) = COUNT(*) THEN SUM(model_roundtrips) END,
		       CASE WHEN COUNT(tool_calls) = COUNT(*) THEN SUM(tool_calls) END,
		       CASE WHEN COUNT(tool_wait_calls) = COUNT(*) THEN SUM(tool_wait_calls) END,
		       CASE WHEN COUNT(tool_test_lint_calls) = COUNT(*) THEN SUM(tool_test_lint_calls) END,
		       CASE WHEN COUNT(tool_edit_calls) = COUNT(*) THEN SUM(tool_edit_calls) END,
		       CASE WHEN COUNT(tool_read_calls) = COUNT(*) THEN SUM(tool_read_calls) END,
		       CASE WHEN COUNT(tool_git_calls) = COUNT(*) THEN SUM(tool_git_calls) END,
		       CASE WHEN COUNT(tool_other_calls) = COUNT(*) THEN SUM(tool_other_calls) END,
		       COALESCE(SUM(CASE WHEN model_roundtrips IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM agent_invocations
		GROUP BY purpose
		ORDER BY SUM(duration_ms) DESC`)
	if err != nil {
		return nil, fmt.Errorf("agent invocation aggregates: %w", err)
	}
	defer rows.Close()

	var aggregates []AgentInvocationAggregate
	for rows.Next() {
		var a AgentInvocationAggregate
		if err := rows.Scan(
			&a.Purpose, &a.Count, &a.TotalDurationMS, &a.SubprocessWaitMS,
			&a.Cold, &a.Started, &a.Resumed, &a.Fallback, &a.Errors,
			&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheCreationTokens,
			&a.FreshInputTokens, &a.ReasoningTokens, &a.ModelRoundtrips, &a.ToolCalls,
			&a.ToolWaitCalls, &a.ToolTestLintCalls, &a.ToolEditCalls, &a.ToolReadCalls, &a.ToolGitCalls, &a.ToolOtherCalls,
			&a.MetricsRows,
		); err != nil {
			return nil, fmt.Errorf("scan agent invocation aggregate: %w", err)
		}
		if a.Count > 0 {
			a.AvgDurationMS = a.TotalDurationMS / int64(a.Count)
		}
		aggregates = append(aggregates, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	receipts, err := d.GetRunMetricReceipts()
	if err != nil {
		return nil, err
	}
	for _, receipt := range receipts {
		aggregates = append(aggregates, receipt.AgentAggregates...)
	}
	return mergeAgentInvocationAggregates(aggregates), nil
}

func mergeAgentInvocationAggregates(values []AgentInvocationAggregate) []AgentInvocationAggregate {
	byPurpose := make(map[string][]AgentInvocationAggregate)
	for _, value := range values {
		byPurpose[value.Purpose] = append(byPurpose[value.Purpose], value)
	}
	result := make([]AgentInvocationAggregate, 0, len(byPurpose))
	for purpose, groups := range byPurpose {
		combined := AgentInvocationAggregate{Purpose: purpose}
		for _, group := range groups {
			combined.Count += group.Count
			combined.TotalDurationMS += group.TotalDurationMS
			combined.Cold += group.Cold
			combined.Started += group.Started
			combined.Resumed += group.Resumed
			combined.Fallback += group.Fallback
			combined.Errors += group.Errors
			combined.InputTokens += group.InputTokens
			combined.OutputTokens += group.OutputTokens
			combined.CacheReadTokens += group.CacheReadTokens
			combined.MetricsRows += group.MetricsRows
		}
		combined.SubprocessWaitMS = sumOptionalAggregate(groups, func(value AgentInvocationAggregate) *int64 { return value.SubprocessWaitMS })
		combined.CacheCreationTokens = sumOptionalAggregate(groups, func(value AgentInvocationAggregate) *int64 { return value.CacheCreationTokens })
		combined.FreshInputTokens = sumOptionalAggregate(groups, func(value AgentInvocationAggregate) *int64 { return value.FreshInputTokens })
		combined.ReasoningTokens = sumOptionalAggregate(groups, func(value AgentInvocationAggregate) *int64 { return value.ReasoningTokens })
		combined.ModelRoundtrips = sumOptionalAggregate(groups, func(value AgentInvocationAggregate) *int64 { return value.ModelRoundtrips })
		combined.ToolCalls = sumOptionalAggregate(groups, func(value AgentInvocationAggregate) *int64 { return value.ToolCalls })
		combined.ToolWaitCalls = sumOptionalAggregate(groups, func(value AgentInvocationAggregate) *int64 { return value.ToolWaitCalls })
		combined.ToolTestLintCalls = sumOptionalAggregate(groups, func(value AgentInvocationAggregate) *int64 { return value.ToolTestLintCalls })
		combined.ToolEditCalls = sumOptionalAggregate(groups, func(value AgentInvocationAggregate) *int64 { return value.ToolEditCalls })
		combined.ToolReadCalls = sumOptionalAggregate(groups, func(value AgentInvocationAggregate) *int64 { return value.ToolReadCalls })
		combined.ToolGitCalls = sumOptionalAggregate(groups, func(value AgentInvocationAggregate) *int64 { return value.ToolGitCalls })
		combined.ToolOtherCalls = sumOptionalAggregate(groups, func(value AgentInvocationAggregate) *int64 { return value.ToolOtherCalls })
		if combined.Count > 0 {
			combined.AvgDurationMS = combined.TotalDurationMS / int64(combined.Count)
		}
		result = append(result, combined)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].TotalDurationMS != result[j].TotalDurationMS {
			return result[i].TotalDurationMS > result[j].TotalDurationMS
		}
		return result[i].Purpose < result[j].Purpose
	})
	return result
}

func sumOptionalAggregate(values []AgentInvocationAggregate, field func(AgentInvocationAggregate) *int64) *int64 {
	total := int64(0)
	for _, value := range values {
		item := field(value)
		if item == nil {
			return nil
		}
		total += *item
	}
	return &total
}

// RunInvocationSummary is the low-cardinality per-run rollup used for the
// bounded terminal remote summary (counts only - no ids, paths, or models).
type RunInvocationSummary struct {
	Count           int
	Resumed         int
	Fallback        int
	TotalDurationMS int64
}

// AgentInvocationSummaryForRun returns the run's invocation rollup.
func (d *DB) AgentInvocationSummaryForRun(runID string) (RunInvocationSummary, error) {
	var s RunInvocationSummary
	err := d.sql.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN session_mode = 'resumed' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN session_mode = 'fallback' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(duration_ms), 0)
		FROM agent_invocations WHERE run_id = ?`, runID).
		Scan(&s.Count, &s.Resumed, &s.Fallback, &s.TotalDurationMS)
	if err != nil {
		return RunInvocationSummary{}, fmt.Errorf("agent invocation summary: %w", err)
	}
	return s, nil
}
