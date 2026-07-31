package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunStartExecutesPostWorktreeHookBeforeFirstStep(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "post-worktree-success")
	head := commitPostWorktreeHook(t, repo, "printf 'ready\\n' >> post-worktree.marker")

	step := &assertPostWorktreeEffectStep{check: func(workDir string) error {
		data, err := os.ReadFile(filepath.Join(workDir, "post-worktree.marker"))
		if err != nil {
			return err
		}
		if string(data) != "ready\n" {
			return &unexpectedHookEffectError{got: string(data)}
		}
		return nil
	}}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "post-worktree hook", "")
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
	head := commitPostWorktreeHook(t, repo, "printf 'authenticate first\\n'; exit 23")

	step := &mockPassStep{name: types.StepIntent}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "post-worktree hook", "")
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
