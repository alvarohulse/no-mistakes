package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// cumulativeSessionAgent models a stable durable session whose reported token
// usage is cumulative across resumed rounds, including cache-write meters.
type cumulativeSessionAgent struct {
	round         int
	cumInput      int
	cumOutput     int
	cumCacheRead  int
	cumCacheWrite int
}

func (a *cumulativeSessionAgent) Name() string                { return "codex" }
func (a *cumulativeSessionAgent) Close() error                { return nil }
func (a *cumulativeSessionAgent) SupportsSessionResume() bool { return true }

func (a *cumulativeSessionAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.round++
	// Round 1 adds 1000/100/600; round 2 adds 1500/150/1200 - so the cumulative
	// counters grow and the per-round deltas are the additions.
	switch a.round {
	case 1:
		a.cumInput, a.cumOutput, a.cumCacheRead, a.cumCacheWrite = 1000, 100, 600, 20
	default:
		a.cumInput, a.cumOutput, a.cumCacheRead, a.cumCacheWrite = 2500, 250, 1800, 70
	}
	reportedCost := float64(a.round) * 1.25
	return &agent.Result{
		Output:        json.RawMessage(`{"findings":[{"severity":"warning","description":"x","action":"auto-fix"},{"severity":"error","description":"y","action":"ask-user"}],"summary":"s"}`),
		SessionID:     "sess-abc",
		Resumed:       opts.Session != nil && opts.Session.ID != "",
		Model:         "gpt-5.6-sol",
		ModelProvider: "openai",
		Usage: agent.TokenUsage{
			InputTokens:           a.cumInput,
			OutputTokens:          a.cumOutput,
			CacheReadTokens:       a.cumCacheRead,
			CacheCreationTokens:   a.cumCacheWrite,
			CacheCreationReported: true,
			ReasoningTokens:       5 * a.round,
		},
		UsageReported:   true,
		ReportedCostUSD: &reportedCost,
		Metrics: &agent.InvocationMetrics{
			ModelRoundtrips:  4,
			ToolCalls:        3,
			ToolCategories:   agent.ToolCategoryCounts{TestLint: 1, Edit: 1, Read: 1},
			SubprocessWaitMS: 1200,
		},
		SessionUsageCumulative: true,
		CacheCreationReported:  true,
	}, nil
}

