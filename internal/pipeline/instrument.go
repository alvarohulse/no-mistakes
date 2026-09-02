package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// perfRecordingAgent decorates the step agent to persist one local
// agent_invocations row per invocation: identity, purpose, session mode,
// timing, exit status, and token usage. Recording is local-only and usually
// best-effort. A selected full-review route is the exception: its receipt is
// inserted before launch and must be finalized before the review can pass.
type perfRecordingAgent struct {
	inner               agent.Agent
	db                  *db.DB
	runID               string
	stepName            types.StepName
	reviewCandidatePool []db.ReviewCandidateReceipt
	// round returns the 1-based round the current invocation belongs to.
	round func() int
}

func (a *perfRecordingAgent) Name() string { return a.inner.Name() }

func (a *perfRecordingAgent) ConfiguredModel() agent.ModelIdentity {
	return agent.ConfiguredModel(a.inner)
}

func (a *perfRecordingAgent) Close() error { return a.inner.Close() }

// SupportsSessionResume forwards the wrapped adapter's session capability.
func (a *perfRecordingAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *perfRecordingAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *perfRecordingAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	if len(a.reviewCandidatePool) > 0 {
		return a.runWithRequiredReviewReceipt(ctx, opts)
	}

	attempts := 0
	previous := opts.OnAttempt
	opts.OnAttempt = func(attempt agent.Attempt) {
		if previous != nil {
			previous(attempt)
		}
		attempts++
		attemptOpts := opts
		attemptOpts.Session = attempt.Session
		attemptOpts.SessionFallback = attempt.SessionFallback
		a.recordBestEffort(ctx, attemptOpts, attempt.Agent, attempt.Result, attempt.Err, attempt.StartedAt, attempt.CompletedAt)
	}
	start := time.Now()
	result, err := a.inner.Run(ctx, opts)
	if attempts == 0 {
		a.recordBestEffort(ctx, opts, a.inner.Name(), result, err, start, time.Now())
	}
	return result, err
}

func (a *perfRecordingAgent) runWithRequiredReviewReceipt(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	if a.db == nil {
		return nil, fmt.Errorf("persist review selection receipt: database is unavailable")
	}
	startedAt := time.Now()
	pending := a.newInvocation(ctx, opts, a.inner.Name(), nil, nil, startedAt, startedAt)
	pending.ExitStatus = "started"
	persisted, err := a.db.InsertAgentInvocation(pending)
	if err != nil {
		return nil, fmt.Errorf("persist review selection receipt: %w", err)
	}

	attempts := 0
	var receiptErr error
	previous := opts.OnAttempt
	opts.OnAttempt = func(attempt agent.Attempt) {
		if previous != nil {
			previous(attempt)
		}
		attempts++
		attemptOpts := opts
		attemptOpts.Session = attempt.Session
		attemptOpts.SessionFallback = attempt.SessionFallback
		completed := a.newInvocation(ctx, attemptOpts, attempt.Agent, attempt.Result, attempt.Err, attempt.StartedAt, attempt.CompletedAt)
		if attempts == 1 {
			completed.ID = persisted.ID
			_, receiptErr = a.db.UpdateAgentInvocation(completed)
			return
		}
		a.insertBestEffort(completed)
	}

	result, runErr := a.inner.Run(ctx, opts)
	if attempts == 0 {
		completed := a.newInvocation(ctx, opts, a.inner.Name(), result, runErr, startedAt, time.Now())
		completed.ID = persisted.ID
		_, receiptErr = a.db.UpdateAgentInvocation(completed)
	}
	if receiptErr != nil {
		finalizeErr := fmt.Errorf("finalize review selection receipt: %w", receiptErr)
		if runErr != nil {
			return result, errors.Join(runErr, finalizeErr)
		}
		return result, finalizeErr
	}
	return result, runErr
}

func (a *perfRecordingAgent) recordBestEffort(ctx context.Context, opts agent.RunOpts, agentName string, result *agent.Result, runErr error, startedAt, completedAt time.Time) {
	if a.db == nil {
		return
	}
	a.insertBestEffort(a.newInvocation(ctx, opts, agentName, result, runErr, startedAt, completedAt))
}

func (a *perfRecordingAgent) insertBestEffort(inv db.AgentInvocation) {
	if _, dbErr := a.db.InsertAgentInvocation(inv); dbErr != nil {
		slog.Warn("failed to record agent invocation", "step", a.stepName, "error", dbErr)
	}
}

