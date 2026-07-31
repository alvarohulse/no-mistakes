package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const maxPostWorktreeErrorRunes = 4 * 1024

func runPostWorktreeHook(ctx context.Context, workDir string, cfg *config.Config) error {
	hook := strings.TrimSpace(cfg.Hooks.PostWorktree)
	if hook == "" {
		return nil
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd.exe", "/c", hook)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", hook)
	}
	cmd.Dir = workDir
	shellenv.ConfigureShellCommand(cmd, cfg.ProcessTerminationGrace)
	output, err := shellenv.CombinedOutputShellCommand(cmd)
	if err == nil {
		return nil
	}

	detail := boundedPostWorktreeOutput(output)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("post-worktree hook failed with exit code %d%s", exitErr.ExitCode(), detail)
	}
	return fmt.Errorf("post-worktree hook failed: %s%s", safeurl.RedactText(err.Error()), detail)
}

func boundedPostWorktreeOutput(output []byte) string {
	text := strings.TrimSpace(safeurl.RedactText(string(output)))
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > maxPostWorktreeErrorRunes {
		text = "…" + string(runes[len(runes)-maxPostWorktreeErrorRunes:])
	}
	return ": " + text
}

func (m *RunManager) parkPostWorktreeFailure(ctx context.Context, run *db.Run, repo *db.Repo, hookErr error) error {
	errMsg := hookErr.Error()
	if err := m.db.ParkRunForEnvironmentFailure(run.ID, errMsg); err != nil {
		failureMessage := fmt.Sprintf("park post-worktree hook failure: %v", err)
		run.Status = types.RunFailed
		run.Error = &failureMessage
		if dbErr := m.db.UpdateRunError(run.ID, failureMessage); dbErr != nil {
			slog.Error("failed to record post-worktree hook park failure", "run_id", run.ID, "error", dbErr)
		}
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
	if err := m.db.CompleteRunAwaitingAgent(run.ID, time.Since(parkedAt).Milliseconds()); err != nil {
		slog.Warn("failed to complete post-worktree hook park", "run_id", run.ID, "error", err)
	}
	if err := m.db.UpdateRunErrorStatus(run.ID, terminalMessage, terminalStatus); err != nil {
		slog.Error("failed to finish post-worktree hook park", "run_id", run.ID, "error", err)
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
