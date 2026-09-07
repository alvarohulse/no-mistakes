package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunStartExecutesPostWorktreeHookBeforeFirstStep(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "post-worktree-success")
	head := commitPostWorktreeHook(t, repo, postWorktreeSuccessHook())

	step := &assertPostWorktreeEffectStep{check: func(workDir string) error {
		data, err := os.ReadFile(filepath.Join(workDir, "post-worktree.marker"))
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(data)) != "ready" {
			return &unexpectedHookEffectError{got: string(data)}
		}
		return nil
	}}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "post-worktree hook", "", "", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}
	if got := step.executions; got != 1 {
		t.Fatalf("first step executions = %d, want 1", got)
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].StepName != types.StepReview {
		t.Fatalf("step records = %+v, want only review (no hook step)", steps)
	}
}

func TestRunStartParksPostWorktreeHookFailureBeforeStepRecords(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "post-worktree-failure")
	head := commitPostWorktreeHook(t, repo, postWorktreeFailingHook())

	step := &mockPassStep{name: types.StepIntent}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "post-worktree hook", "", "", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	run := waitForPostWorktreePark(t, database, runID)
	if run.Status != types.RunRunning || run.AwaitingAgentSince == nil {
		t.Fatalf("parked run = status %s awaiting %v", run.Status, run.AwaitingAgentSince)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "post-worktree hook failed with exit code 23") || !strings.Contains(*run.Error, "authenticate first") {
		t.Fatalf("parked run error = %v", run.Error)
	}
	if got := step.execCnt.Load(); got != 0 {
		t.Fatalf("intent executed %d times, want 0", got)
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Fatalf("step records = %+v, want none before intent", steps)
	}
	worktree := p.WorktreeDir(repo.ID, runID)
	if info, err := os.Stat(worktree); err != nil || !info.IsDir() {
		t.Fatalf("parked worktree missing: %v", err)
	}

	if err := manager.HandleCancel(runID); err != nil {
		t.Fatalf("cancel parked run: %v", err)
	}
	cancelled := waitForRunStatus(t, database, runID, types.RunCancelled)
	if cancelled.AwaitingAgentSince != nil {
		t.Fatalf("cancelled run remained parked: %v", cancelled.AwaitingAgentSince)
	}
}

