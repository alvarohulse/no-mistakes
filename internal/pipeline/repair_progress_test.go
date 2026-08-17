package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRepairProgress_StopsOnNormalizedRepeatedFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initGitRepo(t, dir)
	progress := NewRepairProgress(0)

	first := `{"findings":[{"id":"review-1","severity":"warning","file":"main.go","line":10,"description":"  nil pointer   remains ","action":"auto-fix"}]}`
	decision, err := progress.Next(context.Background(), dir, first, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Attempt || decision.Audit.Result != RepairResultAttempted {
		t.Fatalf("first decision = %+v, want attempted", decision)
	}

	writeTestFile(t, dir, "fix.go", "package fix\n")
	second := `{"findings":[{"id":"review-99","severity":"WARNING","file":"main.go","line":91,"description":"nil pointer remains","action":"ask-user","source":"agent"}]}`
	decision, err = progress.Next(context.Background(), dir, second, 8)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Attempt || decision.Audit.Result != RepairResultRepeatedFailure {
		t.Fatalf("second decision = %+v, want repeated failure stop", decision)
	}
	if decision.Audit.FailureFingerprint == "" || decision.Audit.FailureFingerprint != progress.Audit().FailureFingerprint {
		t.Fatalf("failure fingerprint was not retained: %+v", decision.Audit)
	}
}

func TestRepairProgress_IgnoresTimestampOnlyChanges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initGitRepo(t, dir)
	logPath := filepath.Join(dir, "repair.log")
	if err := os.WriteFile(logPath, []byte("same log content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	execGit(t, dir, "add", "repair.log")
	execGit(t, dir, "commit", "-m", "add log")

	progress := NewRepairProgress(0)
	if decision, err := progress.Next(context.Background(), dir, failureJSON("first"), 3); err != nil || !decision.Attempt {
		t.Fatalf("first decision = %+v, err=%v", decision, err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(logPath, later, later); err != nil {
		t.Fatal(err)
	}
	decision, err := progress.Next(context.Background(), dir, failureJSON("different"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Attempt || decision.Audit.Result != RepairResultNoProgress {
		t.Fatalf("timestamp-only decision = %+v, want no-progress stop", decision)
	}
}

func TestRepairProgress_HardCapsConfiguredLimitAtThree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initGitRepo(t, dir)
	progress := NewRepairProgress(0)

	for attempt := 1; attempt <= MaxRepairAttempts; attempt++ {
		decision, err := progress.Next(context.Background(), dir, failureJSON(fmt.Sprintf("failure-%d", attempt)), 99)
		if err != nil {
			t.Fatal(err)
		}
		if !decision.Attempt || decision.AttemptNumber != attempt {
			t.Fatalf("attempt %d decision = %+v", attempt, decision)
		}
		writeTestFile(t, dir, "progress.txt", fmt.Sprintf("progress-%d\n", attempt))
	}

	decision, err := progress.Next(context.Background(), dir, failureJSON("failure-4"), 99)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Attempt || decision.Audit.Result != RepairResultAttemptLimit {
		t.Fatalf("fourth decision = %+v, want hard-cap stop", decision)
	}
}

func failureJSON(description string) string {
	return fmt.Sprintf(`{"findings":[{"severity":"error","description":%q,"action":"auto-fix"}]}`, description)
}
