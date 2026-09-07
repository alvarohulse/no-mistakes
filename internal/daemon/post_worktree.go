package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/worktreehook"
)

func runPostWorktreeHook(ctx context.Context, workDir string, cfg *config.Config) error {
	return worktreehook.Run(ctx, workDir, cfg)
}

const (
	postWorktreeTerminalizeRetryInterval = 100 * time.Millisecond
	postWorktreeTerminalizationTimeout   = 30 * time.Second
)

// unresolvedPostWorktreeRunError means the run's durable terminal state could
// not be persisted. The caller must retain the run's ownership material so
// startup recovery can settle the active row from a complete worktree and
// effective-config record.
type unresolvedPostWorktreeRunError struct {
	cause error
}

func (e *unresolvedPostWorktreeRunError) Error() string { return e.cause.Error() }

func (e *unresolvedPostWorktreeRunError) Unwrap() error { return e.cause }

func (m *RunManager) unresolvedPostWorktreeRun(cause error) error {
	// Set the admission guard before returning to lifecycle cleanup so no new
	// run can be admitted after this run loses its durable terminal state.
	m.closeRunAdmission()
	return &unresolvedPostWorktreeRunError{cause: cause}
}

func (m *RunManager) parkPostWorktreeFailure(ctx context.Context, run *db.Run, repo *db.Repo, hookErr error) error {
	timeout := m.postWorktreeTerminalizationTimeout
	if timeout <= 0 {
		timeout = postWorktreeTerminalizationTimeout
	}
	return m.parkPostWorktreeFailureWithTerminalizationTimeout(ctx, run, repo, hookErr, timeout)
}

func (m *RunManager) parkPostWorktreeFailureWithTerminalizationTimeout(ctx context.Context, run *db.Run, repo *db.Repo, hookErr error, terminalizationTimeout time.Duration) error {
	errMsg := hookErr.Error()
	if err := m.db.ParkRunForEnvironmentFailure(run.ID, errMsg); err != nil {
		failureMessage := fmt.Sprintf("park post-worktree hook failure: %v", err)
		if dbErr := m.db.FailRunAfterEnvironmentParkFailure(run.ID, failureMessage); dbErr != nil {
			return m.unresolvedPostWorktreeRun(errors.Join(errors.New(failureMessage), fmt.Errorf("persist failed post-worktree hook run: %w", dbErr)))
		}
		persisted, confirmErr := m.confirmPostWorktreeTerminalState(run.ID, failureMessage, types.RunFailed)
		if confirmErr != nil {
			return m.unresolvedPostWorktreeRun(fmt.Errorf("verify failed post-worktree hook fallback: %w", confirmErr))
		}
		run.Status = persisted.Status
		run.Error = persisted.Error
		status := string(run.Status)
		m.broadcast(ipc.Event{Type: ipc.EventRunCompleted, RunID: run.ID, RepoID: repo.ID, Status: &status, Branch: &run.Branch, Error: run.Error})
		return errors.New(failureMessage)
	}
	run.Status = types.RunRunning
	run.Error = &errMsg
	status := string(run.Status)
	m.broadcast(ipc.Event{
		Type:   ipc.EventRunUpdated,
		RunID:  run.ID,
		RepoID: repo.ID,
		Status: &status,
		Branch: &run.Branch,
		Error:  run.Error,
	})

	parkedAt := time.Now()
	<-ctx.Done()
	terminalCause := context.Cause(ctx)
	if terminalCause == nil {
		terminalCause = errors.New("post-worktree hook park ended")
	}
	terminalMessage := terminalCause.Error()
	terminalStatus := types.RunFailed
	if terminalMessage == types.RunCancelReasonAbortedByUser || terminalMessage == types.RunCancelReasonSuperseded {
		terminalStatus = types.RunCancelled
	}
	terminalizationCtx, cancelTerminalization := context.WithTimeout(context.WithoutCancel(ctx), terminalizationTimeout)
	defer cancelTerminalization()
	terminalizationFailed := false
	var lastTerminalizationErr error
	var persisted *db.Run
	for persisted == nil {
		if err := m.db.TerminalizeAwaitingRun(run.ID, terminalMessage, terminalStatus, time.Since(parkedAt).Milliseconds()); err == nil {
			var verifyErr error
			persisted, verifyErr = m.confirmPostWorktreeTerminalState(run.ID, terminalMessage, terminalStatus)
			if verifyErr != nil {
				fallback, fallbackErr := m.fallbackPostWorktreeTerminalState(run.ID, terminalMessage, terminalStatus)
				if fallbackErr != nil {
					return m.unresolvedPostWorktreeRun(errors.Join(
						terminalCause,
						fmt.Errorf("verify primary post-worktree terminalization: %w", verifyErr),
						fallbackErr,
					))
				}
				persisted = fallback
				slog.Warn("post-worktree hook terminalization recovered through fallback write", "run_id", run.ID, "error", verifyErr)
			}
		} else {
			lastTerminalizationErr = err
			if !terminalizationFailed {
				slog.Error("failed to finish post-worktree hook park; retaining run ownership until terminal state persists", "run_id", run.ID, "error", err)
				terminalizationFailed = true
			}
		}
		if persisted != nil {
			break
		}

		retryTimer := time.NewTimer(postWorktreeTerminalizeRetryInterval)
		select {
		case <-terminalizationCtx.Done():
			if !retryTimer.Stop() {
				select {
				case <-retryTimer.C:
				default:
				}
			}
			terminalizationErr := errors.Join(
				terminalCause,
				lastTerminalizationErr,
				fmt.Errorf("post-worktree hook terminalization retry budget exhausted: %w", terminalizationCtx.Err()),
			)
			fallback, fallbackErr := m.fallbackPostWorktreeTerminalState(run.ID, terminalMessage, terminalStatus)
			if fallbackErr != nil {
				return m.unresolvedPostWorktreeRun(errors.Join(
					terminalizationErr,
					fallbackErr,
				))
			}
			slog.Warn("post-worktree hook terminalization recovered through fallback write", "run_id", run.ID, "error", terminalizationErr)
			persisted = fallback
		case <-retryTimer.C:
		}
	}
	run.Status = persisted.Status
	run.Error = persisted.Error
	status = string(run.Status)
	m.broadcast(ipc.Event{
		Type:   ipc.EventRunCompleted,
		RunID:  run.ID,
		RepoID: repo.ID,
		Status: &status,
		Branch: &run.Branch,
		Error:  run.Error,
	})
	return terminalCause
}

