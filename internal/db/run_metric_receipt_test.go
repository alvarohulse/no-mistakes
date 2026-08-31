package db

import (
	"database/sql"
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

type retentionTestError struct{ message string }

func (e *retentionTestError) Error() string { return e.message }
