package steps

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/artifact"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func beginRefreshReceiptRound(t *testing.T, sctx *pipeline.StepContext) *db.StepRound {
	t.Helper()
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepRefresh)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.BeginStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	sctx.Round = 1
	sctx.RoundID = round.ID
	sctx.RoundTrigger = "initial"
	t.Cleanup(func() {
		if err := sctx.DB.CompleteStepRound(round.ID, nil, nil, 0); err != nil {
			t.Errorf("complete refresh receipt test round: %v", err)
		}
	})
	return round
}

func TestRefreshStepRecordsTargetDecisionsAndPrimaryArtifacts(t *testing.T) {
	t.Parallel()
	dir, upstream, featureHead := setupStackedRefreshRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.RefreshStrategy = types.RefreshStrategyRebase
	sctx.Run.StackedOn = "dependency"
	sctx.Repo.UpstreamURL = upstream
	beginRefreshReceiptRound(t, sctx)

	if _, err := (&RefreshStep{}).Execute(sctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 2 {
		t.Fatalf("refresh operations = %+v, want one decision per target", operations)
	}

	var skipped, rebased *db.RefreshOperation
	for _, operation := range operations {
		switch operation.DestinationRef {
		case "origin/feature":
			skipped = operation
		case "origin/dependency":
			rebased = operation
		}
		if operation.SourceRef != "refs/heads/feature" || operation.AuthoritativeBaseRef != "origin/dependency" {
			t.Fatalf("operation identity = %+v", operation)
		}
	}
	if skipped == nil || skipped.Decision != db.RefreshDecisionSkipped || len(skipped.CommandAttemptIDs) != 0 {
		t.Fatalf("pushed-branch receipt = %+v", skipped)
	}
	if rebased == nil || rebased.Decision != db.RefreshDecisionRebased || rebased.ConflictState != db.RefreshConflictStateNone || len(rebased.CommandAttemptIDs) != 1 {
		t.Fatalf("base refresh receipt = %+v", rebased)
	}
	if rebased.StartingHeadSHA != featureHead || rebased.ResultingHeadSHA == featureHead || rebased.AuthoritativeBaseSHA == nil || *rebased.AuthoritativeBaseSHA == "" {
		t.Fatalf("base refresh identities = %+v", rebased)
	}

	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].ID != rebased.CommandAttemptIDs[0] || attempts[0].OutputArtifactID == nil {
		t.Fatalf("refresh command attempts = %+v", attempts)
	}
	registered, err := sctx.DB.GetArtifact(*attempts[0].OutputArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if registered == nil || registered.Purpose != db.ArtifactPurposeCommandOutput {
		t.Fatalf("refresh output artifact = %+v", registered)
	}
	store, err := artifact.NewStore(sctx.Paths, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(registered); err != nil {
		t.Fatalf("read refresh output artifact: %v", err)
	}
}

func TestRefreshStepRecordsConflictedDecisionWithFailedPrimaryArtifact(t *testing.T) {
	t.Parallel()
	dir, upstream, featureHead := setupConflictingStackedRefreshRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.RefreshStrategy = types.RefreshStrategyMerge
	sctx.Run.StackedOn = "dependency"
	sctx.Repo.UpstreamURL = upstream
	beginRefreshReceiptRound(t, sctx)

	outcome, err := (&RefreshStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("refresh outcome = %+v, want conflict approval", outcome)
	}
	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var conflicted *db.RefreshOperation
	for _, operation := range operations {
		if operation.DestinationRef == "origin/dependency" {
			conflicted = operation
		}
	}
	if conflicted == nil || conflicted.Decision != db.RefreshDecisionConflicted || conflicted.ConflictState != db.RefreshConflictStateDetected || conflicted.RepairState != db.RefreshRepairStateNotAttempted || len(conflicted.CommandAttemptIDs) != 1 {
		t.Fatalf("conflicted receipt = %+v", conflicted)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].OutputArtifactID == nil {
		t.Fatalf("conflicted command attempts = %+v", attempts)
	}
}

func TestRefreshReceiptRefusalStoresDiagnosticArtifact(t *testing.T) {
	t.Parallel()
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, t.TempDir(), "base", "head", config.Commands{})
	beginRefreshReceiptRound(t, sctx)
	recorder := newRefreshReceiptRecorder(sctx, types.RefreshStrategyRebase, "refs/heads/feature", "origin/main")
	reason := strings.Repeat("credentialed refusal details ", 2000)
	if err := recorder.recordRefusal("origin/main", db.RefreshDecisionRefused, reason); err != nil {
		t.Fatal(err)
	}
	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].Decision != db.RefreshDecisionRefused || operations[0].DiagnosticArtifactID == nil {
		t.Fatalf("refusal receipt = %+v", operations)
	}
	registered, err := sctx.DB.GetArtifact(*operations[0].DiagnosticArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if registered == nil || registered.Purpose != db.ArtifactPurposeOperationDiagnostic || registered.SourceBytes > maxRefreshDiagnosticBytes {
		t.Fatalf("refusal diagnostic = %+v", registered)
	}
	store, err := artifact.NewStore(sctx.Paths, "")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := store.Read(registered)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "refresh diagnostic truncated") {
		t.Fatalf("diagnostic contents missing truncation marker: %q", contents)
	}
}

