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
	recorder.verifiedRemoteSHA = pushReceiptStringPointer(strings.Repeat("c", 40))
	recorder.setDecision(db.PushLeaseOrForceDecisionNewBranch, "created branch on https://user:secret@example.com/repo", "")
	recorder.recordBinding(1)
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

func TestRunStepGitCommandResultReturnsPersistedAttemptID(t *testing.T) {
	sctx := newPushReceiptTestContext(t)

	result := runStepGitCommandResult(sctx, "git --version", "push", "--version")
	if err := result.err(); err != nil {
		t.Fatal(err)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || result.attemptID != attempts[0].ID {
		t.Fatalf("command attempt result = %q, attempts = %+v", result.attemptID, attempts)
	}
}

func TestPushReceiptRecorderRecordsOnlyTheDirectAttempt(t *testing.T) {
	sctx := newPushReceiptTestContext(t)
	if _, _, err := runStepGitCommand(sctx, "git --version", "push", "--version"); err != nil {
		t.Fatal(err)
	}
	recorder := newPushReceiptRecorder(sctx, "https://example.com/repo", "refs/heads/feature")
	if err := recorder.start(); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.runGit("push", "git --version", "--version"); err != nil {
		t.Fatal(err)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || len(recorder.attemptIDs) != 1 || recorder.attemptIDs[0] != attempts[1].ID {
		t.Fatalf("receipt attempts = %+v, all attempts = %+v", recorder.attemptIDs, attempts)
	}
}

func TestPushReceiptRecorderCrashRecoveryTerminalizesLinkedAttempts(t *testing.T) {
	sctx := newPushReceiptTestContext(t)
	recorder := newPushReceiptRecorder(sctx, "https://example.com/repo", "refs/heads/feature")
	if err := recorder.start(); err != nil {
		t.Fatal(err)
	}
	pushedSHA := strings.Repeat("c", 40)
	recorder.setPushedSHA(pushedSHA)
	if _, err := recorder.runGit("push", "git --version", "--version"); err != nil {
		t.Fatal(err)
	}
	recorder.verifiedRemoteSHA = pushReceiptStringPointer(pushedSHA)
	recorder.persistProgress()
	generation, err := sctx.DB.UpdateRunPushBindingWithGenerationForOperation(sctx.Run.ID, db.PushBinding{
		HeadSHA:           pushedSHA,
		TargetKind:        recorder.targetKind,
		TargetFingerprint: recorder.targetFingerprint,
		Ref:               recorder.destinationRef,
	}, recorder.operationID)
	if err != nil {
		t.Fatal(err)
	}
	if generation != 1 {
		t.Fatalf("push generation = %d, want 1", generation)
	}
	beforeRecovery, err := sctx.DB.GetPushOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeRecovery) != 1 || beforeRecovery[0].Terminalized || len(beforeRecovery[0].CommandAttemptIDs) != 1 ||
		!beforeRecovery[0].BindingUpdated || beforeRecovery[0].ResultingGeneration == nil || *beforeRecovery[0].ResultingGeneration != 1 ||
		beforeRecovery[0].VerifiedRemoteSHA == nil || *beforeRecovery[0].VerifiedRemoteSHA != pushedSHA {
		t.Fatalf("in-progress receipt = %+v, want linked attempt and atomic binding evidence", beforeRecovery)
	}
	if recovered, err := sctx.DB.RecoverStaleRun(sctx.Run.ID, "daemon crashed during push"); err != nil || !recovered {
		t.Fatalf("recover stale run = %v, %v", recovered, err)
	}
	afterRecovery, err := sctx.DB.GetPushOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRecovery) != 1 || !afterRecovery[0].Terminalized || afterRecovery[0].Outcome != db.PushOperationOutcomeProcessError || len(afterRecovery[0].CommandAttemptIDs) != 1 ||
		!afterRecovery[0].BindingUpdated || afterRecovery[0].ResultingGeneration == nil || *afterRecovery[0].ResultingGeneration != 1 ||
		afterRecovery[0].VerifiedRemoteSHA == nil || *afterRecovery[0].VerifiedRemoteSHA != pushedSHA {
		t.Fatalf("recovered receipt = %+v, want terminal process-error retaining attempt and binding evidence", afterRecovery)
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

func TestExpectedMissingRefErrorRejectsOtherGitFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "missing ref",
			err:  &pushCommandExitError{args: []string{"rev-parse", "--verify", "--quiet", "refs/heads/missing^{commit}"}, code: 1},
			want: true,
		},
		{
			name: "fatal git output",
			err:  &pushCommandExitError{args: []string{"rev-parse", "--verify", "--quiet", "refs/heads/missing^{commit}"}, code: 128, output: "fatal: not a git repository"},
		},
		{
			name: "other command",
			err:  &pushCommandExitError{args: []string{"push", "origin", "HEAD:refs/heads/main"}, code: 1},
		},
		{
			name: "signal",
			err:  &pushCommandExitError{args: []string{"rev-parse", "--verify", "--quiet", "refs/heads/missing^{commit}"}, code: -1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isExpectedMissingRefError(tt.err); got != tt.want {
				t.Fatalf("isExpectedMissingRefError() = %t, want %t", got, tt.want)
			}
		})
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
			if recorder.verifiedRemoteSHA == nil || *recorder.verifiedRemoteSHA != strings.Repeat("d", 40) {
				t.Fatalf("verified remote SHA = %+v, want mismatch SHA", recorder.verifiedRemoteSHA)
			}
		})
	}
}

