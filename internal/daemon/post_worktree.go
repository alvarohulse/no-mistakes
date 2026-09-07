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

func (m *RunManager) parkPostWorktreeFailure(ctx context.Context, run *db.Run, repo *db.Repo, hookErr error) error {
	errMsg := hookErr.Error()
	if err := m.db.ParkRunForEnvironmentFailure(run.ID, errMsg); err != nil {
		failureMessage := fmt.Sprintf("park post-worktree hook failure: %v", err)
		if dbErr := m.db.FailRunAfterEnvironmentParkFailure(run.ID, failureMessage); dbErr != nil {
			return errors.Join(errors.New(failureMessage), fmt.Errorf("persist failed post-worktree hook run: %w", dbErr))
		}
		run.Status = types.RunFailed
		run.Error = &failureMessage
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
	terminalMessage := "post-worktree hook park ended"
	if cause := context.Cause(ctx); cause != nil {
		terminalMessage = cause.Error()
	}
	terminalStatus := types.RunFailed
	if terminalMessage == types.RunCancelReasonAbortedByUser || terminalMessage == types.RunCancelReasonSuperseded {
		terminalStatus = types.RunCancelled
	}
	if err := m.db.TerminalizeAwaitingRun(run.ID, terminalMessage, terminalStatus, time.Since(parkedAt).Milliseconds()); err != nil {
		slog.Error("failed to finish post-worktree hook park", "run_id", run.ID, "error", err)
		return errors.Join(errors.New(terminalMessage), fmt.Errorf("persist terminal post-worktree hook park: %w", err))
	}
	run.Status = terminalStatus
	run.Error = &terminalMessage
	status = string(run.Status)
	m.broadcast(ipc.Event{
		Type:   ipc.EventRunCompleted,
		RunID:  run.ID,
		RepoID: repo.ID,
		Status: &status,
		Branch: &run.Branch,
		Error:  run.Error,
	})
	return errors.New(terminalMessage)
}
