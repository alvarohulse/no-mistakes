package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestOpenSanitizesArchivedMetricReceiptsAndReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-cost-receipt.sqlite")
	before, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := before.InsertRepo("/tmp/legacy-cost-receipt", "https://github.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := before.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := before.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	payload := `{"schema_version":4,"archived_at":99,"run":{"id":"` + run.ID + `","repo_id":"` + repo.ID + `","status":"completed","created_at":` + fmt.Sprint(run.CreatedAt) + `},"pull_request":true,"steps":[{"name":"review","custom_step":"kept"}],"invocations":[{"id":"inv-1","reported_cost_usd":null,"usage_coverage":"complete","raw_usage":{"input_tokens":11},"activity":{"tool_calls":2},"custom_invocation":{"kept":true},"costs":{"api_list_estimate":{"value_usd":1.5}}}],"metrics":{"invocation_count":1},"costs":{"api_list_estimate":{"value_usd":1.5}},"integrity_error_count":0,"custom_top":{"kept":"yes"}}`
	record := RunMetricReceipt{
		RunID: run.ID, RepoID: repo.ID, RunCreatedAt: run.CreatedAt, RunStatus: types.RunCompleted,
		SchemaVersion: 4, PayloadJSON: payload, ArchivedAt: 99, PullRequest: true,
		ReportedFindings: 3, FixedFindings: 2,
		StepStats:       []StepStats{{StepName: types.StepReview, ReportedFindings: 3, FixedFindings: 2}},
		AgentAggregates: []AgentInvocationAggregate{{Purpose: "review", Count: 1, TotalDurationMS: 123}},
	}
	if archived, err := before.ArchiveRunWithMetricReceipt(record, true); err != nil || !archived {
		t.Fatalf("archive legacy receipt = %t, %v", archived, err)
	}
	if _, err := before.sql.Exec(`UPDATE run_metric_receipts SET artifact_cleanup_pending = 1 WHERE run_id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	old, err := before.GetRunMetricReceipt(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := before.sql.Exec(`DELETE FROM schema_migrations WHERE name = ?`, runMetricReceiptCostSanitizerMigration); err != nil {
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := after.GetRunMetricReceipt(run.ID)
	if err != nil {
		after.Close()
		t.Fatal(err)
	}
	if migrated.SchemaVersion != 5 || migrated.ReceiptSHA256 == old.ReceiptSHA256 {
		after.Close()
		t.Fatalf("migrated receipt version/digest = %d/%q, old digest %q", migrated.SchemaVersion, migrated.ReceiptSHA256, old.ReceiptSHA256)
	}
	if migrated.RunID != old.RunID || migrated.RepoID != old.RepoID || migrated.RunCreatedAt != old.RunCreatedAt || migrated.RunStatus != old.RunStatus || migrated.ArchivedAt != old.ArchivedAt || migrated.PullRequest != old.PullRequest || migrated.ReportedFindings != old.ReportedFindings || migrated.FixedFindings != old.FixedFindings || !reflect.DeepEqual(migrated.StepStats, old.StepStats) || !reflect.DeepEqual(migrated.AgentAggregates, old.AgentAggregates) {
		after.Close()
		t.Fatalf("receipt metadata changed during migration:\n old: %+v\n new: %+v", old, migrated)
	}
	var cleanupPending int
	if err := after.sql.QueryRow(`SELECT artifact_cleanup_pending FROM run_metric_receipts WHERE run_id = ?`, run.ID).Scan(&cleanupPending); err != nil {
		after.Close()
		t.Fatal(err)
	}
	if cleanupPending != 1 {
		after.Close()
		t.Fatalf("artifact cleanup metadata = %d, want 1", cleanupPending)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(migrated.PayloadJSON), &decoded); err != nil {
		after.Close()
		t.Fatal(err)
	}
	if _, exists := decoded["costs"]; exists {
		after.Close()
		t.Fatalf("top-level costs survived migration: %s", migrated.PayloadJSON)
	}
	if decoded["schema_version"] != float64(5) || !reflect.DeepEqual(decoded["custom_top"], map[string]any{"kept": "yes"}) {
		after.Close()
		t.Fatalf("top-level facts changed during migration: %#v", decoded)
	}
	invocations := decoded["invocations"].([]any)
	invocation := invocations[0].(map[string]any)
	if _, exists := invocation["costs"]; exists {
		after.Close()
		t.Fatalf("invocation costs survived migration: %s", migrated.PayloadJSON)
	}
	if value, exists := invocation["reported_cost_usd"]; !exists || value != nil || invocation["usage_coverage"] != "complete" || !reflect.DeepEqual(invocation["raw_usage"], map[string]any{"input_tokens": float64(11)}) || !reflect.DeepEqual(invocation["activity"], map[string]any{"tool_calls": float64(2)}) || !reflect.DeepEqual(invocation["custom_invocation"], map[string]any{"kept": true}) {
		after.Close()
		t.Fatalf("invocation facts changed during migration: %#v", invocation)
	}
	firstPayload, firstDigest := migrated.PayloadJSON, migrated.ReceiptSHA256
	if err := after.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reopened.GetRunMetricReceipt(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.PayloadJSON != firstPayload || again.ReceiptSHA256 != firstDigest {
		t.Fatalf("idempotent reopen changed receipt:\n first: %s / %s\n again: %s / %s", firstPayload, firstDigest, again.PayloadJSON, again.ReceiptSHA256)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE run_metric_receipts SET receipt_sha256 = 'corrupt' WHERE run_id = ?`, run.ID); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	postMigration, err := Open(path)
	if err != nil {
		t.Fatalf("completed migration rescanned receipts: %v", err)
	}
	defer postMigration.Close()
	if _, err := postMigration.GetRunMetricReceipt(run.ID); err == nil || !strings.Contains(err.Error(), "SHA-256 verification") {
		t.Fatalf("corrupt migrated receipt read error = %v", err)
	}
}

