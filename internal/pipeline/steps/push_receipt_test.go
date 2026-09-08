package steps

import (
	"context"
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
	database, err := db.Open(filepath.Join(t.TempDir(), "push-receipt.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(filepath.Join(t.TempDir(), "repo"), "https://user:secret@example.com/repo", "main")
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
	sctx := &pipeline.StepContext{
		Ctx: context.Background(), Run: run, Repo: repo, DB: database,
		StepResultID: step.ID, RoundID: round.ID, Paths: paths.WithRoot(t.TempDir()),
		Config: &config.Config{}, WorkDir: repo.WorkingPath,
	}
	recorder := newPushReceiptRecorder(sctx, "https://user:secret@example.com/repo", "refs/heads/feature")
	recorder.pushedSHA = strings.Repeat("c", 40)
	recorder.setDecision(db.PushLeaseOrForceDecisionNewBranch, "created branch on https://user:secret@example.com/repo", "")
	if err := recorder.finish(nil); err != nil {
		t.Fatal(err)
	}
	operations, err := database.GetPushOperationsByRun(run.ID)
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
