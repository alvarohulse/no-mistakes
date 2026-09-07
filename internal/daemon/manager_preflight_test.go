package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type cancellationBlockedStep struct {
	name      types.StepName
	started   chan struct{}
	cancelled chan struct{}
	release   <-chan struct{}
}

func (s *cancellationBlockedStep) Name() types.StepName { return s.name }

func (s *cancellationBlockedStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	close(s.started)
	<-sctx.Ctx.Done()
	close(s.cancelled)
	<-s.release
	return nil, context.Cause(sctx.Ctx)
}

func TestPreflightRunsInOrderBeforeRunCreation(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "preflight-success")
	order := filepath.Join(t.TempDir(), "order")
	head := writePreflightPolicyCommit(t, repo, marker, []string{
		"printf 'one\\n' >> " + shellQuoteForTest(order),
		"test \"$(cat " + shellQuoteForTest(order) + ")\" = one && printf 'two\\n' >> " + shellQuoteForTest(order),
	})
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "preflight success", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}
	content, err := os.ReadFile(order)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "one\ntwo\n" {
		t.Fatalf("preflight order = %q", content)
	}
}

func TestPreflightNonzeroRedactsBoundsAndCreatesNoRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "preflight-nonzero")
	command := `printf 'authorization: Bearer abcdefghijklmnopqrstuvwxyz\n'; yes x | head -c 100000; exit 7`
	head := writePreflightPolicyCommit(t, repo, marker, []string{command})
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	_, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "preflight nonzero", "", "", "")
	assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "preflight command 1 exited with code 7")
	if strings.Contains(err.Error(), "abcdefghijklmnopqrstuvwxyz") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("preflight error was not redacted: %q", err)
	}
	if len(err.Error()) > preflightOutputLimit+1024 || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("preflight error was not bounded with truncation: %d bytes", len(err.Error()))
	}
}

func TestPreflightTimeoutCreatesNoRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "preflight-timeout")
	head := writePreflightPolicyCommit(t, repo, marker, []string{"echo ready"})
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	manager.executePreflight = func(context.Context, runner.Prepared, runner.ExecuteOptions) (runner.Result, error) {
		return runner.Result{}, runner.ErrTimeout
	}
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	_, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "preflight timeout", "", "", "")
	assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "preflight command 1 timed out")
}

func TestPreflightLaunchErrorCreatesNoRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "preflight-launch")
	head := writePreflightPolicyCommit(t, repo, marker, []string{"echo ready"})
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	manager.executePreflight = func(context.Context, runner.Prepared, runner.ExecuteOptions) (runner.Result, error) {
		return runner.Result{}, errors.New("launch runner: authorization: Bearer abcdefghijklmnopqrstuvwxyz")
	}
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	_, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "preflight launch", "", "", "")
	assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "preflight command 1 launch failed")
	if strings.Contains(err.Error(), "abcdefghijklmnopqrstuvwxyz") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("launch error was not redacted: %q", err)
	}
}

func TestPreflightInvalidInactiveRunnerCreatesNoRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "preflight-invalid-runner")
	configYAML := validPolicyResolutionConfig(marker) + `preflight:
  - run: echo ready
    windows:
      runner:
        executable: C:\private\pwsh.exe
        args: [-NoLogo, -NoProfile, -NonInteractive, -Command]
`
	writePolicyConfigCommit(t, repo, configYAML, "configure invalid preflight", "refs/heads/main")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	_, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "invalid runner", "", "", "")
	assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "windows runner")
}

func TestPreflightFailureDoesNotSupersedeActiveRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, _ := newPolicyResolutionFixture(t, "preflight-preserve-active")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	started := make(chan struct{})
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{&mockSlowStep{name: types.StepReview, started: started}}
	})
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	activeID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "active run", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	global := "overrides:\n  test/repo:\n    preflight:\n      - exit 9\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "replacement", "", "", ""); err == nil || !strings.Contains(err.Error(), "preflight command 1 exited with code 9") {
		t.Fatalf("replacement error = %v", err)
	}
	active, err := database.GetRun(activeID)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || active.Status != types.RunRunning {
		t.Fatalf("active run status = %v, want running", active)
	}
}