// TestPerfRecording_ResumedSessionRecordsPerRoundDeltas proves a resumed
// session's cumulative token counters are stored per round as correct deltas,
// with fresh input, reasoning, model identity, activity metrics, workload, and
// finding counts all populated, and cache creation left unknown.
func TestPerfRecording_ResumedSessionRecordsPerRoundDeltas(t *testing.T) {
	database, _, run, _ := setupTest(t)

	roundNum := 0
	base := &cumulativeSessionAgent{}
	wrapped := &perfRecordingAgent{
		inner:    base,
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return roundNum },
	}
	sessions := NewRunSessions(database, run.ID, wrapped, true)

	for r := 1; r <= 2; r++ {
		roundNum = r
		opts := agent.RunOpts{
			Purpose:  "review",
			Workload: &agent.InvocationWorkload{Files: 4, Lines: 120},
		}
		if _, err := sessions.Run(context.Background(), wrapped, SessionRoleReviewer, opts, nil); err != nil {
			t.Fatalf("round %d: %v", r, err)
		}
	}

	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 2 {
		t.Fatalf("got %d rows, want 2", len(invs))
	}
	r1, r2 := invs[0], invs[1]

	if r1.SessionMode != db.InvocationModeStarted || r2.SessionMode != db.InvocationModeResumed {
		t.Fatalf("session modes = %q/%q", r1.SessionMode, r2.SessionMode)
	}
	if r1.SessionKey == "" || r1.SessionKey != r2.SessionKey {
		t.Fatalf("session keys must match across rounds: %q/%q", r1.SessionKey, r2.SessionKey)
	}

	// Raw counters are cumulative.
	if r1.InputTokens != 1000 || r2.InputTokens != 2500 {
		t.Fatalf("raw input = %d/%d, want 1000/2500", r1.InputTokens, r2.InputTokens)
	}
	// Deltas are the per-round additions.
	assertPtr(t, "r1 delta input", r1.DeltaInputTokens, 1000)
	assertPtr(t, "r2 delta input", r2.DeltaInputTokens, 1500)
	assertPtr(t, "r2 delta output", r2.DeltaOutputTokens, 150)
	assertPtr(t, "r2 delta cache", r2.DeltaCacheReadTokens, 1200)
	// Codex fresh input excludes both cache reads and cache writes.
	assertPtr(t, "r1 fresh", r1.FreshInputTokens, 380)
	assertPtr(t, "r2 fresh", r2.FreshInputTokens, 630)
	// Reasoning + activity metrics.
	assertPtr(t, "r2 reasoning", r2.ReasoningTokens, 10)
	assertPtr(t, "r2 roundtrips", r2.ModelRoundtrips, 4)
	assertPtr(t, "r2 tool calls", r2.ToolCalls, 3)
	assertPtr(t, "r2 test/lint", r2.ToolTestLintCalls, 1)
	assertPtr64(t, "r2 subprocess wait", r2.SubprocessWaitMS, 1200)
	// Workload + findings.
	assertPtr(t, "r2 workload files", r2.WorkloadFiles, 4)
	assertPtr(t, "r2 workload lines", r2.WorkloadLines, 120)
	assertPtr(t, "r2 finding count", r2.FindingCount, 2)
	// Model identity.
	if r2.Model != "gpt-5.6-sol" || r2.ModelProvider == nil || *r2.ModelProvider != "openai" {
		t.Fatalf("model/provider = %q/%v", r2.Model, r2.ModelProvider)
	}
	assertPtr(t, "r2 raw cache write", r2.CacheCreationTokens, 70)
	assertPtr(t, "r2 delta cache write", r2.DeltaCacheCreationTokens, 50)
	if r2.ReportedCostUSD == nil || *r2.ReportedCostUSD != 2.5 {
		t.Fatalf("r2 reported cost = %v, want 2.5", r2.ReportedCostUSD)
	}
}

type configuredModelFailureAgent struct{}

func (*configuredModelFailureAgent) Name() string { return "codex" }
func (*configuredModelFailureAgent) Close() error { return nil }
func (*configuredModelFailureAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return nil, errors.New("codex start: unavailable")
}
func (*configuredModelFailureAgent) ConfiguredModel() agent.ModelIdentity {
	return agent.ModelIdentity{Name: "gpt-5.6-sol", Vendor: "openai"}
}

func TestPerfRecording_FailedInvocationRetainsConfiguredModelIdentity(t *testing.T) {
	database, _, run, _ := setupTest(t)
	wrapped := &perfRecordingAgent{
		inner:    &configuredModelFailureAgent{},
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return 1 },
		reviewCandidatePool: []db.ReviewCandidateReceipt{
			{Agent: "codex", Model: "gpt-5.6-sol", Vendor: "openai"},
		},
	}

	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Purpose: "review"}); err == nil {
		t.Fatal("Run() succeeded, want configured-model failure")
	}
	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 {
		t.Fatalf("invocations = %d, want 1", len(invs))
	}
	if invs[0].Model != "gpt-5.6-sol" || invs[0].ModelProvider == nil || *invs[0].ModelProvider != "openai" {
		t.Fatalf("failed invocation model/provider = %q/%v", invs[0].Model, invs[0].ModelProvider)
	}
	if len(invs[0].ReviewCandidatePool) != 1 || invs[0].ReviewCandidatePool[0].Agent != "codex" {
		t.Fatalf("failed invocation review pool = %#v", invs[0].ReviewCandidatePool)
	}
}

type receiptObservingAgent struct {
	database *db.DB
	runID    string
	calls    int
}

