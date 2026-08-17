//go:build unix

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunStartPostWorktreeHookReapsLeakedGrandchild(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "post-worktree-process-group")
	head := commitPostWorktreeHook(t, repo, "sleep 300 & echo $! > post-worktree-child.pid")

	step := &assertPostWorktreeEffectStep{check: func(workDir string) error {
		data, err := os.ReadFile(filepath.Join(workDir, "post-worktree-child.pid"))
		if err != nil {
			return err
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return err
		}
		if pidGoneWithin(pid, 3*time.Second) {
			return nil
		}
		return errors.New("post-worktree grandchild remained alive after hook exit")
	}}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "post-worktree process group", "", "", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}
}