func (a *perfRecordingAgent) newInvocation(ctx context.Context, opts agent.RunOpts, agentName string, result *agent.Result, runErr error, startedAt, completedAt time.Time) db.AgentInvocation {
	purpose := opts.Purpose
	if purpose == "" {
		purpose = string(a.stepName)
	}

	sessionKey := invocationSessionKey(opts, result)
	inv := db.AgentInvocation{
		RunID:               a.runID,
		StepName:            string(a.stepName),
		Round:               a.round(),
		Purpose:             purpose,
		Agent:               agentName,
		UsageCoverage:       agent.UsageCoverageUnknown,
		InvocationMode:      types.AgentInvocationModeHarnessCLI,
		SessionMode:         invocationSessionMode(opts),
		SessionKey:          sessionKey,
		StartedAt:           startedAt.Unix(),
		CompletedAt:         completedAt.Unix(),
		DurationMS:          completedAt.Sub(startedAt).Milliseconds(),
		ExitStatus:          "ok",
		ReviewCandidatePool: append([]db.ReviewCandidateReceipt(nil), a.reviewCandidatePool...),
	}
	configuredModel := agent.ConfiguredModel(a.inner)
	inv.Model = configuredModel.Name
	if configuredModel.Vendor != "" {
		vendor := configuredModel.Vendor
		inv.ModelProvider = &vendor
	}
	if opts.SessionFallback && opts.SessionFallbackReason != "" {
		reason := opts.SessionFallbackReason
		inv.FallbackReason = &reason
	}
	if opts.Workload != nil {
		files, lines := opts.Workload.Files, opts.Workload.Lines
		inv.WorkloadFiles = &files
		inv.WorkloadLines = &lines
	}
	a.recordResult(&inv, sessionKey, result)
	if runErr != nil {
		if ctx.Err() != nil || errors.Is(runErr, context.Canceled) {
			inv.ExitStatus = "cancelled"
			inv.FailureCategory = "cancelled"
		} else {
			inv.ExitStatus = "error"
			inv.FailureCategory = classifyInvocationFailure(runErr)
		}
	}
	return inv
}

// recordResult folds a successful (or partially successful) result's identity,
// usage, per-round token deltas, and bounded activity metrics into inv.
// Unreported nullable metrics stay nil rather than becoming fabricated zeros;
// usage coverage is always persisted and defaults to unknown.
func (a *perfRecordingAgent) recordResult(inv *db.AgentInvocation, sessionKey string, result *agent.Result) {
	if result == nil {
		return
	}
	inv.UsageCoverage = result.UsageCoverage
	if inv.UsageCoverage == "" {
		inv.UsageCoverage = agent.UsageCoverageUnknown
	}
	if result.Model != "" {
		inv.Model = result.Model
	}
	inv.AgentObservationsReported = result.AgentObservationsReported
	inv.AgentObservations = append([]types.AgentObservation(nil), result.AgentObservations...)
	if result.AgentObservationsReported {
		count := result.NestedAgentCount
		if count == 0 && len(result.AgentObservations) > 0 {
			count = len(result.AgentObservations)
		}
		inv.NestedAgentCount = &count
	}
	if result.ModelProvider != "" {
		provider := result.ModelProvider
		inv.ModelProvider = &provider
	}
	inv.InputTokens = result.Usage.InputTokens
	inv.OutputTokens = result.Usage.OutputTokens
	inv.CacheReadTokens = result.Usage.CacheReadTokens
	usage := result.Usage
	if result.UsageReported {
		usage.Reported = true
	}
	inputReported := usage.InputIsReported()
	outputReported := usage.OutputIsReported()
	cacheReadReported := usage.CacheReadIsReported()
	cacheCreationReported := result.CacheCreationReported || usage.CacheCreationReported

	if cacheCreationReported {
		cacheCreation := result.Usage.CacheCreationTokens
		inv.CacheCreationTokens = &cacheCreation
	}
	if inputReported {
		input := usage.InputTokens
		var cacheRead *int
		if cacheReadReported {
			cacheRead = &usage.CacheReadTokens
		}
		_, inv.FreshInputTokens = agent.CanonicalInputMeters(inv.Agent, &input, cacheRead, inv.CacheCreationTokens)
	}
	inv.ReportedCostUSD = result.ReportedCostUSD

	// Per-round deltas: for a resumed session whose raw counters are cumulative,
	// subtract the same session's prior cumulative so the row cannot be mistaken
	// for per-round usage. Read the prior BEFORE this row is inserted.
	if inputReported || outputReported || cacheReadReported || cacheCreationReported {
		prior, hasPrior := a.db.LatestSessionCumulativeMeters(a.runID, sessionKey)
		inv.DeltaInputTokens = perRoundTokenMeter(usage.InputTokens, inputReported, prior.Input, hasPrior, result.SessionUsageCumulative)
		inv.DeltaOutputTokens = perRoundTokenMeter(usage.OutputTokens, outputReported, prior.Output, hasPrior, result.SessionUsageCumulative)
		inv.DeltaCacheReadTokens = perRoundTokenMeter(usage.CacheReadTokens, cacheReadReported, prior.CacheRead, hasPrior, result.SessionUsageCumulative)
		inv.DeltaCacheCreationTokens = perRoundTokenMeter(usage.CacheCreationTokens, cacheCreationReported, prior.CacheCreation, hasPrior, result.SessionUsageCumulative)
	}

	if result.Metrics != nil {
		m := result.Metrics
		// Reasoning tokens are reported only by adapters that also report
		// activity metrics (codex); a real zero there is meaningful.
		if result.UsageReported {
			reasoning := result.Usage.ReasoningTokens
			inv.ReasoningTokens = &reasoning
		}
		roundtrips := m.ModelRoundtrips
		inv.ModelRoundtrips = &roundtrips
		toolCalls := m.ToolCalls
		inv.ToolCalls = &toolCalls
		wait := m.ToolCategories.Wait
		testLint := m.ToolCategories.TestLint
		edit := m.ToolCategories.Edit
		read := m.ToolCategories.Read
		git := m.ToolCategories.Git
		other := m.ToolCategories.Other
		inv.ToolWaitCalls = &wait
		inv.ToolTestLintCalls = &testLint
		inv.ToolEditCalls = &edit
		inv.ToolReadCalls = &read
		inv.ToolGitCalls = &git
		inv.ToolOtherCalls = &other
		subprocessWait := m.SubprocessWaitMS
		inv.SubprocessWaitMS = &subprocessWait
	}

	if count, ok := countOutputFindings(result.Output); ok {
		inv.FindingCount = &count
	}
}