func (m *RunManager) fallbackPostWorktreeTerminalState(runID, errMsg string, status types.RunStatus) (*db.Run, error) {
	// RecoverStaleRun always marks the run failed and mutates step state. This
	// failure happens before the first step and may be an operator cancellation,
	// so UpdateRunErrorStatus is the one-shot atomic fallback that preserves the
	// intended terminal status while clearing the park.
	if err := m.db.UpdateRunErrorStatus(runID, errMsg, status); err != nil {
		return nil, fmt.Errorf("persist post-worktree hook terminalization fallback: %w", err)
	}
	confirmed, err := m.confirmPostWorktreeTerminalState(runID, errMsg, status)
	if err != nil {
		return nil, fmt.Errorf("verify post-worktree hook terminalization fallback: %w", err)
	}
	return confirmed, nil
}

func (m *RunManager) confirmPostWorktreeTerminalState(runID, errMsg string, status types.RunStatus) (*db.Run, error) {
	run, err := m.db.GetRun(runID)
	if err != nil {
		return nil, fmt.Errorf("read post-worktree terminal state: %w", err)
	}
	if run == nil {
		return nil, errors.New("post-worktree terminal state is missing")
	}
	if run.Status != status {
		return nil, fmt.Errorf("post-worktree terminal status = %s, want %s", run.Status, status)
	}
	if run.AwaitingAgentSince != nil {
		return nil, errors.New("post-worktree terminal state remains parked")
	}
	if run.Error == nil || *run.Error != errMsg {
		return nil, fmt.Errorf("post-worktree terminal error = %v, want %q", run.Error, errMsg)
	}
	return run, nil
}
