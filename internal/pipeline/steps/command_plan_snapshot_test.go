package steps

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCommandPlanningSnapshotRetainsBackupAfterFailedRestore(t *testing.T) {
	t.Parallel()
	dir, _, _ := setupGitRepo(t)
	preparedPath := filepath.Join(dir, "prepared.txt")
	if err := os.WriteFile(preparedPath, []byte("prepared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureCommandPlanningSnapshot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	backupDir := snapshot.backupDir
	t.Cleanup(func() { _ = os.RemoveAll(backupDir) })
	if err := os.WriteFile(preparedPath, []byte("mutated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot.indexPath = filepath.Join(dir, "missing", "index")

	if err := snapshot.restore(context.Background(), dir); err == nil {
		t.Fatal("restore() error = nil, want failure after destructive cleanup")
	}
	snapshot.cleanup()
	if _, err := os.Stat(backupDir); err != nil {
		t.Fatalf("recovery backup was removed after failed restore: %v", err)
	}
}
