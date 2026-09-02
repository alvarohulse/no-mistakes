package stats

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunAuditCanonicalJSONHasStableShape(t *testing.T) {
	zero := int64(0)
	audit := RunAudit{
		SchemaVersion: SchemaVersion,
		Run: RunIdentity{
			ID: "run-1", RepoID: "repo-1", Branch: "feature", HeadSHA: "abc", BaseSHA: "def",
			RefreshStrategy: types.RefreshStrategyMerge, Status: types.RunCompleted,
			CreatedAt: 10, UpdatedAt: 20, ParkedMS: &zero, RichDataRetained: true,
			ConfigSources: []ConfigDigest{},
		},
		Steps:           []Step{},
		SkipReceipts:    []SkipReceipt{},
		Invocations:     []Invocation{},
		Metrics:         emptyMetrics(),
		IntegrityErrors: []string{},
	}
	got, err := audit.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":8,"run":{"id":"run-1","repo_id":"repo-1","branch":"feature","head_sha":"abc","base_sha":"def","refresh_strategy":"merge","status":"completed","created_at":10,"updated_at":20,"parked_ms":0,"pinned_at":null,"rich_data_retained":true,"no_mistakes_version":null,"no_mistakes_build_sha":null,"policy_digest":null,"config_sources":[]},"steps":[],"skip_receipts":[],"invocations":[],"metrics":{"invocation_count":0,"delta_input_tokens":{"value":null,"coverage":{"reported":0,"total":0},"integrity_error":null},"delta_output_tokens":{"value":null,"coverage":{"reported":0,"total":0},"integrity_error":null},"delta_cache_read_tokens":{"value":null,"coverage":{"reported":0,"total":0},"integrity_error":null},"delta_cache_write_tokens":{"value":null,"coverage":{"reported":0,"total":0},"integrity_error":null},"reported_cost_usd":{"value":null,"coverage":{"reported":0,"total":0},"integrity_error":null}},"integrity_errors":[]}`
	if got != want {
		t.Fatalf("canonical JSON mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildRunAuditPreservesZeroAndRejectsPartialTotals(t *testing.T) {
	database, run := newAuditRun(t)
	zeroInt := 0
	zeroCost := 0.0
	five := 5
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex",
		UsageCoverage: agent.UsageCoverageComplete,
		SessionMode:   db.InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 100, ExitStatus: "ok",
		DeltaInputTokens: &zeroInt, DeltaOutputTokens: &zeroInt, DeltaCacheReadTokens: &zeroInt,
		DeltaCacheCreationTokens: &zeroInt, ReportedCostUSD: &zeroCost,
	})
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex",
		SessionMode: db.InvocationModeResumed, StartedAt: 3, CompletedAt: 4, DurationMS: 100, ExitStatus: "ok",
		DeltaInputTokens: &five,
	})

	audit, err := BuildRunAudit(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Run.ParkedMS == nil || *audit.Run.ParkedMS != 0 {
		t.Fatalf("parked_ms = %v, want known zero", audit.Run.ParkedMS)
	}
	if audit.Metrics.DeltaInputTokens.Value == nil || *audit.Metrics.DeltaInputTokens.Value != 5 {
		t.Fatalf("input total = %+v", audit.Metrics.DeltaInputTokens)
	}
	if audit.Metrics.DeltaOutputTokens.Value != nil || audit.Metrics.DeltaOutputTokens.Coverage != (Coverage{Reported: 1, Total: 2}) || audit.Metrics.DeltaOutputTokens.IntegrityError == nil {
		t.Fatalf("partial output metric = %+v", audit.Metrics.DeltaOutputTokens)
	}
	if audit.Invocations[0].DeltaUsage.OutputTokens == nil || *audit.Invocations[0].DeltaUsage.OutputTokens != 0 {
		t.Fatalf("reported zero became unknown: %+v", audit.Invocations[0].DeltaUsage)
	}
	if audit.Invocations[0].UsageCoverage != agent.UsageCoverageComplete || audit.Invocations[1].UsageCoverage != agent.UsageCoverageUnknown {
		t.Fatalf("usage coverage = %q/%q, want complete/unknown", audit.Invocations[0].UsageCoverage, audit.Invocations[1].UsageCoverage)
	}
	if audit.Invocations[1].DeltaUsage.OutputTokens != nil {
		t.Fatalf("missing output became zero: %+v", audit.Invocations[1].DeltaUsage)
	}
	raw, err := audit.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"invocation_mode", "nested_agents_reported", "nested_agent_count", "nested_agents"} {
		if strings.Contains(raw, `"`+removed+`"`) {
			t.Fatalf("run audit retained removed field %q: %s", removed, raw)
		}
	}
}

