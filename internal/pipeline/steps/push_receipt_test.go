package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPushReceiptRecorderRedactsTargetAndPersistsOutcome(t *testing.T) {
	sctx := newPushReceiptTestContext(t)
	database := sctx.DB
	recorder := newPushReceiptRecorder(sctx, "https://user:secret@example.com/repo", "refs/heads/feature")
	if err := recorder.start(); err != nil {
		t.Fatal(err)
	}
	recorder.setPushedSHA(strings.Repeat("c", 40))
	recorder.setDecision(db.PushLeaseOrForceDecisionNewBranch, "created branch on https://user:secret@example.com/repo", "")
	if err := recorder.finish(nil); err != nil {
		t.Fatal(err)
	}
	operations, err := database.GetPushOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 {
		t.Fatalf("push receipts = %d, want 1", len(operations))
	}
	operation := operations[0]
	if strings.Contains(operation.TargetIdentity, "secret") || strings.Contains(operation.DecisionReason, "secret") {
		t.Fatalf("credential leaked into receipt: %+v", operation)
	}
	if operation.Outcome != db.PushOperationOutcomeCreated || operation.LeaseOrForceDecision != db.PushLeaseOrForceDecisionNewBranch {
		t.Fatalf("receipt decision = %+v", operation)
	}
}

func TestPushReceiptRecorderPreservesAttemptOrder(t *testing.T) {
	sctx := newPushReceiptTestContext(t)
	recorder := newPushReceiptRecorder(sctx, "https://example.com/repo", "refs/heads/feature")
	if err := recorder.start(); err != nil {
		t.Fatal(err)
	}
	call := 0
	recorder.attemptSnapshot = func() ([]string, error) {
		call++
		if call == 1 {
			return []string{"before"}, nil
		}
		return []string{"before", "attempt-2", "attempt-1"}, nil
	}
	if _, err := recorder.runGit("push", "git --version", "--version"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(recorder.attemptIDs, ","); got != "attempt-2,attempt-1" {
		t.Fatalf("attempt order = %q, want attempt-2,attempt-1", got)
	}
}

func TestPushReceiptRecorderSurfacesPostCommandAttemptLookupFailure(t *testing.T) {
	sctx := newPushReceiptTestContext(t)
	recorder := newPushReceiptRecorder(sctx, "https://example.com/repo", "refs/heads/feature")
	if err := recorder.start(); err != nil {
		t.Fatal(err)
	}
	call := 0
	recorder.attemptSnapshot = func() ([]string, error) {
		call++
		if call == 1 {
			return nil, nil
		}
		return nil, errors.New("attempt lookup unavailable")
	}
	runErr := func() error {
		_, err := recorder.runGit("push", "git --version", "--version")
		return err
	}()
	if runErr == nil || !strings.Contains(runErr.Error(), "record push command attempts") {
		t.Fatalf("run error = %v, want post-command lookup failure", runErr)
	}
	if err := recorder.finish(runErr); err != nil {
		t.Fatal(err)
	}
	receipts, err := sctx.DB.GetPushOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].Outcome != db.PushOperationOutcomeFailed {
		t.Fatalf("terminal receipt = %+v", receipts)
	}
}

func TestPushReceiptRecorderClassifiesSignalExitAsProcessError(t *testing.T) {
	sctx := newPushReceiptTestContext(t)
	recorder := newPushReceiptRecorder(sctx, "https://example.com/repo", "refs/heads/feature")
	if err := recorder.start(); err != nil {
		t.Fatal(err)
	}
	runErr := &pushCommandExitError{command: "git push", code: -1, output: "terminated by signal"}
	if err := recorder.finish(runErr); err != nil {
		t.Fatal(err)
	}
	receipts, err := sctx.DB.GetPushOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].Outcome != db.PushOperationOutcomeProcessError {
		t.Fatalf("signal receipt = %+v", receipts)
	}
}

func TestPushReceiptRecorderRetainsObservedVerificationMismatch(t *testing.T) {
	for _, want := range []string{"normal push", "CI repair push"} {
		t.Run(want, func(t *testing.T) {
			recorder := &pushReceiptRecorder{}
			err := recordVerifiedPushRemoteSHA(recorder, strings.Repeat("d", 40)+"\trefs/heads/feature\n", strings.Repeat("e", 40))
			if err == nil {
				t.Fatal("expected verification mismatch")
			}
			if recorder.remoteAfterSHA == nil || *recorder.remoteAfterSHA != strings.Repeat("d", 40) {
				t.Fatalf("observed remote SHA = %+v, want mismatch SHA", recorder.remoteAfterSHA)
			}
		})
	}
}

func newPushReceiptTestContext(t *testing.T) *pipeline.StepContext {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "push-receipt.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	workDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "init")
	gitCmd(t, workDir, "config", "user.name", "test")
	gitCmd(t, workDir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(workDir, "README"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "README")
	gitCmd(t, workDir, "commit", "-m", "test")
	repo, err := database.InsertRepo(workDir, "https://example.com/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	round, err := database.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &pipeline.StepContext{
		Ctx: context.Background(), Run: run, Repo: repo, DB: database,
		StepResultID: step.ID, RoundID: round.ID, RoundTrigger: "initial",
		Paths: paths.WithRoot(t.TempDir()), Config: &config.Config{}, WorkDir: repo.WorkingPath,
	}
}
