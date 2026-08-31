package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