func TestOpenDoesNotRelabelUnsupportedMetricReceiptVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsupported-receipts.sqlite")
	before, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := before.InsertRepo("/tmp/unsupported-receipts", "https://github.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[string]RunMetricReceipt)
	for _, version := range []int{1, 6} {
		runID := fmt.Sprintf("receipt-v%d", version)
		run, err := before.InsertRunWithIDAndOptions(runID, repo.ID, "feature", "head", "base", RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := before.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
		payload := fmt.Sprintf(`{"schema_version":%d,"run":{"id":%q,"repo_id":%q,"status":"completed","created_at":%d},"costs":{"legacy":true}}`, version, run.ID, repo.ID, run.CreatedAt)
		record := RunMetricReceipt{
			RunID: run.ID, RepoID: repo.ID, RunCreatedAt: run.CreatedAt, RunStatus: types.RunCompleted,
			SchemaVersion: version, PayloadJSON: payload, ArchivedAt: run.UpdatedAt,
		}
		if archived, err := before.ArchiveRunWithMetricReceipt(record, true); err != nil || !archived {
			t.Fatalf("archive v%d receipt = %t, %v", version, archived, err)
		}
		stored, err := before.GetRunMetricReceipt(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		want[run.ID] = *stored
	}
	if _, err := before.sql.Exec(`DELETE FROM schema_migrations WHERE name = ?`, runMetricReceiptCostSanitizerMigration); err != nil {
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	for runID, expected := range want {
		got, err := after.GetRunMetricReceipt(runID)
		if err != nil {
			t.Fatal(err)
		}
		if got.SchemaVersion != expected.SchemaVersion {
			t.Fatalf("unsupported receipt %q was relabeled from v%d to v%d", runID, expected.SchemaVersion, got.SchemaVersion)
		}
		if expected.SchemaVersion > RunMetricReceiptSchemaVersion {
			if got.PayloadJSON != expected.PayloadJSON || got.ReceiptSHA256 != expected.ReceiptSHA256 {
				t.Fatalf("future receipt %q was rewritten:\nwant: %+v\n got: %+v", runID, expected, *got)
			}
			continue
		}
		if got.PayloadJSON == expected.PayloadJSON || got.ReceiptSHA256 == expected.ReceiptSHA256 {
			t.Fatalf("legacy receipt %q retained its estimate bytes", runID)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal([]byte(got.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		if _, exists := payload["costs"]; exists {
			t.Fatalf("legacy receipt %q retained costs: %s", runID, got.PayloadJSON)
		}
	}
}

func TestOpenRejectsCorruptArchivedReceiptWithoutPartiallyMigratingSiblings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt-cost-receipt.sqlite")
	before, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := before.InsertRepo("/tmp/corrupt-cost-receipt", "https://github.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"a-valid", "z-corrupt"} {
		run, err := before.InsertRunWithIDAndOptions(runID, repo.ID, "feature", "head", "base", RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := before.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
		payload := `{"schema_version":4,"run":{"id":"` + run.ID + `","repo_id":"` + repo.ID + `","status":"completed","created_at":` + fmt.Sprint(run.CreatedAt) + `},"invocations":[{"id":"inv-1","costs":{"api_list_estimate":{"value_usd":1.5}}}],"costs":{"api_list_estimate":{"value_usd":1.5}}}`
		record := RunMetricReceipt{RunID: run.ID, RepoID: repo.ID, RunCreatedAt: run.CreatedAt, RunStatus: types.RunCompleted, SchemaVersion: 4, PayloadJSON: payload, ArchivedAt: run.UpdatedAt}
		if archived, err := before.ArchiveRunWithMetricReceipt(record, true); err != nil || !archived {
			t.Fatalf("archive %s = %t, %v", runID, archived, err)
		}
	}
	if _, err := before.sql.Exec(`UPDATE run_metric_receipts SET receipt_sha256 = 'corrupt' WHERE run_id = 'z-corrupt'`); err != nil {
		t.Fatal(err)
	}
	if _, err := before.sql.Exec(`DELETE FROM schema_migrations WHERE name = ?`, runMetricReceiptCostSanitizerMigration); err != nil {
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	if opened, err := Open(path); err == nil || !strings.Contains(err.Error(), "z-corrupt") || !strings.Contains(err.Error(), "SHA-256 verification") {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("corrupt receipt open error = %v", err)
	}
	raw, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int
	var payload string
	if err := raw.QueryRow(`SELECT schema_version, payload_json FROM run_metric_receipts WHERE run_id = 'a-valid'`).Scan(&version, &payload); err != nil {
		t.Fatal(err)
	}
	if version != 4 || !strings.Contains(payload, `"costs"`) {
		t.Fatalf("valid sibling was partially migrated: version=%d payload=%s", version, payload)
	}
}

func TestRunMetricReceiptSurvivesCascadesAndDetectsMutation(t *testing.T) {
	database := openTestDB(t)
	repo, err := database.InsertRepo("/tmp/receipt-cascade", "https://github.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	record := RunMetricReceipt{
		RunID: run.ID, RepoID: repo.ID, RunCreatedAt: run.CreatedAt, RunStatus: types.RunCompleted,
		SchemaVersion: 1, PayloadJSON: `{"schema_version":1}`, ArchivedAt: run.UpdatedAt,
		ReportedFindings: 2, FixedFindings: 1,
	}
	archived, err := database.ArchiveRunWithMetricReceipt(record, true)
	if err != nil || !archived {
		t.Fatalf("archive = %v, %v", archived, err)
	}
	if err := database.DeleteRepo(repo.ID); err != nil {
		t.Fatal(err)
	}
	receipt, err := database.GetRunMetricReceipt(run.ID)
	if err != nil || receipt == nil {
		t.Fatalf("receipt after repository cascade = %+v, %v", receipt, err)
	}

	if _, err := database.sql.Exec(`UPDATE run_metric_receipts SET reported_findings = 99 WHERE run_id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetRunMetricReceipt(run.ID); err == nil || !strings.Contains(err.Error(), "SHA-256 verification") {
		t.Fatalf("mutated immutable receipt error = %v", err)
	}
}

func TestArchiveRunWithMetricReceiptPreventsConcurrentPinDuringCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retention-lock.sqlite")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo("/tmp/receipt-lock", "https://github.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}

	competitorSQL, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)&_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatal(err)
	}
	competitorSQL.SetMaxOpenConns(1)
	t.Cleanup(func() { competitorSQL.Close() })
	competitor := &DB{sql: competitorSQL}

	record := RunMetricReceipt{
		RunID: run.ID, RepoID: repo.ID, RunCreatedAt: run.CreatedAt, RunStatus: types.RunCompleted,
		SchemaVersion: 1, PayloadJSON: `{"schema_version":1}`, ArchivedAt: run.UpdatedAt,
	}
	cleanupCalled := false
	archived, err := database.ArchiveRunWithMetricReceiptAndTargets(record, true, []string{filepath.Join(t.TempDir(), run.ID)}, func() error {
		cleanupCalled = true
		if _, pinErr := competitor.SetRunPinned(run.ID, true); pinErr == nil {
			return &retentionTestError{message: "concurrent pin succeeded during cleanup"}
		}
		return nil
	})
	if err != nil || !archived || !cleanupCalled {
		t.Fatalf("archive with concurrent pin = archived %v, cleanup %v, err %v", archived, cleanupCalled, err)
	}
	if retained, err := competitor.GetRun(run.ID); err != nil || retained != nil {
		t.Fatalf("archived run after cleanup = %+v, %v", retained, err)
	}
}

func TestOpenDoesNotMarkLegacyMetricReceiptsForArtifactCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-receipts.sqlite")
	before, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := before.InsertRepo("/tmp/legacy-receipts", "https://github.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := before.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := before.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	record := RunMetricReceipt{
		RunID: run.ID, RepoID: repo.ID, RunCreatedAt: run.CreatedAt, RunStatus: types.RunCompleted,
		SchemaVersion: 1, PayloadJSON: `{"schema_version":1}`, ArchivedAt: run.UpdatedAt,
	}
	if archived, err := before.ArchiveRunWithMetricReceipt(record, true); err != nil || !archived {
		t.Fatalf("archive legacy receipt = %t, %v", archived, err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE run_metric_receipts DROP COLUMN artifact_cleanup_pending`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE run_artifact_cleanup_journal`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { after.Close() })
	var pending int
	if err := after.sql.QueryRow(`SELECT artifact_cleanup_pending FROM run_metric_receipts WHERE run_id = ?`, run.ID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("legacy receipt cleanup pending = %d, want 0", pending)
	}
}

func TestCleanupPendingRunArtifactsContinuesPastReceiptWithoutTargets(t *testing.T) {
	database := openTestDB(t)
	repo, err := database.InsertRepo("/tmp/pending-cleanup", "https://github.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}

	archivePending := func(runID string) (*Run, string) {
		run, err := database.InsertRunWithIDAndOptions(runID, repo.ID, "feature", "head", "base", RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), run.ID)
		record := RunMetricReceipt{
			RunID: run.ID, RepoID: repo.ID, RunCreatedAt: run.CreatedAt, RunStatus: types.RunCompleted,
			SchemaVersion: 1, PayloadJSON: `{"schema_version":1}`, ArchivedAt: run.UpdatedAt,
		}
		archived, err := database.ArchiveRunWithMetricReceiptAndTargets(record, false, []string{target}, func() error {
			return &retentionTestError{message: "leave cleanup pending"}
		})
		if !archived || err == nil {
			t.Fatalf("seed pending cleanup for %s = archived %t, error %v", run.ID, archived, err)
		}
		return run, target
	}

	invalid, _ := archivePending("a-invalid-cleanup")
	valid, validTarget := archivePending("b-valid-cleanup")
	if _, err := database.sql.Exec(`DELETE FROM run_artifact_cleanup_journal WHERE run_id = ?`, invalid.ID); err != nil {
		t.Fatal(err)
	}

	var cleaned []string
	count, err := database.CleanupPendingRunArtifactsWithTargets(repo.ID, func(runID string, targets []string) error {
		cleaned = append(cleaned, runID)
		if runID != valid.ID || len(targets) != 1 || targets[0] != validTarget {
			t.Fatalf("cleanup callback = run %q, targets %v", runID, targets)
		}
		return nil
	})
	if count != 1 || len(cleaned) != 1 || cleaned[0] != valid.ID {
		t.Fatalf("cleanup = count %d, runs %v, error %v; want valid sibling cleaned", count, cleaned, err)
	}
	if err == nil || !strings.Contains(err.Error(), invalid.ID) || !strings.Contains(err.Error(), "has no targets") {
		t.Fatalf("cleanup error = %v; want invalid run identity and missing-target reason", err)
	}
	var pendingCleanupErr *PendingArtifactCleanupError
	if !errors.As(err, &pendingCleanupErr) {
		t.Fatalf("cleanup error = %T, want pending artifact cleanup error", err)
	}

	for _, expectation := range []struct {
		runID   string
		pending int
	}{{invalid.ID, 1}, {valid.ID, 0}} {
		var pending int
		if err := database.sql.QueryRow(`SELECT artifact_cleanup_pending FROM run_metric_receipts WHERE run_id = ?`, expectation.runID).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending != expectation.pending {
			t.Fatalf("run %s cleanup pending = %d, want %d", expectation.runID, pending, expectation.pending)
		}
	}
}

func TestCleanupPendingRunArtifactsDoesNotClassifyCompletionWriteFailureAsCleanupFailure(t *testing.T) {
	database := openTestDB(t)
	repo, err := database.InsertRepo("/tmp/pending-cleanup-write", "https://github.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), run.ID)
	record := RunMetricReceipt{
		RunID: run.ID, RepoID: repo.ID, RunCreatedAt: run.CreatedAt, RunStatus: types.RunCompleted,
		SchemaVersion: 1, PayloadJSON: `{"schema_version":1}`, ArchivedAt: run.UpdatedAt,
	}
	archived, err := database.ArchiveRunWithMetricReceiptAndTargets(record, false, []string{target}, func() error {
		return &retentionTestError{message: "leave cleanup pending"}
	})
	if !archived || err == nil {
		t.Fatalf("seed pending cleanup = archived %t, error %v", archived, err)
	}
	if _, err := database.sql.Exec(`
		CREATE TRIGGER fail_artifact_cleanup_completion
		BEFORE UPDATE OF artifact_cleanup_pending ON run_metric_receipts
		WHEN NEW.artifact_cleanup_pending = 0
		BEGIN
			SELECT RAISE(FAIL, 'injected artifact cleanup completion failure');
		END`); err != nil {
		t.Fatal(err)
	}

	cleanupCalled := false
	count, err := database.CleanupPendingRunArtifactsWithTargets(repo.ID, func(runID string, targets []string) error {
		cleanupCalled = true
		if runID != run.ID || len(targets) != 1 || targets[0] != target {
			t.Fatalf("cleanup callback = run %q, targets %v", runID, targets)
		}
		return nil
	})
	if count != 0 || !cleanupCalled || err == nil || !strings.Contains(err.Error(), "injected artifact cleanup completion failure") {
		t.Fatalf("cleanup completion write = count %d, called %t, error %v", count, cleanupCalled, err)
	}
	var pendingCleanupErr *PendingArtifactCleanupError
	if errors.As(err, &pendingCleanupErr) {
		t.Fatalf("cleanup completion write error was classified as retryable cleanup: %v", err)
	}
	var pending int
	if err := database.sql.QueryRow(`SELECT artifact_cleanup_pending FROM run_metric_receipts WHERE run_id = ?`, run.ID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("cleanup pending = %d, want 1 after completion write failure", pending)
	}
}

type retentionTestError struct{ message string }

func (e *retentionTestError) Error() string { return e.message }