func TestRunStartFallsBackAfterPostWorktreeTerminalizationBudget(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "post-worktree-terminal-fallback")
	head := commitPostWorktreeHook(t, repo, postWorktreeFailingHook())
	raw := installRunUpdateTrigger(t, p.DB(), `
		CREATE TRIGGER reject_primary_post_worktree_terminalization
		BEFORE UPDATE OF status ON runs WHEN NEW.status = 'cancelled' AND NEW.parked_ms < 5000
		BEGIN SELECT RAISE(FAIL, 'injected primary terminalization failure'); END;
	`)

	manager := NewRunManager(database, p, nil)
	manager.postWorktreeTerminalizationTimeout = 500 * time.Millisecond
	t.Cleanup(manager.Shutdown)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "post-worktree fallback", "", "", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	waitForPostWorktreePark(t, database, runID)
	if _, err := raw.Exec(`UPDATE runs SET awaiting_agent_since = ? WHERE id = ?`, time.Now().Add(-10*time.Second).Unix(), runID); err != nil {
		t.Fatal(err)
	}
	subscription, err := manager.Subscribe(runID)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	if event, ok := subscription.Next(context.Background()); !ok || event.Type != ipc.EventStreamGap {
		t.Fatalf("initial subscription event = (%+v, %v), want stream gap", event, ok)
	}

	manager.mu.Lock()
	done := manager.dones[runID]
	manager.mu.Unlock()
	if done == nil {
		t.Fatal("fallback run lost its completion handle")
	}
	if err := manager.HandleCancel(runID); err != nil {
		t.Fatalf("cancel parked run: %v", err)
	}
	earlyReadCtx, stopEarlyRead := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if event, ok := subscription.Next(earlyReadCtx); ok {
		t.Fatalf("fallback run broadcast terminal event before durable fallback: %+v", event)
	}
	stopEarlyRead()
	if run, err := database.GetRun(runID); err != nil {
		t.Fatal(err)
	} else if run.Status != types.RunRunning || run.AwaitingAgentSince == nil {
		t.Fatalf("fallback run before fallback = status %s awaiting=%v, want active parked row", run.Status, run.AwaitingAgentSince)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fallback run did not finish terminalization")
	}
	got, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCancelled || got.AwaitingAgentSince != nil || got.Error == nil || *got.Error != types.RunCancelReasonAbortedByUser {
		t.Fatalf("fallback terminalized run = status %s awaiting=%v error=%v, want cancelled terminal state", got.Status, got.AwaitingAgentSince, got.Error)
	}
	if got.ParkedMS < 5000 {
		t.Fatalf("fallback terminalized parked_ms = %d, want accrued parked duration", got.ParkedMS)
	}
	if event, ok := subscription.Next(context.Background()); !ok || event.Type != ipc.EventRunCompleted {
		t.Fatalf("terminal event = (%+v, %v), want completed terminal state", event, ok)
	}
	active, err := database.GetActiveRuns()
	if err != nil {
		t.Fatal(err)
	}
	for _, activeRun := range active {
		if activeRun.ID == runID {
			t.Fatalf("fallback run remained active: %+v", activeRun)
		}
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, runID)); !os.IsNotExist(err) {
		t.Fatalf("fallback run worktree = %v, want removed", err)
	}
	if _, err := git.RunBare(context.Background(), p.RepoDir(repo.ID), "rev-parse", "--verify", policyTrustedRunRef(runID)); err == nil {
		t.Fatal("fallback run trusted ref was retained")
	}
	manager.mu.Lock()
	_, executorRetained := manager.executors[runID]
	_, cancelRetained := manager.cancels[runID]
	_, doneRetained := manager.dones[runID]
	manager.mu.Unlock()
	if executorRetained || cancelRetained || doneRetained {
		t.Fatalf("fallback run tracking retained = executor:%v cancel:%v done:%v, want all false", executorRetained, cancelRetained, doneRetained)
	}
}

func TestRunStartRetainsUnresolvedPostWorktreeRunAndQuarantinesDaemon(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "post-worktree-unresolved")
	head := commitPostWorktreeHook(t, repo, postWorktreeFailingHook())
	installRunUpdateTrigger(t, p.DB(), `
		CREATE TRIGGER reject_post_worktree_terminalization
		BEFORE UPDATE OF status ON runs WHEN NEW.status = 'cancelled'
		BEGIN SELECT RAISE(FAIL, 'injected terminal write failure'); END;
	`)

	manager := NewRunManager(database, p, nil)
	manager.postWorktreeTerminalizationTimeout = 150 * time.Millisecond
	t.Cleanup(manager.Shutdown)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "post-worktree unresolved", "", "", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	waitForPostWorktreePark(t, database, runID)
	subscription, err := manager.Subscribe(runID)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	if event, ok := subscription.Next(context.Background()); !ok || event.Type != ipc.EventStreamGap {
		t.Fatalf("initial subscription event = (%+v, %v), want stream gap", event, ok)
	}
	// The park may have been broadcast before Subscribe attached. Drain any
	// queued update if present, but do not require it because the DB snapshot
	// above is the authoritative park assertion.
	parkReadCtx, stopParkRead := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if event, ok := subscription.Next(parkReadCtx); ok && event.Type != ipc.EventRunUpdated {
		t.Fatalf("park subscription event = (%+v, %v), want updated parked state", event, ok)
	}
	stopParkRead()

	if err := manager.HandleCancel(runID); err != nil {
		t.Fatalf("cancel parked run: %v", err)
	}
	manager.mu.Lock()
	done := manager.dones[runID]
	manager.mu.Unlock()
	if done == nil {
		t.Fatal("unresolved run lost its completion handle")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unresolved run did not finish its bounded terminalization attempt")
	}

	got, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning || got.AwaitingAgentSince == nil {
		t.Fatalf("unresolved run = status %s awaiting=%v, want active parked row", got.Status, got.AwaitingAgentSince)
	}
	worktree := p.WorktreeDir(repo.ID, runID)
	if info, err := os.Stat(worktree); err != nil || !info.IsDir() {
		t.Fatalf("unresolved run worktree = %v, want retained directory", err)
	}
	manager.mu.Lock()
	_, executorRetained := manager.executors[runID]
	_, cancelRetained := manager.cancels[runID]
	_, doneRetained := manager.dones[runID]
	manager.mu.Unlock()
	if !executorRetained || !cancelRetained || !doneRetained {
		t.Fatalf("unresolved run tracking retained = executor:%v cancel:%v done:%v, want all true", executorRetained, cancelRetained, doneRetained)
	}
	if _, err := os.Stat(p.EffectiveConfigYAML(runID)); err != nil {
		t.Fatalf("unresolved run effective config artifact missing: %v", err)
	}
	if _, err := git.RunBare(context.Background(), p.RepoDir(repo.ID), "rev-parse", "--verify", policyTrustedRunRef(runID)); err != nil {
		t.Fatalf("unresolved run trusted ref missing: %v", err)
	}
	if !manager.shuttingDown.Load() {
		t.Fatal("unresolved run did not quarantine the daemon")
	}

	readCtx, stopRead := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stopRead()
	for {
		event, ok := subscription.Next(readCtx)
		if !ok {
			break
		}
		if event.Type == ipc.EventRunCompleted {
			t.Fatalf("unresolved run broadcast terminal event: %+v", event)
		}
	}
	if _, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "blocked replacement", "", "", ""); err == nil || !strings.Contains(err.Error(), "daemon is shutting down") {
		t.Fatalf("replacement start error = %v, want daemon quarantine refusal", err)
	}
}

