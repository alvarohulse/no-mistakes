package stats

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPruneRichRunDataRetainsTheRequiredUnionAndArchivesMetrics(t *testing.T) {
	database, err := db.Open(t.TempDir() + "/retention.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepoWithID("repo-1", "/private/repo-path", "https://github.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}

	const privateMarker = "PRIVATE-CONTENT-MUST-NOT-SURVIVE"
	const privateCommandMarker = "PRIVATE-COMMAND-MUST-NOT-SURVIVE"
	const privateProseMarker = "PRIVATE-PROSE-MUST-NOT-SURVIVE"
	terminal := make([]*db.Run, 0, 54)
	for index := 0; index < 53; index++ {
		branch := "feature/retention"
		if index == 0 {
			branch = "/private/path/" + privateMarker
		}
		run, err := database.InsertRun(repo.ID, branch, "head-"+privateMarker, "base-"+privateMarker)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
		terminal = append(terminal, run)
	}
	pinned, err := database.InsertRun(repo.ID, "feature/pinned", "pinned-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(pinned.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetRunPinned(pinned.ID, true); err != nil {
		t.Fatal(err)
	}
	active, err := database.InsertRun(repo.ID, "feature/active", "active-head", "base")
	if err != nil {
		t.Fatal(err)
	}

	oldest := terminal[0]
	if err := database.UpdateRunConfigSources(oldest.ID, []db.ConfigSource{{Kind: db.ConfigSourceGlobal, Digest: strings.Repeat("a", 64), Path: "/private/" + privateMarker}}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(oldest.ID, "https://github.com/owner/repo/pull/1"); err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(oldest.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	initialFindings := `{"findings":[{"id":"finding-1","severity":"error","description":"` + privateMarker + `","action":"auto-fix"}],"summary":"` + privateMarker + `","risk_level":"low","risk_rationale":"test"}`
	if _, err := database.InsertStepRound(review.ID, 1, "initial", &initialFindings, nil, 10); err != nil {
		t.Fatal(err)
	}
	repairRound, err := database.InsertStepRound(review.ID, 2, "auto_fix", nil, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepRoundRepairAudit(repairRound.ID, "sha256:retained-repair-fingerprint", "resolved"); err != nil {
		t.Fatal(err)
	}
	zero := 0
	runnerVersion := "5.2.26"
	if err := database.SetStepEvidence(review.ID, db.StepEvidence{
		Commands: []db.CommandEvidence{
			{
				Round: 2, Sequence: 1, Command: "/private/resolved/path/" + privateCommandMarker, Outcome: db.CommandOutcomePassed, ExitCode: &zero,
				CommandSource: runner.SourceLinux,
				Runner: &runner.Provenance{
					SchemaVersion: runner.SchemaVersion, Platform: "linux", Source: runner.SourceLinux,
					Executable: "zsh", Args: []string{"-lc"}, Version: &runnerVersion,
				},
			},
			{Round: 2, Sequence: 2, Command: privateCommandMarker, Outcome: db.CommandOutcomeError},
		},
		Evidence: []string{privateProseMarker}, Explanation: privateProseMarker,
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(review.ID, types.StepStatusCompleted, 0, 30, ""); err != nil {
		t.Fatal(err)
	}
	provider := "openai"
	fallback := db.FallbackReasonOther
	tokens := 10
	historicalReceipt := `{"api_list_estimate":{"value_usd":1,"coverage":{"reported":2,"eligible":2},"complete":true,"basis":"historical"}}`
	seedInvocation(t, database, db.AgentInvocation{
		RunID: oldest.ID, StepName: string(types.StepReview), Round: 2, Purpose: "review-fix", Agent: "codex",
		Model: "gpt-5.6-sol", ModelProvider: &provider, SessionMode: db.InvocationModeFallback,
		SessionKey: privateMarker, FallbackReason: &fallback, StartedAt: oldest.CreatedAt, CompletedAt: oldest.CreatedAt + 1,
		DurationMS: 1000, ExitStatus: "ok", DeltaInputTokens: &tokens, DeltaOutputTokens: &tokens,
		PricingReceiptJSON:        &historicalReceipt,
		AgentObservationsReported: true,
		AgentObservations:         []types.AgentObservation{{Identity: privateMarker, InvocationMode: types.AgentInvocationModeSubagentTool}},
	})
	seedInvocation(t, database, db.AgentInvocation{
		RunID: oldest.ID, StepName: string(types.StepReview), Round: 1, Purpose: "legacy-raw", Agent: "codex",
		SessionMode: db.InvocationModeStarted, StartedAt: oldest.CreatedAt, CompletedAt: oldest.CreatedAt + 1,
		DurationMS: 500, ExitStatus: "ok", InputTokens: 20, OutputTokens: 5, CacheReadTokens: 3,
	})
	beforeAggregates, err := database.AgentInvocationAggregates()
	if err != nil {
		t.Fatal(err)
	}
	beforeAudit, err := BuildRunAudit(database, oldest.ID)
	if err != nil {
		t.Fatal(err)
	}
	query := Query{
		RunID: oldest.ID, Steps: []types.StepName{types.StepReview}, Agents: []string{"codex"},
		Models: []string{"gpt-5.6-sol"}, Purposes: []string{"review-fix"},
	}
	beforeReport, err := BuildReport(database, query, time.Unix(oldest.CreatedAt+1, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}

	youngNow := time.Unix(oldest.CreatedAt, 0).UTC().Add(13 * 24 * time.Hour)
	if pruned, err := PruneRichRunData(database, youngNow, RichRunRetentionAge, RichRunRetentionFloor, nil); err != nil || pruned != 0 {
		t.Fatalf("prune inside age floor = %d, %v; want 0", pruned, err)
	}

	var removed []string
	oldNow := time.Unix(oldest.CreatedAt, 0).UTC().Add(15 * 24 * time.Hour)
	pruned, err := PruneRichRunData(database, oldNow, RichRunRetentionAge, RichRunRetentionFloor, testRunArtifactCleanup(t, func(runID string) error {
		removed = append(removed, runID)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 3 || len(removed) != 3 {
		t.Fatalf("pruned = %d, removed = %v; want oldest 3 terminal unpinned", pruned, removed)
	}
	for _, run := range terminal[:3] {
		if got, err := database.GetRun(run.ID); err != nil || got != nil {
			t.Fatalf("pruned run %s = %+v, %v", run.ID, got, err)
		}
	}
	for _, run := range append(terminal[3:], pinned, active) {
		if got, err := database.GetRun(run.ID); err != nil || got == nil {
			t.Fatalf("retained run %s = %+v, %v", run.ID, got, err)
		}
	}

	receipt, err := database.GetRunMetricReceipt(oldest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil {
		t.Fatal("pruned run has no long-lived metric receipt")
	}
	if strings.Contains(receipt.PayloadJSON, privateMarker) || strings.Contains(receipt.PayloadJSON, privateCommandMarker) || strings.Contains(receipt.PayloadJSON, privateProseMarker) || strings.Contains(receipt.PayloadJSON, "/private/") {
		t.Fatalf("metric receipt retained private content: %s", receipt.PayloadJSON)
	}

	audit, err := BuildRunAudit(database, oldest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Run.RichDataRetained || audit.Run.Branch != "" || audit.Run.HeadSHA != "" || audit.Invocations[0].SessionKey != "" || len(audit.Invocations[0].NestedAgents) != 0 {
		t.Fatalf("archived audit retained rich identity: %+v", audit)
	}
	if !reflect.DeepEqual(audit.Steps, beforeAudit.Steps) {
		t.Fatalf("content-free step audit changed after pruning:\n before: %+v\n  after: %+v", beforeAudit.Steps, audit.Steps)
	}
	if !beforeAudit.Invocations[0].HistoricalCosts || !audit.Invocations[0].HistoricalCosts {
		t.Fatalf("historical cost label was lost during pruning: before=%t after=%t", beforeAudit.Invocations[0].HistoricalCosts, audit.Invocations[0].HistoricalCosts)
	}
	if got := audit.Steps[0].Commands; len(got) != 2 || got[0].Runner == nil || got[0].Runner.Executable != "zsh" || got[1].Runner != nil {
		t.Fatalf("archived command receipts = %+v", got)
	}
	if got := audit.Steps[0].Rounds[1]; got.RepairFailureFingerprint == nil || *got.RepairFailureFingerprint != "sha256:retained-repair-fingerprint" || got.RepairResult == nil || *got.RepairResult != "resolved" {
		t.Fatalf("archived repair receipt = %+v", got)
	}
	report, err := BuildReport(database, query, oldNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.Runs.Count != 1 || len(report.Agents) != 1 || report.Runs.Items[0].RichDataRetained {
		t.Fatalf("archived filtered report = %+v", report)
	}
	if !reflect.DeepEqual(report.Steps, beforeReport.Steps) || !reflect.DeepEqual(report.Repairs, beforeReport.Repairs) {
		t.Fatalf("content-free report changed after pruning:\n before steps=%+v repairs=%+v\n  after steps=%+v repairs=%+v", beforeReport.Steps, beforeReport.Repairs, report.Steps, report.Repairs)
	}

	stats, err := database.GetStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalRuns != 55 || stats.PullRequests != 1 || stats.RescueRuns != 1 || stats.ReportedFindings != 1 || stats.FixedFindings != 1 {
		t.Fatalf("stats after pruning = %+v", stats)
	}
	aggregates, err := database.AgentInvocationAggregates()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(aggregates, beforeAggregates) {
		t.Fatalf("agent aggregates changed after pruning:\n before: %+v\n  after: %+v", beforeAggregates, aggregates)
	}
}

func TestPruneRichRunDataRetriesCommittedArtifactCleanup(t *testing.T) {
	database, run := newAuditRun(t)
	now := time.Unix(run.CreatedAt, 0).UTC().Add(15 * 24 * time.Hour)
	for index := 0; index < 50; index++ {
		newer, err := database.InsertRun(run.RepoID, "feature/newer", "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateRunStatus(newer.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
	}
	cleanupErr := "artifact cleanup failed"
	pruned, err := PruneRichRunData(database, now, RichRunRetentionAge, RichRunRetentionFloor, testRunArtifactCleanup(t, func(string) error { return &retentionTestError{message: cleanupErr} }))
	if err == nil || !strings.Contains(err.Error(), cleanupErr) || pruned != 1 {
		t.Fatalf("prune cleanup failure = %d, %v", pruned, err)
	}
	if retained, getErr := database.GetRun(run.ID); getErr != nil || retained != nil {
		t.Fatalf("committed archival retained raw run: %+v, %v", retained, getErr)
	}
	if receipt, getErr := database.GetRunMetricReceipt(run.ID); getErr != nil || receipt == nil {
		t.Fatalf("committed archival lost retry owner: %+v, %v", receipt, getErr)
	}

	var retried []string
	pruned, err = PruneRichRunData(database, now, RichRunRetentionAge, RichRunRetentionFloor, testRunArtifactCleanup(t, func(runID string) error {
		retried = append(retried, runID)
		return nil
	}))
	if err != nil || pruned != 0 || !reflect.DeepEqual(retried, []string{run.ID}) {
		t.Fatalf("retry pending cleanup = pruned %d retried %v error %v", pruned, retried, err)
	}
	if _, err := PruneRichRunData(database, now, RichRunRetentionAge, RichRunRetentionFloor, testRunArtifactCleanup(t, func(runID string) error {
		retried = append(retried, runID)
		return nil
	})); err != nil || !reflect.DeepEqual(retried, []string{run.ID}) {
		t.Fatalf("completed cleanup retried again: %v, %v", retried, err)
	}
}

func TestPruneRichRunDataRetriesThePersistedCleanupTargetsAfterTheConfiguredRootChanges(t *testing.T) {
	database, run := newAuditRun(t)
	now := time.Unix(run.CreatedAt, 0).UTC().Add(15 * 24 * time.Hour)
	for index := 0; index < 50; index++ {
		newer, err := database.InsertRun(run.RepoID, "feature/newer", "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateRunStatus(newer.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
	}

	oldRoot := filepath.Join(t.TempDir(), "old-evidence")
	newRoot := filepath.Join(t.TempDir(), "new-evidence")
	oldTarget := filepath.Join(oldRoot, run.ID)
	if err := os.MkdirAll(oldTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	currentRoot := oldRoot
	cleanupFailed := true
	cleanup := &RunArtifactCleanup{}
	cleanup.Targets = func(runID string) []string {
		return []string{filepath.Join(currentRoot, runID)}
	}
	cleanup.Remove = func(_ string, targets []string) error {
		if cleanupFailed {
			cleanupFailed = false
			return &retentionTestError{message: "simulated interrupted cleanup"}
		}
		for _, target := range targets {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
		return nil
	}

	if pruned, err := PruneRichRunData(database, now, RichRunRetentionAge, RichRunRetentionFloor, cleanup); err == nil || pruned != 1 {
		t.Fatalf("initial archival = pruned %d, error %v; want committed archival with cleanup failure", pruned, err)
	}
	currentRoot = newRoot
	if pruned, err := PruneRichRunData(database, now, RichRunRetentionAge, RichRunRetentionFloor, cleanup); err != nil || pruned != 0 {
		t.Fatalf("cleanup retry = pruned %d, error %v", pruned, err)
	}
	if _, err := os.Stat(oldTarget); !os.IsNotExist(err) {
		t.Fatalf("persisted cleanup target survived root change: %v", err)
	}
}

func TestArchiveRepoRunsArchivesRemainingRunsWhenPriorCleanupStillFails(t *testing.T) {
	database, pending := newAuditRun(t)
	_, record, err := BuildMetricReceipt(database, pending.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	archived, err := database.ArchiveRunWithMetricReceiptAndTargets(record, false, []string{filepath.Join(t.TempDir(), pending.ID)}, func() error {
		return &retentionTestError{message: "pending cleanup still blocked"}
	})
	if !archived || err == nil {
		t.Fatalf("seed pending cleanup = archived %t, error %v", archived, err)
	}

	next, err := database.InsertRun(pending.RepoID, "feature/next", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(next.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cleanup := &RunArtifactCleanup{
		Targets: func(runID string) []string { return []string{filepath.Join(root, runID)} },
		Remove: func(runID string, _ []string) error {
			if runID == pending.ID {
				return &retentionTestError{message: "pending cleanup still blocked"}
			}
			return nil
		},
	}

	count, err := ArchiveRepoRuns(database, pending.RepoID, time.Now(), cleanup)
	var cleanupErr *ArtifactCleanupError
	if count != 1 || !errors.As(err, &cleanupErr) {
		t.Fatalf("archive repository runs = count %d, error %v; want one archive and cleanup-only error", count, err)
	}
	if rich, err := database.GetRun(next.ID); err != nil || rich != nil {
		t.Fatalf("pending cleanup blocked later run archival: %+v, %v", rich, err)
	}
}

func TestRemoveRunArtifactTargetsRejectsPathsNotOwnedByTheRun(t *testing.T) {
	root := t.TempDir()
	unrelated := filepath.Join(root, "another-run")
	if err := os.MkdirAll(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRunArtifactTargets("expected-run", []string{unrelated}); err == nil {
		t.Fatal("cleanup accepted a target not named for the run")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("rejected cleanup target was removed: %v", err)
	}
}

func TestPruneRichRunDataHonorsConfiguredRetentionUnion(t *testing.T) {
	database, oldest := newAuditRun(t)
	for index := 0; index < 50; index++ {
		newer, err := database.InsertRun(oldest.RepoID, "feature/newer", "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateRunStatus(newer.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
	}

	insideConfiguredWindow := time.Unix(oldest.CreatedAt, 0).UTC().Add(20 * 24 * time.Hour)
	if pruned, err := PruneRichRunData(database, insideConfiguredWindow, 30*24*time.Hour, 50, nil); err != nil || pruned != 0 {
		t.Fatalf("prune inside configured retention = %d, %v; want 0", pruned, err)
	}
	if pruned, err := PruneRichRunData(database, insideConfiguredWindow.Add(100*24*time.Hour), 0, 50, nil); err != nil || pruned != 0 {
		t.Fatalf("prune with unlimited retention = %d, %v; want 0", pruned, err)
	}

	outsideConfiguredWindow := time.Unix(oldest.CreatedAt, 0).UTC().Add(31 * 24 * time.Hour)
	if pruned, err := PruneRichRunData(database, outsideConfiguredWindow, 30*24*time.Hour, 50, nil); err != nil || pruned != 1 {
		t.Fatalf("prune outside configured retention = %d, %v; want 1", pruned, err)
	}
}

func TestPruneRichRunDataHonorsConfiguredNewestFloor(t *testing.T) {
	database, oldest := newAuditRun(t)
	for index := 0; index < 200; index++ {
		newer, err := database.InsertRun(oldest.RepoID, "feature/newer", "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateRunStatus(newer.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Unix(oldest.CreatedAt, 0).UTC().Add(15 * 24 * time.Hour)
	if pruned, err := PruneRichRunData(database, now, RichRunRetentionAge, 200, nil); err != nil || pruned != 1 {
		t.Fatalf("prune with configured newest floor = %d, %v; want 1", pruned, err)
	}
}

type retentionTestError struct{ message string }

func (e *retentionTestError) Error() string { return e.message }

func testRunArtifactCleanup(t *testing.T, remove func(string) error) *RunArtifactCleanup {
	t.Helper()
	root := t.TempDir()
	return &RunArtifactCleanup{
		Targets: func(runID string) []string { return []string{filepath.Join(root, runID)} },
		Remove:  func(runID string, _ []string) error { return remove(runID) },
	}
}