func TestBuildRunAuditKeepsContentFreeCommandAndRepairReceipts(t *testing.T) {
	database, run := newAuditRun(t)
	step, err := database.InsertStepResult(run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	round, err := database.InsertStepRound(step.ID, 1, "auto_fix", nil, nil, 25)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepRoundRepairAudit(round.ID, "sha256:repair-fingerprint", "resolved"); err != nil {
		t.Fatal(err)
	}
	zero := 0
	version := "5.2.26"
	const privateCommand = "/private/worktree/DO-NOT-RETAIN --token secret"
	if err := database.SetStepEvidence(step.ID, db.StepEvidence{
		Commands: []db.CommandEvidence{
			{
				Round: 1, Sequence: 1, Command: privateCommand, Outcome: db.CommandOutcomePassed, ExitCode: &zero,
				CommandSource: runner.SourceLinux,
				Runner: &runner.Provenance{
					SchemaVersion: runner.SchemaVersion, Platform: "linux", Source: runner.SourceLinux,
					Executable: "zsh", Args: []string{"-lc"}, Version: &version,
				},
			},
			{Round: 1, Sequence: 2, Command: privateCommand, Outcome: db.CommandOutcomeError, CommandSource: runner.SourceBase},
		},
		Evidence: []string{"private prose must not enter stats"}, Explanation: "private explanation must not enter stats",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 25, ""); err != nil {
		t.Fatal(err)
	}

	audit, err := BuildRunAudit(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Steps) != 1 || len(audit.Steps[0].Commands) != 2 {
		t.Fatalf("command receipts = %+v", audit.Steps)
	}
	first, second := audit.Steps[0].Commands[0], audit.Steps[0].Commands[1]
	if first.Round != 1 || first.Sequence != 1 || first.Outcome != db.CommandOutcomePassed || first.ExitCode == nil || *first.ExitCode != 0 || first.CommandSource != runner.SourceLinux {
		t.Fatalf("first command receipt = %+v", first)
	}
	if first.Runner == nil || first.Runner.Executable != "zsh" || first.Runner.Version == nil || *first.Runner.Version != version || len(first.Runner.Args) != 1 || first.Runner.Args[0] != "-lc" {
		t.Fatalf("runner provenance = %+v", first.Runner)
	}
	if second.Runner != nil || second.Outcome != db.CommandOutcomeError {
		t.Fatalf("runner-less command receipt = %+v", second)
	}
	if len(audit.Steps[0].Rounds) != 1 || audit.Steps[0].Rounds[0].RepairFailureFingerprint == nil || *audit.Steps[0].Rounds[0].RepairFailureFingerprint != "sha256:repair-fingerprint" || audit.Steps[0].Rounds[0].RepairResult == nil || *audit.Steps[0].Rounds[0].RepairResult != "resolved" {
		t.Fatalf("repair receipt = %+v", audit.Steps[0].Rounds)
	}
	encoded := mustJSON(t, audit)
	for _, private := range []string{privateCommand, "private prose", "private explanation"} {
		if strings.Contains(encoded, private) {
			t.Fatalf("run audit retained private content %q: %s", private, encoded)
		}
	}
}

func TestBuildRunAuditAggregatesResumedSessionDeltasNotRawCounters(t *testing.T) {
	database, run := newAuditRun(t)
	firstDelta, secondDelta := 1000, 1500
	firstRaw, secondRaw := 1000, 2500
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex",
		SessionMode: db.InvocationModeStarted, SessionKey: "session", StartedAt: 1, CompletedAt: 2, ExitStatus: "ok",
		InputTokens: firstRaw, DeltaInputTokens: &firstDelta,
	})
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex",
		SessionMode: db.InvocationModeResumed, SessionKey: "session", StartedAt: 3, CompletedAt: 4, ExitStatus: "ok",
		InputTokens: secondRaw, DeltaInputTokens: &secondDelta,
	})

	audit, err := BuildRunAudit(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Metrics.DeltaInputTokens.Value == nil || *audit.Metrics.DeltaInputTokens.Value != 2500 {
		t.Fatalf("delta input total = %+v, want 2500", audit.Metrics.DeltaInputTokens)
	}
	if audit.Invocations[1].RawUsage.InputTokens == nil || *audit.Invocations[1].RawUsage.InputTokens != 2500 {
		t.Fatalf("raw cumulative receipt = %+v", audit.Invocations[1].RawUsage)
	}
}