func TestRunStartQuarantinesWhenPostWorktreeTerminalStateCannotBeVerified(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "post-worktree-unverified")
	head := commitPostWorktreeHook(t, repo, postWorktreeFailingHook())
	installRunUpdateTrigger(t, p.DB(), `
		CREATE TRIGGER ignore_post_worktree_terminalization
		BEFORE UPDATE OF status ON runs WHEN NEW.status = 'cancelled'
		BEGIN SELECT RAISE(IGNORE); END;
	`)

	manager := NewRunManager(database, p, nil)
	manager.postWorktreeTerminalizationTimeout = 150 * time.Millisecond
	t.Cleanup(manager.Shutdown)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "post-worktree unverified", "", "", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	waitForPostWorktreePark(t, database, runID)
	subscription, err := manager.Subscribe(runID)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	if event, ok := subscription.Next(context.Background()); !ok || event.Type != ipc.EventStreamGap {
		t.Fatalf("initial subscription event = (%+v, %v), want stream gap", event, ok)
	}

	manager.mu.Lock()
	done := manager.dones[runID]
	manager.mu.Unlock()
	if done == nil {
		t.Fatal("unverified run lost its completion handle")
	}
	if err := manager.HandleCancel(runID); err != nil {
		t.Fatalf("cancel parked run: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unverified run did not finish its bounded terminalization attempt")
	}

	got, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning || got.AwaitingAgentSince == nil {
		t.Fatalf("unverified run = status %s awaiting=%v, want active parked row", got.Status, got.AwaitingAgentSince)
	}
	if info, err := os.Stat(p.WorktreeDir(repo.ID, runID)); err != nil || !info.IsDir() {
		t.Fatalf("unverified run worktree = %v, want retained directory", err)
	}
	if _, err := os.Stat(p.EffectiveConfigYAML(runID)); err != nil {
		t.Fatalf("unverified run effective config artifact missing: %v", err)
	}
	if _, err := git.RunBare(context.Background(), p.RepoDir(repo.ID), "rev-parse", "--verify", policyTrustedRunRef(runID)); err != nil {
		t.Fatalf("unverified run trusted ref missing: %v", err)
	}
	if !manager.shuttingDown.Load() {
		t.Fatal("unverified run did not quarantine the daemon")
	}

	readCtx, stopRead := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stopRead()
	for {
		event, ok := subscription.Next(readCtx)
		if !ok {
			break
		}
		if event.Type == ipc.EventRunCompleted {
			t.Fatalf("unverified run broadcast terminal event: %+v", event)
		}
	}
}