func TestStartRunQuarantineAfterAdmissionKeepsReplacement(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "admission-reservation")
	head := writePreflightPolicyCommit(t, repo, marker, []string{"echo ready"})
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	blocked := &cancellationBlockedStep{
		name:      types.StepReview,
		started:   started,
		cancelled: cancelled,
		release:   release,
	}
	launches := 0
	manager := NewRunManager(database, p, func() []pipeline.Step {
		if launches == 0 {
			launches++
			return []pipeline.Step{blocked}
		}
		launches++
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	t.Cleanup(manager.Shutdown)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	setSafeBareRepositoryExplicitForDaemonTest(t)

	activeID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "active run", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("active run did not start")
	}

	replacement := make(chan struct {
		runID string
		err   error
	}, 1)
	go func() {
		runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "replacement", "", "", "")
		replacement <- struct {
			runID string
			err   error
		}{runID, err}
	}()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("replacement did not supersede active run")
	}

	quarantined := make(chan error, 1)
	go func() {
		quarantined <- manager.unresolvedPostWorktreeRun(errors.New("unresolved post-worktree terminalization"))
	}()
	releaseOnce.Do(func() { close(release) })

	var result struct {
		runID string
		err   error
	}
	select {
	case result = <-replacement:
	case <-time.After(time.Second):
		t.Fatal("replacement admission did not complete")
	}
	if result.err != nil {
		t.Fatalf("replacement admission = %v, want success", result.err)
	}
	if result.runID == "" || result.runID == activeID {
		t.Fatalf("replacement run ID = %q, active run ID = %q", result.runID, activeID)
	}
	if run, err := database.GetRun(result.runID); err != nil || run == nil {
		t.Fatalf("replacement run = %v, %v", run, err)
	}
	select {
	case err := <-quarantined:
		var unresolvedErr *unresolvedPostWorktreeRunError
		if !errors.As(err, &unresolvedErr) {
			t.Fatalf("quarantine result = %T %v, want unresolved post-worktree error", err, err)
		}
	case <-time.After(time.Second):
		t.Fatal("quarantine did not complete after replacement admission")
	}
}

func TestStartRunAdmissionRejectsConcurrentClosure(t *testing.T) {
	for _, tt := range []struct {
		name  string
		close func(*RunManager)
	}{
		{
			name: "post-worktree quarantine",
			close: func(manager *RunManager) {
				_ = manager.unresolvedPostWorktreeRun(errors.New("unresolved post-worktree terminalization"))
			},
		},
		{
			name: "shutdown",
			close: func(manager *RunManager) {
				manager.Shutdown()
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NM_DEMO", "1")
			p, database, repo, marker := newPolicyResolutionFixture(t, "admission-"+strings.ReplaceAll(tt.name, " ", "-"))
			head := writePreflightPolicyCommit(t, repo, marker, []string{"echo ready"})
			step := &mockPassStep{name: types.StepReview}
			manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
			t.Cleanup(manager.Shutdown)
			setSafeBareRepositoryExplicitForDaemonTest(t)

			preflightStarted := make(chan struct{})
			releasePreflight := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releasePreflight) }) }
			t.Cleanup(release)
			manager.executePreflight = func(context.Context, runner.Prepared, runner.ExecuteOptions) (runner.Result, error) {
				close(preflightStarted)
				<-releasePreflight
				return runner.Result{}, nil
			}

			result := make(chan error, 1)
			go func() {
				_, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "concurrent admission", "", "", "")
				result <- err
			}()
			select {
			case <-preflightStarted:
			case <-time.After(time.Second):
				t.Fatal("run did not reach the paused preflight seam")
			}
			tt.close(manager)
			release()

			select {
			case err := <-result:
				assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "daemon is shutting down")
				assertNoEffectiveConfigArtifacts(t, p)
			case <-time.After(time.Second):
				t.Fatal("run admission did not return after closure")
			}
		})
	}
}

func writePreflightPolicyCommit(t *testing.T, repo *db.Repo, marker string, commands []string) string {
	t.Helper()
	var content strings.Builder
	content.WriteString(validPolicyResolutionConfig(marker))
	content.WriteString("preflight:\n")
	for _, command := range commands {
		content.WriteString("  - ")
		content.WriteString(yamlDoubleQuoted(command))
		content.WriteString("\n")
	}
	writePolicyConfigCommit(t, repo, content.String(), "configure preflight", "refs/heads/main")
	return gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
}