func TestBuildRunAuditReportsOnlyCLIReportedCost(t *testing.T) {
	database, run := newAuditRun(t)
	input, output, cacheRead, cacheWrite := 1_000_000, 1_000_000, 1_000_000, 1_000_000
	reported := 9.25
	provider := "anthropic"
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "cursor",
		Model: "claude-opus-5", ModelProvider: &provider,
		SessionMode: db.InvocationModeCold, StartedAt: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC).Unix(),
		CompletedAt: time.Date(2026, 8, 17, 0, 1, 0, 0, time.UTC).Unix(), ExitStatus: "ok",
		DeltaInputTokens: &input, DeltaOutputTokens: &output, DeltaCacheReadTokens: &cacheRead,
		DeltaCacheCreationTokens: &cacheWrite, ReportedCostUSD: &reported,
	})

	audit, err := BuildRunAudit(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Invocations) != 1 {
		t.Fatalf("invocations = %d, want 1", len(audit.Invocations))
	}
	if got := audit.Invocations[0].ReportedCostUSD; got == nil || *got != reported {
		t.Fatalf("reported cost = %v, want %v", got, reported)
	}
	raw, err := audit.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"costs", "historical_costs", "api_list_estimate", "harness_adjusted_estimate"} {
		if strings.Contains(raw, `"`+removed+`"`) {
			t.Fatalf("run audit retained removed field %q: %s", removed, raw)
		}
	}
}