func TestPushReceiptRecorderDoesNotInferRetryAndUsesUniqueDiagnostics(t *testing.T) {
	sctx := newPushReceiptTestContext(t)
	first := newPushReceiptRecorder(sctx, "https://example.com/repo", "refs/heads/feature")
	if err := first.start(); err != nil {
		t.Fatal(err)
	}
	first.setPushedSHA(strings.Repeat("f", 40))
	first.setDecision(db.PushLeaseOrForceDecisionForceWithLease, "transport failed", strings.Repeat("a", 40))
	if err := first.finish(errors.New("transport failed")); err != nil {
		t.Fatal(err)
	}

	second := newPushReceiptRecorder(sctx, "https://example.com/repo", "refs/heads/feature")
	if err := second.start(); err != nil {
		t.Fatal(err)
	}
	second.setPushedSHA(strings.Repeat("f", 40))
	second.setDecision(db.PushLeaseOrForceDecisionForceWithLease, "retry transport failed", strings.Repeat("a", 40))
	if err := second.finish(errors.New("retry transport failed")); err != nil {
		t.Fatal(err)
	}

	receipts, err := sctx.DB.GetPushOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 2 || receipts[1].RetryOfOperationID != nil || receipts[1].RetryReason != nil {
		t.Fatalf("retry linkage = %+v", receipts)
	}
	artifacts, err := sctx.DB.GetArtifactsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("diagnostic artifacts = %d, want one per failed attempt", len(artifacts))
	}
	if receipts[0].DiagnosticArtifactID == nil || receipts[1].DiagnosticArtifactID == nil || *receipts[0].DiagnosticArtifactID == *receipts[1].DiagnosticArtifactID || artifacts[0].RelativePath == artifacts[1].RelativePath {
		t.Fatalf("diagnostic linkage/path collision = %+v / %+v", receipts, artifacts)
	}
	for _, receipt := range receipts {
		diagnostic, err := sctx.DB.GetArtifact(*receipt.DiagnosticArtifactID)
		if err != nil {
			t.Fatal(err)
		}
		if diagnostic == nil || diagnostic.OperationID == nil || *diagnostic.OperationID != receipt.ID {
			t.Fatalf("diagnostic = %+v, want operation %q", diagnostic, receipt.ID)
		}
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