func (a *receiptObservingAgent) Name() string { return "codex" }
func (a *receiptObservingAgent) Close() error { return nil }
func (a *receiptObservingAgent) ConfiguredModel() agent.ModelIdentity {
	return agent.ModelIdentity{Name: "gpt-5.6-sol", Vendor: "openai"}
}
func (a *receiptObservingAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.calls++
	invocations, err := a.database.GetAgentInvocationsByRun(a.runID)
	if err != nil {
		return nil, err
	}
	if len(invocations) != 1 {
		return nil, fmt.Errorf("pre-launch receipts = %d, want 1", len(invocations))
	}
	receipt := invocations[0]
	if receipt.ExitStatus != "started" || receipt.Agent != "codex" || len(receipt.ReviewCandidatePool) != 1 {
		return nil, fmt.Errorf("pre-launch receipt = %+v", receipt)
	}
	result := &agent.Result{Model: "gpt-5.6-sol", ModelProvider: "openai"}
	if opts.OnAttempt != nil {
		now := time.Now()
		opts.OnAttempt(agent.Attempt{Agent: a.Name(), Result: result, StartedAt: now, CompletedAt: now})
	}
	return result, nil
}

func TestPerfRecording_ReviewSelectionReceiptExistsBeforeLaunchAndIsFinalized(t *testing.T) {
	database, _, run, _ := setupTest(t)
	inner := &receiptObservingAgent{database: database, runID: run.ID}
	wrapped := &perfRecordingAgent{
		inner:    inner,
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return 1 },
		reviewCandidatePool: []db.ReviewCandidateReceipt{
			{Agent: "codex", Model: "gpt-5.6-sol", Vendor: "openai"},
		},
	}

	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Purpose: "review"}); err != nil {
		t.Fatal(err)
	}
	invocations, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 || len(invocations) != 1 || invocations[0].ExitStatus != "ok" {
		t.Fatalf("calls/receipts = %d/%+v, want one finalized receipt", inner.calls, invocations)
	}
}

func TestPerfRecording_ReviewReceiptFailurePreventsHarnessLaunch(t *testing.T) {
	database, _, run, _ := setupTest(t)
	inner := &routedTestAgent{name: "codex", model: agent.ModelIdentity{Name: "gpt-5.6-sol", Vendor: "openai"}}
	wrapped := &perfRecordingAgent{
		inner:    inner,
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return 1 },
		reviewCandidatePool: []db.ReviewCandidateReceipt{
			{Agent: "codex", Model: "unsafe\nmodel", Vendor: "openai"},
		},
	}

	_, err := wrapped.Run(context.Background(), agent.RunOpts{Purpose: "review"})
	if err == nil || !strings.Contains(err.Error(), "persist review selection receipt") {
		t.Fatalf("Run() error = %v, want mandatory receipt failure", err)
	}
	if inner.calls != 0 {
		t.Fatalf("harness calls = %d, want zero", inner.calls)
	}
}

// resumeFailingAgent starts a session cold, then fails any resume with an
// exit-shaped error, then succeeds on the fresh fallback session.
type resumeFailingAgent struct{ calls int }

func (a *resumeFailingAgent) Name() string                { return "codex" }
func (a *resumeFailingAgent) Close() error                { return nil }
func (a *resumeFailingAgent) SupportsSessionResume() bool { return true }

func (a *resumeFailingAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.calls++
	if opts.Session != nil && opts.Session.ID != "" && !opts.SessionFallback {
		return nil, errors.New("codex exited: status 1")
	}
	return &agent.Result{Output: json.RawMessage(`{}`), SessionID: "sess-xyz"}, nil
}

// TestPerfRecording_FallbackRecordsReason proves a failed resume records both
// the failed resume row and a fallback row carrying a classified reason.
func TestPerfRecording_FallbackRecordsReason(t *testing.T) {
	database, _, run, _ := setupTest(t)

	roundNum := 0
	wrapped := &perfRecordingAgent{
		inner:    &resumeFailingAgent{},
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return roundNum },
	}
	sessions := NewRunSessions(database, run.ID, wrapped, true)

	for r := 1; r <= 2; r++ {
		roundNum = r
		if _, err := sessions.Run(context.Background(), wrapped, SessionRoleFixer, agent.RunOpts{Purpose: "review-fix"}, nil); err != nil {
			t.Fatalf("round %d: %v", r, err)
		}
	}

	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var fallback, failedResume *db.AgentInvocation
	for i := range invs {
		switch invs[i].SessionMode {
		case db.InvocationModeFallback:
			fallback = &invs[i]
		case db.InvocationModeResumed:
			if invs[i].ExitStatus == "error" {
				failedResume = &invs[i]
			}
		}
	}
	if failedResume == nil {
		t.Fatal("expected a failed resumed invocation row")
	}
	if fallback == nil {
		t.Fatal("expected a fallback invocation row")
	}
	if fallback.FallbackReason == nil || *fallback.FallbackReason != db.FallbackReasonExit {
		t.Fatalf("fallback reason = %v, want %q", fallback.FallbackReason, db.FallbackReasonExit)
	}
}