func TestBuildRunAuditOrdersRecordsAndCarriesSkipAndReviewReceipts(t *testing.T) {
	database, run := newAuditRun(t)
	sources := []db.ConfigSource{{Kind: db.ConfigSourceGlobal, Digest: strings.Repeat("a", 64), Path: "/private/config"}, {Kind: db.ConfigSourceGlobalOverride, Digest: strings.Repeat("b", 64), Ref: "owner/repo", Path: "/private/config"}}
	if err := database.UpdateRunConfigSources(run.ID, sources); err != nil {
		t.Fatal(err)
	}
	policy := `{"version":5,"managed":true,"steps":[{"name":"review","status":"enabled"},{"name":"ci","status":"skipped","skip_source":"global-override"}],"routing":{"review_candidates":[{"agent":"codex","model":{"name":"gpt-5.6-sol","vendor":"openai"}}]}}`
	digest := sha256.Sum256([]byte(policy))
	if err := database.UpdateRunResolvedPolicy(run.ID, policy, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepAsSkipped(ci.ID, types.SkipSourceGlobalOverride); err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(review.ID, 2, "auto_fix", nil, nil, 20); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(review.ID, 1, "initial", nil, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(review.ID, types.StepStatusCompleted, 0, 30, ""); err != nil {
		t.Fatal(err)
	}
	provider := "openai"
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex", Model: "gpt-5.6-sol", ModelProvider: &provider,
		ReviewCandidatePool: []db.ReviewCandidateReceipt{{Agent: "codex", Model: "gpt-5.6-sol", Vendor: "openai"}},
		SessionMode:         db.InvocationModeCold, StartedAt: 20, CompletedAt: 21, ExitStatus: "ok",
	})
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review-fix", Agent: "cursor",
		SessionMode: db.InvocationModeCold, StartedAt: 10, CompletedAt: 11, ExitStatus: "ok",
	})

	audit, err := BuildRunAudit(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Steps) != 2 || audit.Steps[0].Name != types.StepReview || audit.Steps[1].Name != types.StepCI {
		t.Fatalf("step order = %+v", audit.Steps)
	}
	if len(audit.Steps[0].Rounds) != 2 || audit.Steps[0].Rounds[0].Number != 1 || audit.Steps[0].Rounds[1].Number != 2 {
		t.Fatalf("round order = %+v", audit.Steps[0].Rounds)
	}
	if len(audit.SkipReceipts) != 1 || audit.SkipReceipts[0] != (SkipReceipt{Step: types.StepCI, Source: types.SkipSourceGlobalOverride}) {
		t.Fatalf("skip receipts = %+v", audit.SkipReceipts)
	}
	if len(audit.Invocations) != 2 || audit.Invocations[0].Round != 1 || audit.Invocations[1].Review == nil || audit.Invocations[1].Review.Selected.Agent != "codex" {
		t.Fatalf("invocation receipts = %+v", audit.Invocations)
	}
	if got := audit.Run.ConfigSources; len(got) != 2 || got[1].Kind != db.ConfigSourceGlobalOverride || got[1].Digest != strings.Repeat("b", 64) {
		t.Fatalf("config digests = %+v", got)
	}
	encoded := mustJSON(t, audit)
	if strings.Contains(encoded, "/private/config") || strings.Contains(encoded, "owner/repo") {
		t.Fatal("audit leaked private config source metadata")
	}
}