func TestRefreshStep_FailedFeatureFetchLeavesAuthoritativeBaseSHAUnavailable(t *testing.T) {
	t.Parallel()
	dir, _, featureHead := setupStackedRefreshRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = filepath.Join(t.TempDir(), "unreachable-upstream.git")
	sctx.Repo.URLsVerified = true
	beginRefreshReceiptRound(t, sctx)

	if _, err := (&RefreshStep{}).Execute(sctx); err == nil {
		t.Fatal("refresh succeeded despite an unreachable authoritative upstream")
	}
	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 {
		t.Fatalf("fetch-failure receipts = %+v, want one authoritative-base error", operations)
	}
	receipt := operations[0]
	if receipt.Decision != db.RefreshDecisionError || receipt.DestinationRef != "origin/main" {
		t.Fatalf("fetch-failure receipt = %+v", receipt)
	}
	if receipt.AuthoritativeBaseSHA != nil {
		t.Fatalf("fetch-failure receipt claimed authoritative base SHA %q before fetch resolution", *receipt.AuthoritativeBaseSHA)
	}
}

func TestRefreshStepPreparationFailureDoesNotBorrowPriorCommandAttempt(t *testing.T) {
	t.Parallel()
	dir, upstream, featureHead := setupStackedRefreshRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.RefreshStrategy = types.RefreshStrategyRebase
	sctx.Run.StackedOn = "dependency"
	sctx.Repo.UpstreamURL = upstream
	beginRefreshReceiptRound(t, sctx)

	if _, _, err := runStepRunnerCommand(sctx, runner.Command{Run: "printf prior"}, string(types.StepRefresh)); err != nil {
		t.Fatalf("record prior command: %v", err)
	}
	sctx.Config.Runner = runner.Spec{Executable: "not-a-supported-shell", Args: []string{"-c"}}

	if _, err := (&RefreshStep{}).Execute(sctx); err == nil {
		t.Fatal("expected refresh command preparation failure")
	}
	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range operations {
		if operation.DestinationRef != "origin/dependency" {
			continue
		}
		if operation.Decision != db.RefreshDecisionError || len(operation.CommandAttemptIDs) != 0 || operation.DiagnosticArtifactID == nil {
			t.Fatalf("preparation-failure receipt = %+v", operation)
		}
		return
	}
	t.Fatalf("missing preparation-failure receipt: %+v", operations)
}

func TestRefreshGitCommandScopesBareRepositoriesExplicitly(t *testing.T) {
	t.Parallel()
	bare := t.TempDir()
	gitCmd(t, bare, "init", "--bare")

	command := refreshGitCommand(&pipeline.StepContext{WorkDir: bare}, "rev-parse", "HEAD")
	if !strings.Contains(command, "--git-dir="+bare) {
		t.Fatalf("bare refresh command = %q, want explicit git-dir", command)
	}
}