// TestPerfRecording_MissingProviderUsageIsUnknown proves an adapter that reports
// no usage or activity metrics records unknown (NULL) fields, not zeros.
func TestPerfRecording_MissingProviderUsageIsUnknown(t *testing.T) {
	database, _, run, _ := setupTest(t)

	wrapped := &perfRecordingAgent{
		inner:    &noUsageAgent{},
		db:       database,
		runID:    run.ID,
		stepName: types.StepTest,
		round:    func() int { return 1 },
	}
	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Purpose: "test-evidence"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 {
		t.Fatalf("got %d rows, want 1", len(invs))
	}
	inv := invs[0]
	for name, p := range map[string]*int{
		"model_roundtrips":  inv.ModelRoundtrips,
		"tool_calls":        inv.ToolCalls,
		"cache_creation":    inv.CacheCreationTokens,
		"fresh_input":       inv.FreshInputTokens,
		"delta_input":       inv.DeltaInputTokens,
		"delta_output":      inv.DeltaOutputTokens,
		"delta_cache_read":  inv.DeltaCacheReadTokens,
		"delta_cache_write": inv.DeltaCacheCreationTokens,
		"reasoning":         inv.ReasoningTokens,
		"finding_count":     inv.FindingCount,
		"workload_files":    inv.WorkloadFiles,
	} {
		if p != nil {
			t.Fatalf("%s must be unknown (nil) for a no-usage invocation, got %d", name, *p)
		}
	}
	if inv.SubprocessWaitMS != nil {
		t.Fatalf("subprocess wait must be unknown, got %d", *inv.SubprocessWaitMS)
	}
	if inv.ReportedCostUSD != nil {
		t.Fatalf("reported cost must be unknown, got %f", *inv.ReportedCostUSD)
	}
}

func TestPerfRecording_PreservesMissingCacheMeters(t *testing.T) {
	database, _, run, _ := setupTest(t)
	wrapped := &perfRecordingAgent{
		inner: &partialUsageAgent{}, db: database, runID: run.ID,
		stepName: types.StepTest, round: func() int { return 1 },
	}
	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Purpose: "test-evidence"}); err != nil {
		t.Fatal(err)
	}
	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 {
		t.Fatalf("invocations = %d, want 1", len(invs))
	}
	inv := invs[0]
	assertPtr(t, "input", inv.DeltaInputTokens, 100)
	assertPtr(t, "output", inv.DeltaOutputTokens, 20)
	if inv.DeltaCacheReadTokens != nil || inv.DeltaCacheCreationTokens != nil || inv.FreshInputTokens != nil {
		t.Fatalf("missing cache meters became known: %+v", inv)
	}
}