func perRoundTokenMeter(current int, reported bool, prior *int, hasPrior, cumulative bool) *int {
	if !reported || (cumulative && hasPrior && prior == nil) {
		return nil
	}
	priorValue := 0
	if prior != nil {
		priorValue = *prior
	}
	delta := agent.PerRoundTokens(current, priorValue, cumulative)
	return &delta
}

// countOutputFindings returns the number of findings in a structured output
// payload and whether the payload was findings-shaped at all (had a "findings"
// key). It never retains any finding content - only the count.
func countOutputFindings(output json.RawMessage) (int, bool) {
	if len(output) == 0 {
		return 0, false
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(output, &envelope); err != nil {
		return 0, false
	}
	raw, ok := envelope["findings"]
	if !ok {
		return 0, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return 0, false
	}
	return len(items), true
}

func invocationSessionMode(opts agent.RunOpts) string {
	switch {
	case opts.SessionFallback:
		return db.InvocationModeFallback
	case opts.Session == nil:
		return db.InvocationModeCold
	case opts.Session.ID != "":
		return db.InvocationModeResumed
	default:
		return db.InvocationModeStarted
	}
}

// invocationSessionKey fingerprints the session identity so reuse is
// auditable without storing the raw resumable id in the telemetry table.
func invocationSessionKey(opts agent.RunOpts, result *agent.Result) string {
	id := ""
	if result != nil && result.SessionID != "" {
		id = result.SessionID
	} else if opts.Session != nil && opts.Session.ID != "" {
		id = opts.Session.ID
	}
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

// classifyInvocationFailure buckets an invocation error into a
// low-cardinality category. Only the category is stored - never the error
// text, which can embed agent output.
func classifyInvocationFailure(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "parse events") || strings.Contains(msg, "output parse"):
		return "parse"
	case strings.Contains(msg, "exited"):
		return "exit"
	case strings.Contains(msg, "start"):
		return "spawn"
	default:
		return "other"
	}
}

// classifyFallbackReason buckets the error that failed a session resume into a
// low-cardinality reason (see db.FallbackReason*). Like classifyInvocationFailure
// it stores only the category, never the error text.
func classifyFallbackReason(err error) string {
	if err == nil {
		return db.FallbackReasonOther
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unexpected argument") || strings.Contains(msg, "unrecognized") ||
		strings.Contains(msg, "unknown flag") || strings.Contains(msg, "unexpected flag"):
		return db.FallbackReasonUnsupported
	case strings.Contains(msg, "parse events") || strings.Contains(msg, "output parse"):
		return db.FallbackReasonParse
	case strings.Contains(msg, "exited"):
		return db.FallbackReasonExit
	case strings.Contains(msg, "start"):
		return db.FallbackReasonSpawn
	case strings.Contains(msg, "temporarily") || strings.Contains(msg, "capacity") ||
		strings.Contains(msg, "rate limit") || strings.Contains(msg, "overloaded") ||
		strings.Contains(msg, "timeout"):
		return db.FallbackReasonTransient
	default:
		return db.FallbackReasonOther
	}
}
