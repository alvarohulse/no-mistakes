package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
	_ "modernc.org/sqlite"
)

func TestExecutor_RecordsCompletedReviewApprovedHead(t *testing.T) {
	database, p, run, repo := setupTest(t)
	const reviewedHead = "1111111111111111111111111111111111111111"
	step := &mockStep{name: types.StepReview, outcome: &StepOutcome{ReviewApprovedHeadSHA: reviewedHead}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewApprovedHeadSHA == nil || *got.ReviewApprovedHeadSHA != reviewedHead {
		t.Fatalf("review-approved head = %#v, want %s", got.ReviewApprovedHeadSHA, reviewedHead)
	}
}

func TestExecutor_FullRereviewReplacesApprovalWithoutAuthorizingParkedRound(t *testing.T) {
	database, p, run, repo := setupTest(t)
	const firstReviewedHead = "1111111111111111111111111111111111111111"
	const rereviewedHead = "2222222222222222222222222222222222222222"
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		if calls == 1 {
			return &StepOutcome{
				NeedsApproval:         true,
				Findings:              `{"findings":[{"id":"r1","severity":"error","description":"fix me","action":"auto-fix"}]}`,
				ReviewApprovedHeadSHA: firstReviewedHead,
			}, nil
		}
		return &StepOutcome{ReviewApprovedHeadSHA: rereviewedHead}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	workDir := t.TempDir()

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.ReviewApprovedHeadSHA != nil {
		t.Fatalf("parked review gained approval authority: %#v", parked.ReviewApprovedHeadSHA)
	}
	if err := exec.Respond(types.StepReview, types.ActionFix, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rereview did not complete")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewApprovedHeadSHA == nil || *got.ReviewApprovedHeadSHA != rereviewedHead {
		t.Fatalf("rereview approval = %#v, want %s", got.ReviewApprovedHeadSHA, rereviewedHead)
	}
}

func TestExecutor_ParkedOrFailedReviewDoesNotAdvanceExistingApproval(t *testing.T) {
	const existingHead = "1111111111111111111111111111111111111111"
	const unapprovedHead = "2222222222222222222222222222222222222222"

	t.Run("parked then aborted", func(t *testing.T) {
		database, p, run, repo := setupTest(t)
		if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, existingHead); err != nil {
			t.Fatal(err)
		}
		step := &mockStep{name: types.StepReview, outcome: &StepOutcome{
			NeedsApproval:         true,
			Findings:              `{"findings":[{"id":"r1","severity":"error","description":"blocked","action":"ask-user"}]}`,
			ReviewApprovedHeadSHA: unapprovedHead,
		}}
		exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
		workDir := t.TempDir()
		done := make(chan error, 1)
		go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
		waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
		got, _ := database.GetRun(run.ID)
		if got.ReviewApprovedHeadSHA == nil || *got.ReviewApprovedHeadSHA != existingHead {
			t.Fatalf("parked review advanced approval: %#v", got.ReviewApprovedHeadSHA)
		}
		if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
			t.Fatal(err)
		}
		<-done
		got, _ = database.GetRun(run.ID)
		if got.ReviewApprovedHeadSHA == nil || *got.ReviewApprovedHeadSHA != existingHead {
			t.Fatalf("aborted review advanced approval: %#v", got.ReviewApprovedHeadSHA)
		}
	})

	t.Run("failed", func(t *testing.T) {
		database, p, run, repo := setupTest(t)
		if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, existingHead); err != nil {
			t.Fatal(err)
		}
		step := newFailStep(types.StepReview, errors.New("review agent failed"))
		exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
		if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
			t.Fatal("expected failed review")
		}
		got, _ := database.GetRun(run.ID)
		if got.ReviewApprovedHeadSHA == nil || *got.ReviewApprovedHeadSHA != existingHead {
			t.Fatalf("failed review advanced approval: %#v", got.ReviewApprovedHeadSHA)
		}
	})
}

func TestExecutor_ApprovalWaiverAndCompletionRollBackTogether(t *testing.T) {
	for _, tt := range []struct {
		name    string
		trigger string
	}{
		{
			name: "waiver write failure",
			trigger: `CREATE TRIGGER reject_waiver
				BEFORE INSERT ON round_decisions
				BEGIN SELECT RAISE(FAIL, 'injected waiver write failure'); END`,
		},
		{
			name: "completion write failure",
			trigger: `CREATE TRIGGER reject_step_completion
				BEFORE UPDATE OF status ON step_results
				WHEN NEW.status = 'completed'
				BEGIN SELECT RAISE(FAIL, 'injected completion write failure'); END`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			raw, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(tt.trigger); err != nil {
				raw.Close()
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			const reviewedHead = "1111111111111111111111111111111111111111"
			step := &mockStep{name: types.StepReview, outcome: &StepOutcome{
				NeedsApproval:         true,
				Findings:              `{"findings":[{"id":"review-1","severity":"error","description":"needs approval","action":"ask-user"}]}`,
				ReviewApprovedHeadSHA: reviewedHead,
			}}
			exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
			done := make(chan error, 1)
			go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
			if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err == nil {
				t.Fatal("approval completed despite injected persistence failure")
			}

			steps, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			rounds, err := database.GetRoundsByStep(steps[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rounds) != 1 || rounds[0].Decision != nil {
				t.Fatalf("waiver survived failed completion: %#v", rounds)
			}
			gotRun, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotRun.ReviewApprovedHeadSHA != nil {
				t.Fatalf("review approval survived failed completion: %#v", gotRun.ReviewApprovedHeadSHA)
			}
		})
	}
}