func TestPerfRecording_ResumedSessionDoesNotInferNewlyAppearingCacheMeters(t *testing.T) {
	database, _, run, _ := setupTest(t)
	round := 0
	wrapped := &perfRecordingAgent{
		inner: &partialCumulativeAgent{}, db: database, runID: run.ID,
		stepName: types.StepReview, round: func() int { return round },
	}
	sessions := NewRunSessions(database, run.ID, wrapped, true)
	for round = 1; round <= 2; round++ {
		if _, err := sessions.Run(context.Background(), wrapped, SessionRoleReviewer, agent.RunOpts{Purpose: "review"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 2 {
		t.Fatalf("invocations = %d, want 2", len(invs))
	}
	assertPtr(t, "round 2 input", invs[1].DeltaInputTokens, 100)
	assertPtr(t, "round 2 output", invs[1].DeltaOutputTokens, 20)
	if invs[1].DeltaCacheReadTokens != nil || invs[1].DeltaCacheCreationTokens != nil {
		t.Fatalf("newly appearing cumulative cache meters must be unknown: %+v", invs[1])
	}
}

func TestPerfRecording_ResumedSessionDoesNotSkipUnknownUsageRound(t *testing.T) {
	database, _, run, _ := setupTest(t)
	round := 0
	wrapped := &perfRecordingAgent{
		inner: &intermittentCumulativeUsageAgent{}, db: database, runID: run.ID,
		stepName: types.StepReview, round: func() int { return round },
	}
	sessions := NewRunSessions(database, run.ID, wrapped, true)
	for round = 1; round <= 3; round++ {
		if _, err := sessions.Run(context.Background(), wrapped, SessionRoleReviewer, agent.RunOpts{Purpose: "review"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 3 {
		t.Fatalf("invocations = %d, want 3", len(invs))
	}
	if invs[1].DeltaInputTokens != nil || invs[1].DeltaOutputTokens != nil {
		t.Fatalf("round 2 unknown usage became known: %+v", invs[1])
	}
	if invs[2].DeltaInputTokens != nil || invs[2].DeltaOutputTokens != nil {
		t.Fatalf("round 3 absorbed usage from the unknown round: %+v", invs[2])
	}
}

type partialUsageAgent struct{}

func (*partialUsageAgent) Name() string { return "codex" }
func (*partialUsageAgent) Close() error { return nil }
func (*partialUsageAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{
		UsageReported: true,
		Usage: agent.TokenUsage{
			InputTokens: 100, OutputTokens: 20, Reported: true,
			MeterPresenceReported: true, InputReported: true, OutputReported: true,
		},
	}, nil
}

type partialCumulativeAgent struct{ calls int }

func (a *partialCumulativeAgent) Name() string                { return "codex" }
func (a *partialCumulativeAgent) Close() error                { return nil }
func (a *partialCumulativeAgent) SupportsSessionResume() bool { return true }
func (a *partialCumulativeAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.calls++
	usage := agent.TokenUsage{
		InputTokens: 100 * a.calls, OutputTokens: 20 * a.calls, Reported: true,
		MeterPresenceReported: true, InputReported: true, OutputReported: true,
	}
	if a.calls == 2 {
		usage.CacheReadTokens = 50
		usage.CacheCreationTokens = 10
		usage.CacheReadReported = true
		usage.CacheCreationReported = true
	}
	return &agent.Result{
		SessionID: "partial-session", Resumed: opts.Session != nil,
		Usage: usage, UsageReported: true, CacheCreationReported: usage.CacheCreationReported,
		SessionUsageCumulative: true,
	}, nil
}

type intermittentCumulativeUsageAgent struct{ calls int }

func (a *intermittentCumulativeUsageAgent) Name() string                { return "codex" }
func (a *intermittentCumulativeUsageAgent) Close() error                { return nil }
func (a *intermittentCumulativeUsageAgent) SupportsSessionResume() bool { return true }
func (a *intermittentCumulativeUsageAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.calls++
	result := &agent.Result{
		SessionID: "intermittent-session", Resumed: opts.Session != nil,
		SessionUsageCumulative: true,
	}
	if a.calls == 2 {
		return result, nil
	}
	result.Usage = agent.TokenUsage{
		InputTokens: 100 * a.calls, OutputTokens: 20 * a.calls, Reported: true,
		MeterPresenceReported: true, InputReported: true, OutputReported: true,
	}
	result.UsageReported = true
	return result, nil
}

type noUsageAgent struct{}

func (noUsageAgent) Name() string { return "noop-agent" }
func (noUsageAgent) Close() error { return nil }
func (noUsageAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{}, nil
}

func assertPtr(t *testing.T, name string, got *int, want int) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %d", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %d, want %d", name, *got, want)
	}
}

func assertPtr64(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %d", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %d, want %d", name, *got, want)
	}
}