func TestPostWorktreeParkFailureKeepsDatabaseAuthoritative(t *testing.T) {
	t.Run("fallback persists failed run", func(t *testing.T) {
		p, database := newRefreshRunFixture(t)
		repo, _ := database.InsertRepo("/tmp/post-worktree-fallback", "https://github.com/test/fallback", "main")
		run, err := database.InsertRun(repo.ID, "feature", "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		installRunUpdateTrigger(t, p.DB(), `
			CREATE TRIGGER reject_environment_park
			BEFORE UPDATE OF awaiting_agent_since ON runs
			BEGIN SELECT RAISE(FAIL, 'injected environment park failure'); END;
		`)

		manager := NewRunManager(database, p, nil)
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(errors.New(types.RunCancelReasonAbortedByUser))
		if err := manager.parkPostWorktreeFailure(ctx, run, repo, errors.New("hook failed")); err == nil {
			t.Fatal("parkPostWorktreeFailure() error = nil")
		}

		got, err := database.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != types.RunFailed || got.Error == nil {
			t.Fatalf("fallback run = status %s error %v, want failed with error", got.Status, got.Error)
		}
	})

	t.Run("failed fallback emits no false terminal event", func(t *testing.T) {
		p, database := newRefreshRunFixture(t)
		repo, _ := database.InsertRepo("/tmp/post-worktree-double-failure", "https://github.com/test/double-failure", "main")
		run, err := database.InsertRun(repo.ID, "feature", "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		installRunUpdateTrigger(t, p.DB(), `
			CREATE TRIGGER reject_environment_park
			BEFORE UPDATE OF awaiting_agent_since ON runs
			BEGIN SELECT RAISE(FAIL, 'injected environment park failure'); END;
			CREATE TRIGGER reject_failed_fallback
			BEFORE UPDATE OF status ON runs WHEN NEW.status = 'failed'
			BEGIN SELECT RAISE(FAIL, 'injected failed fallback failure'); END;
		`)

		manager := NewRunManager(database, p, nil)
		mailbox := newEventMailbox(run.ID, 0)
		if event, ok := mailbox.next(context.Background()); !ok || event.Type != ipc.EventStreamGap {
			t.Fatalf("initial mailbox event = (%+v, %v), want stream gap", event, ok)
		}
		manager.subscribers[run.ID] = []*eventMailbox{mailbox}
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(errors.New(types.RunCancelReasonAbortedByUser))
		err = manager.parkPostWorktreeFailure(ctx, run, repo, errors.New("hook failed"))
		if err == nil || !strings.Contains(err.Error(), "injected failed fallback failure") {
			t.Fatalf("parkPostWorktreeFailure() error = %v, want fallback persistence failure", err)
		}

		got, err := database.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != types.RunPending || run.Status != types.RunPending {
			t.Fatalf("failed fallback status = database %s memory %s, want pending authority preserved", got.Status, run.Status)
		}
		readCtx, stopRead := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer stopRead()
		if event, ok := mailbox.next(readCtx); ok {
			t.Fatalf("failed fallback broadcast false terminal event: %+v", event)
		}
	})

	t.Run("terminal write failure retries after cancelled context", func(t *testing.T) {
		p, database := newRefreshRunFixture(t)
		repo, _ := database.InsertRepo("/tmp/post-worktree-terminal", "https://github.com/test/terminal", "main")
		run, err := database.InsertRun(repo.ID, "feature", "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		raw := installRunUpdateTrigger(t, p.DB(), `
			CREATE TRIGGER reject_post_worktree_terminalization
			BEFORE UPDATE OF status ON runs WHEN NEW.status = 'cancelled'
			BEGIN SELECT RAISE(FAIL, 'injected terminal write failure'); END;
		`)

		manager := NewRunManager(database, p, nil)
		subscription, err := manager.Subscribe(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer subscription.Close()
		if event, ok := subscription.Next(context.Background()); !ok || event.Type != ipc.EventStreamGap {
			t.Fatalf("initial subscription event = (%+v, %v), want stream gap", event, ok)
		}

		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(errors.New(types.RunCancelReasonAbortedByUser))
		result := make(chan error, 1)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			result <- manager.parkPostWorktreeFailureWithTerminalizationTimeout(ctx, run, repo, errors.New("hook failed"), time.Second)
		}()
		t.Cleanup(func() {
			_, _ = raw.Exec(`DROP TRIGGER IF EXISTS reject_post_worktree_terminalization`)
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Error("terminalization retry did not finish after database recovery")
			}
		})

		if event, ok := subscription.Next(context.Background()); !ok || event.Type != ipc.EventRunUpdated {
			t.Fatalf("park event = (%+v, %v), want updated parked state", event, ok)
		}

		time.Sleep(2 * postWorktreeTerminalizeRetryInterval)
		got, err := database.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != types.RunRunning || got.AwaitingAgentSince == nil || got.Error == nil || *got.Error != "hook failed" {
			t.Fatalf("terminal write failure persisted status=%s awaiting=%v error=%v, want running parked hook failure", got.Status, got.AwaitingAgentSince, got.Error)
		}
		if run.Status != types.RunRunning || run.Error == nil || *run.Error != "hook failed" {
			t.Fatalf("terminal write failure mutated in-memory run: status=%s error=%v", run.Status, run.Error)
		}
		select {
		case err := <-result:
			t.Fatalf("parkPostWorktreeFailure() returned before terminalization persisted: %v", err)
		default:
		}
		if _, err := raw.Exec(`DROP TRIGGER reject_post_worktree_terminalization`); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			if err == nil || !strings.Contains(err.Error(), types.RunCancelReasonAbortedByUser) {
				t.Fatalf("parkPostWorktreeFailure() error = %v, want cancelled terminal error", err)
			}
		case <-time.After(time.Second):
			t.Fatal("terminalization retry did not finish after database recovery")
		}
		got, err = database.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != types.RunCancelled || got.AwaitingAgentSince != nil || got.Error == nil || *got.Error != types.RunCancelReasonAbortedByUser {
			t.Fatalf("terminalized run = status %s awaiting=%v error=%v, want cancelled terminal state", got.Status, got.AwaitingAgentSince, got.Error)
		}
		if event, ok := subscription.Next(context.Background()); !ok || event.Type != ipc.EventRunCompleted {
			t.Fatalf("terminal event = (%+v, %v), want completed terminal state", event, ok)
		}
	})

	t.Run("persistent terminal write failure expires without terminal event", func(t *testing.T) {
		p, database := newRefreshRunFixture(t)
		repo, _ := database.InsertRepo("/tmp/post-worktree-terminal-timeout", "https://github.com/test/terminal-timeout", "main")
		run, err := database.InsertRun(repo.ID, "feature", "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		installRunUpdateTrigger(t, p.DB(), `
			CREATE TRIGGER reject_post_worktree_terminalization
			BEFORE UPDATE OF status ON runs WHEN NEW.status = 'cancelled'
			BEGIN SELECT RAISE(FAIL, 'injected terminal write failure'); END;
		`)

		manager := NewRunManager(database, p, nil)
		subscription, err := manager.Subscribe(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer subscription.Close()
		if event, ok := subscription.Next(context.Background()); !ok || event.Type != ipc.EventStreamGap {
			t.Fatalf("initial subscription event = (%+v, %v), want stream gap", event, ok)
		}

		cancelCause := errors.New(types.RunCancelReasonAbortedByUser)
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cancelCause)
		result := make(chan error, 1)
		go func() {
			result <- manager.parkPostWorktreeFailureWithTerminalizationTimeout(ctx, run, repo, errors.New("hook failed"), 150*time.Millisecond)
		}()

		var terminalErr error
		select {
		case terminalErr = <-result:
		case <-time.After(time.Second):
			t.Fatal("terminalization did not stop within the shortened retry budget")
		}
		var unresolvedErr *unresolvedPostWorktreeRunError
		if !errors.As(terminalErr, &unresolvedErr) {
			t.Fatalf("terminalization error = %T %v, want unresolved post-worktree error", terminalErr, terminalErr)
		}
		if !errors.Is(terminalErr, cancelCause) {
			t.Fatalf("terminalization error = %v, want original cancellation cause", terminalErr)
		}
		if !errors.Is(terminalErr, context.DeadlineExceeded) {
			t.Fatalf("terminalization error = %v, want retry deadline exhaustion", terminalErr)
		}
		if !strings.Contains(terminalErr.Error(), "injected terminal write failure") {
			t.Fatalf("terminalization error = %v, want final database failure", terminalErr)
		}

		got, err := database.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != types.RunRunning || got.AwaitingAgentSince == nil || got.Error == nil || *got.Error != "hook failed" {
			t.Fatalf("terminal timeout persisted status=%s awaiting=%v error=%v, want running parked hook failure", got.Status, got.AwaitingAgentSince, got.Error)
		}
		if run.Status != types.RunRunning || run.Error == nil || *run.Error != "hook failed" {
			t.Fatalf("terminal timeout mutated in-memory run: status=%s error=%v", run.Status, run.Error)
		}
		if event, ok := subscription.Next(context.Background()); !ok || event.Type != ipc.EventRunUpdated {
			t.Fatalf("park event = (%+v, %v), want updated parked state", event, ok)
		}
		readCtx, stopRead := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer stopRead()
		if event, ok := subscription.Next(readCtx); ok {
			t.Fatalf("terminal timeout broadcast terminal event: %+v", event)
		}
	})
}

func TestRunStartRetainsCIFixRepairDurabilityUncertainty(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "ci-repair-durability-uncertain")
	started := make(chan struct{})
	release := make(chan struct{})
	step := &controlledFailureStep{
		name:    types.StepCI,
		started: started,
		release: release,
		err:     pipeline.NewCIFixRepairDurabilityError(errors.New("pushed CI repair receipt transaction failed")),
	}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	setSafeBareRepositoryExplicitForDaemonTest(t)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "CI repair durability uncertainty", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("CI step did not start")
	}
	subscription, err := manager.Subscribe(runID)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	revisionBeforeFailure := manager.StateRev(runID)
	manager.mu.Lock()
	done := manager.dones[runID]
	manager.mu.Unlock()
	if done == nil {
		t.Fatal("durability-uncertain run lost its completion handle")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("durability-uncertain run did not stop")
	}
	if manager.StateRev(runID) != revisionBeforeFailure {
		t.Fatal("durability uncertainty emitted a terminal state event")
	}
	if event, ok := subscription.Next(context.Background()); !ok || event.Type != ipc.EventStreamGap {
		t.Fatalf("initial subscription event = (%+v, %v), want stream gap", event, ok)
	}
	got, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning {
		t.Fatalf("run status = %s, want running", got.Status)
	}
	stepResults, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stepResults) != 1 || stepResults[0].Status != types.StepStatusRunning {
		t.Fatalf("step results = %#v, want active CI step", stepResults)
	}
	manager.mu.Lock()
	_, executorRetained := manager.executors[runID]
	_, cancelRetained := manager.cancels[runID]
	_, doneRetained := manager.dones[runID]
	manager.mu.Unlock()
	if !executorRetained || !cancelRetained || !doneRetained {
		t.Fatalf("run tracking retained = executor:%v cancel:%v done:%v, want all true", executorRetained, cancelRetained, doneRetained)
	}
	manager.subMu.Lock()
	completed := manager.completedRuns[runID]
	subscriberCount := len(manager.subscribers[runID])
	manager.subMu.Unlock()
	if completed || subscriberCount != 1 {
		t.Fatalf("subscriber retention = completed:%v count:%d, want false and one", completed, subscriberCount)
	}
	if !manager.shuttingDown.Load() {
		t.Fatal("durability uncertainty did not quarantine the daemon")
	}
	if info, err := os.Stat(p.WorktreeDir(repo.ID, runID)); err != nil || !info.IsDir() {
		t.Fatalf("retained worktree = %v, want directory", err)
	}
	if _, err := os.Stat(p.EffectiveConfigYAML(runID)); err != nil {
		t.Fatalf("retained effective config missing: %v", err)
	}
	if _, err := git.RunBare(context.Background(), p.RepoDir(repo.ID), "rev-parse", "--verify", policyTrustedRunRef(runID)); err != nil {
		t.Fatalf("retained trusted ref missing: %v", err)
	}
}

func TestRunStartOrdinaryPipelineFailureDoesNotQuarantineAdmission(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "ordinary-pipeline-failure")
	step := &controlledFailureStep{
		name: types.StepCI,
		err:  errors.New("ordinary CI failure"),
	}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "ordinary pipeline failure", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed", run.Status)
	}
	if manager.shuttingDown.Load() {
		t.Fatal("ordinary pipeline failure quarantined the daemon")
	}
	nextRunID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "ordinary replacement", "", "", "")
	if err != nil {
		t.Fatalf("replacement after ordinary failure: %v", err)
	}
	if run := waitForRunTerminalState(t, database, nextRunID); run.Status != types.RunFailed {
		t.Fatalf("replacement status = %s, want failed", run.Status)
	}
}

type assertPostWorktreeEffectStep struct {
	check      func(string) error
	executions int
}

func (s *assertPostWorktreeEffectStep) Name() types.StepName { return types.StepReview }

func (s *assertPostWorktreeEffectStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.executions++
	if err := s.check(sctx.WorkDir); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, nil
}

type unexpectedHookEffectError struct{ got string }

func (e *unexpectedHookEffectError) Error() string {
	return "unexpected post-worktree marker: " + e.got
}

type controlledFailureStep struct {
	name    types.StepName
	started chan struct{}
	release <-chan struct{}
	err     error
}

func (s *controlledFailureStep) Name() types.StepName { return s.name }

func (s *controlledFailureStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if s.started != nil {
		close(s.started)
	}
	if s.release != nil {
		<-s.release
	}
	return nil, s.err
}

// postWorktreeSuccessHook writes the "ready" marker the effect step verifies.
// The daemon runs the hook via cmd.exe on Windows and sh elsewhere, so the
// shell syntax must match the target interpreter.
func postWorktreeSuccessHook() string {
	if runtime.GOOS == "windows" {
		return "echo ready>post-worktree.marker"
	}
	return "printf 'ready\\n' >> post-worktree.marker"
}

// postWorktreeFailingHook prints a recognizable message and exits 23 so the
// park assertion can match both the exit code and the emitted output.
func postWorktreeFailingHook() string {
	if runtime.GOOS == "windows" {
		return "echo authenticate first& exit 23"
	}
	return "printf 'authenticate first\\n'; exit 23"
}

func commitPostWorktreeHook(t *testing.T, repo *db.Repo, hook string) string {
	t.Helper()
	configYAML := "auto_fix:\n  lint: 0\n  test: 0\n  review: 0\nhooks:\n  post_worktree: " + yamlDoubleQuoted(hook) + "\n"
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, ".no-mistakes.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "configure post-worktree hook")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	return gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
}

func yamlDoubleQuoted(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + replacer.Replace(value) + `"`
}

func waitForPostWorktreePark(t *testing.T, database *db.DB, runID string) *db.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := database.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.Status == types.RunRunning && run.AwaitingAgentSince != nil && run.Error != nil {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not park on post-worktree failure", runID)
	return nil
}

func waitForRunStatus(t *testing.T, database *db.DB, runID string, status types.RunStatus) *db.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := database.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.Status == status {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach status %s", runID, status)
	return nil
}

func installRunUpdateTrigger(t *testing.T, databasePath, statement string) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec(statement); err != nil {
		t.Fatal(err)
	}
	return raw
}
