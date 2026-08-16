package daemon

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestConfiguredCISkipCompletesWithSourceAndExplicitNoneClearsIt(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, _ := newPolicyResolutionFixture(t, "configured-ci-skip")
	global := "overrides:\n  test/repo:\n    pipeline:\n      skip_steps: [ci]\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	review := &mockPassStep{name: types.StepReview}
	ci := &mockPassStep{name: types.StepCI}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{review, ci} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "configured skip", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	ciResult := stepResultByName(t, steps, types.StepCI)
	if ciResult.Status != types.StepStatusSkipped || ciResult.SkipSource == nil || *ciResult.SkipSource != string(types.SkipSourceGlobalOverride) {
		t.Fatalf("ci result = %+v", ciResult)
	}
	if ci.execCnt.Load() != 0 {
		t.Fatalf("configured skipped CI executed %d times", ci.execCnt.Load())
	}
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ResolvedPolicy == nil || !strings.Contains(*run.ResolvedPolicy, `"name":"ci","status":"skipped","skip_source":"global-override"`) {
		t.Fatalf("resolved policy missing configured skip receipt: %v", run.ResolvedPolicy)
	}

	explicitNone := []types.StepName{}
	runID, err = manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", explicitNone, "clear skip", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("cleared run status = %s, error = %v", run.Status, run.Error)
	}
	steps, err = database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	ciResult = stepResultByName(t, steps, types.StepCI)
	if ciResult.Status != types.StepStatusCompleted || ciResult.SkipSource != nil {
		t.Fatalf("explicit-none CI result = %+v", ciResult)
	}
	if ci.execCnt.Load() != 1 {
		t.Fatalf("explicit none executed CI %d times, want 1", ci.execCnt.Load())
	}
}

func TestConfiguredReviewSkipIsRejectedWhilePushRemainsActive(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "configured-review-skip")
	global := "overrides:\n  test/repo:\n    pipeline:\n      skip_steps: [review]\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{step, &mockPassStep{name: types.StepPush}}
	})
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	_, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "unsafe skip", "", "", "")
	assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "cannot skip review while push remains enabled")
}

func TestExplicitReviewSkipIsRejectedUnlessPushIsAlsoSkipped(t *testing.T) {
	steps := []pipeline.Step{
		&mockPassStep{name: types.StepReview},
		&mockPassStep{name: types.StepPush},
	}
	requested := resolveRunStepSkips([]types.StepName{types.StepReview}, []types.StepName{types.StepCI})
	if err := validateRunStepSkips(requested, steps); err == nil || !strings.Contains(err.Error(), "cannot skip review") {
		t.Fatalf("review-only skip error = %v", err)
	}
	requested = resolveRunStepSkips([]types.StepName{types.StepReview, types.StepPush}, []types.StepName{types.StepCI})
	if err := validateRunStepSkips(requested, steps); err != nil {
		t.Fatalf("review+push skip rejected: %v", err)
	}
	for _, receipt := range requested {
		if receipt.Source != types.SkipSourceRunRequest {
			t.Fatalf("explicit skip source = %q", receipt.Source)
		}
	}
}

func stepResultByName(t *testing.T, steps []*db.StepResult, name types.StepName) *db.StepResult {
	t.Helper()
	for _, step := range steps {
		if step.StepName == name {
			return step
		}
	}
	t.Fatalf("step %s not found", name)
	return nil
}