func TestBuildRunAuditNormalizesLegacyPolicySkipSource(t *testing.T) {
	database, run := newAuditRun(t)
	policy := `{"version":4,"steps":[{"name":"ci","status":"skipped"}]}`
	digest := sha256.Sum256([]byte(policy))
	if err := database.UpdateRunResolvedPolicy(run.ID, policy, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(ci.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
		t.Fatal(err)
	}

	audit, err := BuildRunAudit(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.SkipReceipts) != 1 || audit.SkipReceipts[0].Source != types.SkipSourceRunRequest {
		t.Fatalf("legacy skip receipts = %+v", audit.SkipReceipts)
	}
	if audit.Steps[0].SkipSource == nil || *audit.Steps[0].SkipSource != types.SkipSourceRunRequest {
		t.Fatalf("legacy step skip source = %+v", audit.Steps[0])
	}
}

func TestBuildRunAuditReportsPolicyAndStoredStepMismatch(t *testing.T) {
	database, run := newAuditRun(t)
	policy := `{"version":5,"steps":[{"name":"ci","status":"skipped","skip_source":"global-override"}]}`
	digest := sha256.Sum256([]byte(policy))
	if err := database.UpdateRunResolvedPolicy(run.ID, policy, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(ci.ID, types.StepStatusCompleted, 0, 0, ""); err != nil {
		t.Fatal(err)
	}

	audit, err := BuildRunAudit(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Steps[0].SkipSource != nil {
		t.Fatalf("completed step gained skip source: %+v", audit.Steps[0])
	}
	if len(audit.SkipReceipts) != 1 || audit.SkipReceipts[0].Source != types.SkipSourceGlobalOverride {
		t.Fatalf("policy skip receipt = %+v", audit.SkipReceipts)
	}
	if !strings.Contains(strings.Join(audit.IntegrityErrors, "\n"), "skipped in resolved policy but has status completed") {
		t.Fatalf("integrity errors = %v", audit.IntegrityErrors)
	}
}

func TestBuildRunAuditAcceptsSourceLessRuntimeSkip(t *testing.T) {
	database, run := newAuditRun(t)
	policy := `{"version":5,"steps":[{"name":"ci","status":"enabled"}]}`
	digest := sha256.Sum256([]byte(policy))
	if err := database.UpdateRunResolvedPolicy(run.ID, policy, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(ci.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
		t.Fatal(err)
	}

	audit, err := BuildRunAudit(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.IntegrityErrors) != 0 {
		t.Fatalf("runtime skip produced integrity errors: %v", audit.IntegrityErrors)
	}
	if len(audit.SkipReceipts) != 0 || audit.Steps[0].SkipSource != nil {
		t.Fatalf("runtime skip produced a planned receipt: %+v", audit)
	}
}

func TestBuildRunAuditReportsReviewSelectionOutsideCandidatePool(t *testing.T) {
	database, run := newAuditRun(t)
	provider := "openai"
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "cursor", Model: "gpt-5.6-luna", ModelProvider: &provider,
		ReviewCandidatePool: []db.ReviewCandidateReceipt{{Agent: "codex", Model: "gpt-5.6-sol", Vendor: "openai"}},
		SessionMode:         db.InvocationModeCold, StartedAt: 1, CompletedAt: 2, ExitStatus: "ok",
	})

	audit, err := BuildRunAudit(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(audit.IntegrityErrors, "\n"), "selected route is absent from its candidate pool") {
		t.Fatalf("integrity errors = %v", audit.IntegrityErrors)
	}
}

func TestBuildRunAuditReportsMissingCompletedPolicyStep(t *testing.T) {
	database, run := newAuditRun(t)
	policy := `{"version":5,"steps":[{"name":"review","status":"enabled"},{"name":"ci","status":"skipped","skip_source":"global-override"}]}`
	digest := sha256.Sum256([]byte(policy))
	if err := database.UpdateRunResolvedPolicy(run.ID, policy, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(review.ID, types.StepStatusCompleted, 0, 0, ""); err != nil {
		t.Fatal(err)
	}

	audit, err := BuildRunAudit(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.SkipReceipts) != 1 || audit.SkipReceipts[0].Step != types.StepCI {
		t.Fatalf("policy skip receipts = %+v", audit.SkipReceipts)
	}
	if !strings.Contains(strings.Join(audit.IntegrityErrors, "\n"), "missing stored result for policy step ci") {
		t.Fatalf("integrity errors = %v", audit.IntegrityErrors)
	}
}

func TestBuildRunAuditRequiresManagedFullReviewReceipt(t *testing.T) {
	database, run := newAuditRun(t)
	policy := `{"version":5,"managed":true,"steps":[{"name":"review","status":"enabled"}],"routing":{"review_candidates":[{"agent":"codex"}]}}`
	digest := sha256.Sum256([]byte(policy))
	if err := database.UpdateRunResolvedPolicy(run.ID, policy, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(review.ID, types.StepStatusCompleted, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex", Model: "gpt-5.6-sol",
		SessionMode: db.InvocationModeCold, StartedAt: 1, CompletedAt: 2, ExitStatus: "ok",
	})

	audit, err := BuildRunAudit(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(audit.IntegrityErrors, "\n"), "has no candidate-pool receipt") {
		t.Fatalf("integrity errors = %v", audit.IntegrityErrors)
	}
}

func TestBuildRunAuditReconcilesManagedReviewPoolWithPolicy(t *testing.T) {
	database, run := newAuditRun(t)
	policy := `{"version":5,"managed":true,"steps":[{"name":"review","status":"enabled"}],"routing":{"review_candidates":[{"agent":"claude","model":{"name":"claude-opus-5","vendor":"anthropic"}},{"agent":"codex","model":{"name":"gpt-5.6-sol","vendor":"openai"}}]}}`
	digest := sha256.Sum256([]byte(policy))
	if err := database.UpdateRunResolvedPolicy(run.ID, policy, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(review.ID, types.StepStatusCompleted, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	provider := "openai"
	seedInvocation(t, database, db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex", Model: "gpt-5.6-sol", ModelProvider: &provider,
		ReviewCandidatePool: []db.ReviewCandidateReceipt{{Agent: "codex", Model: "gpt-5.6-sol", Vendor: "openai"}},
		SessionMode:         db.InvocationModeCold, StartedAt: 1, CompletedAt: 2, ExitStatus: "ok",
	})

	audit, err := BuildRunAudit(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(audit.IntegrityErrors, "\n"), "candidate pool differs from resolved policy") {
		t.Fatalf("integrity errors = %v", audit.IntegrityErrors)
	}
}

func TestBuildMetricsRejectsAggregateMismatch(t *testing.T) {
	one := 1
	two := int64(2)
	metrics, integrityErrors := buildMetrics([]Invocation{{DeltaUsage: TokenMeters{InputTokens: &one}}}, db.AgentInvocationAuditTotals{
		Rows: 1, DeltaInputReported: 1, DeltaInputSum: &two,
	})
	if metrics.DeltaInputTokens.Value != nil || metrics.DeltaInputTokens.IntegrityError == nil {
		t.Fatalf("mismatched input metric = %+v", metrics.DeltaInputTokens)
	}
	if len(integrityErrors) == 0 || !strings.Contains(strings.Join(integrityErrors, "\n"), "aggregate mismatch") {
		t.Fatalf("integrity errors = %v", integrityErrors)
	}
}

func TestBuildMetricsRejectsInvocationRowCountMismatch(t *testing.T) {
	one := 1
	oneSum := int64(1)
	metrics, integrityErrors := buildMetrics([]Invocation{{DeltaUsage: TokenMeters{InputTokens: &one}}}, db.AgentInvocationAuditTotals{
		Rows: 2, DeltaInputReported: 1, DeltaInputSum: &oneSum,
	})
	if metrics.DeltaInputTokens.Value != nil || metrics.DeltaInputTokens.IntegrityError == nil {
		t.Fatalf("mismatched row count input metric = %+v", metrics.DeltaInputTokens)
	}
	if !strings.Contains(strings.Join(integrityErrors, "\n"), "invocation row count mismatch") {
		t.Fatalf("integrity errors = %v", integrityErrors)
	}
}

func newAuditRun(t *testing.T) (*db.DB, *db.Run) {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/audit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepoWithID("repo-1", "/tmp/repo", "https://github.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRunWithOptions(repo.ID, "feature", "abc", "def", db.RunOptions{RefreshStrategy: types.RefreshStrategyMerge})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	return database, run
}

func seedInvocation(t *testing.T, database *db.DB, invocation db.AgentInvocation) {
	t.Helper()
	if _, err := database.InsertAgentInvocation(invocation); err != nil {
		t.Fatal(err)
	}
}

func emptyMetrics() Metrics {
	return Metrics{
		DeltaInputTokens:      IntMetric{Coverage: Coverage{}},
		DeltaOutputTokens:     IntMetric{Coverage: Coverage{}},
		DeltaCacheReadTokens:  IntMetric{Coverage: Coverage{}},
		DeltaCacheWriteTokens: IntMetric{Coverage: Coverage{}},
		ReportedCostUSD:       FloatMetric{Coverage: Coverage{}},
	}
}

func mustJSON(t *testing.T, audit *RunAudit) string {
	t.Helper()
	encoded, err := audit.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
