package steps

import (
	"context"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestApprovedFailingConfiguredTestPreservesHistoryWithoutProof(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{Test: "exit 1"})

	continued := make(chan struct{})
	approvalReached := make(chan struct{}, 1)
	exec := pipeline.NewExecutor(
		sctx.DB,
		sctx.Paths,
		sctx.Config,
		sctx.Agent,
		[]pipeline.Step{&TestStep{}, &proofContinuationStep{continued: continued}},
		func(event ipc.Event) {
			if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepTest &&
				event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
				select {
				case approvalReached <- struct{}{}:
				default:
				}
			}
		},
	)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir)
	}()

	select {
	case <-approvalReached:
	case <-time.After(5 * time.Second):
		t.Fatal("configured Test failure never reached its approval gate")
	}
	if err := exec.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve failed Test step: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute pipeline: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline did not continue after approving the failed Test step")
	}
	select {
	case <-continued:
	default:
		t.Fatal("pipeline did not execute the step after Test approval")
	}

	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0].StepName != types.StepTest || steps[0].Status != types.StepStatusCompleted ||
		steps[1].StepName != types.StepLint || steps[1].Status != types.StepStatusCompleted {
		t.Fatalf("step completion after approval = %+v", steps)
	}

	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != "fail" ||
		attempts[0].ExitCode == nil || *attempts[0].ExitCode != 1 || attempts[0].OutputArtifactID == nil ||
		attempts[0].TestedSHA != nil || attempts[0].AcceptedAsProof || attempts[0].ProofReason != nil {
		t.Fatalf("failed configured Test attempt = %+v", attempts)
	}

	proofs, err := sctx.DB.GetAcceptedCommandAttemptsByTestedSHA(sctx.Run.ID, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(proofs) != 0 {
		t.Fatalf("accepted proof for approved failing Test = %+v, want empty", proofs)
	}
}

func TestAcceptedTestProofDoesNotFollowRetryAcrossLaterHead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Runner = runner.Spec{Executable: "sh", Args: []string{"-c"}}
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	command := runner.Command{Run: "exit \"$NM_TEST_EXIT\""}

	runRound := func(round int, trigger, exitCode string) {
		t.Helper()
		wantExitCode, err := strconv.Atoi(exitCode)
		if err != nil {
			t.Fatal(err)
		}
		roundRecord, err := sctx.DB.InsertStepRound(step.ID, round, trigger, nil, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		sctx.Round = round
		sctx.RoundID = roundRecord.ID
		sctx.RoundTrigger = trigger
		sctx.Env = []string{"NM_TEST_EXIT=" + exitCode}
		_, gotExitCode, err := runStepRunnerCommand(sctx, command, string(types.StepTest))
		if err != nil || gotExitCode != wantExitCode {
			t.Fatalf("round %d result = exit %d error %v", round, gotExitCode, err)
		}
	}

	runRound(1, "initial", "1")
	runRound(2, "auto_fix", "0")
	gitCmd(t, dir, "commit", "--allow-empty", "-m", "test: advance tested head")
	laterHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, laterHead); err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA = laterHead
	runRound(3, "auto_fix", "1")
	if err := sctx.DB.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 3 || attempts[0].AcceptedAsProof || !attempts[1].AcceptedAsProof || attempts[2].AcceptedAsProof ||
		attempts[1].RetryOfAttemptID == nil || *attempts[1].RetryOfAttemptID != attempts[0].ID ||
		attempts[2].RetryOfAttemptID != nil || attempts[2].TestedSHA != nil {
		t.Fatalf("retry and mutation history = %+v", attempts)
	}

	firstHeadProofs, err := sctx.DB.GetAcceptedCommandAttemptsByTestedSHA(sctx.Run.ID, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstHeadProofs) != 1 || firstHeadProofs[0].ID != attempts[1].ID {
		t.Fatalf("accepted proof for first tested head = %+v", firstHeadProofs)
	}
	laterHeadProofs, err := sctx.DB.GetAcceptedCommandAttemptsByTestedSHA(sctx.Run.ID, laterHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(laterHeadProofs) != 0 {
		t.Fatalf("accepted proof for later head = %+v, want empty", laterHeadProofs)
	}
}

type proofContinuationStep struct {
	continued chan<- struct{}
}

func (s *proofContinuationStep) Name() types.StepName { return types.StepLint }

func (s *proofContinuationStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	close(s.continued)
	return &pipeline.StepOutcome{}, nil
}
