package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_AutoFixTriggersWithoutApproval(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	// Config with auto-fix enabled for review (max 3 attempts)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings:      `{"findings":[{"severity":"error","description":"bug","action":"auto-fix"}],"summary":"1 issue"}`,
				}, nil
			}
			// After auto-fix, verify Fixing is set
			if !sctx.Fixing {
				t.Error("expected Fixing to be true on auto-fix re-execution")
			}
			if sctx.PreviousFindings == "" {
				t.Error("expected PreviousFindings to be set on auto-fix")
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected step called 2 times (initial + auto-fix), got %d", callCount)
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCompleted {
		t.Errorf("expected run status %q, got %q", types.RunCompleted, updated.Status)
	}
}

func TestExecutor_CommandSequenceRestartsForEachRound(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	zero := 0
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepLint,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			sctx.RecordCommand("first", &zero, nil)
			sctx.RecordCommand("second", &zero, nil)
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings:      `{"findings":[{"severity":"warning","description":"issue","action":"auto-fix"}],"summary":"lint issue"}`,
				}, nil
			}
			return &StepOutcome{}, nil
		},
	}

	workDir := t.TempDir()
	initGitRepo(t, workDir)
	executor := NewExecutor(database, paths, &config.Config{AutoFix: config.AutoFix{Lint: 1}}, nil, []Step{step}, nil)
	if err := executor.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := steps[0].Evidence()
	if err != nil {
		t.Fatal(err)
	}
	wantRounds := []int{1, 1, 2, 2}
	wantSequences := []int{1, 2, 1, 2}
	if len(evidence.Commands) != len(wantRounds) {
		t.Fatalf("commands = %+v", evidence.Commands)
	}
	for i, command := range evidence.Commands {
		if command.Round != wantRounds[i] || command.Sequence != wantSequences[i] {
			t.Fatalf("commands = %+v", evidence.Commands)
		}
	}
}

func TestStepContext_RecordCommandBoundsAndRedactsDisplayText(t *testing.T) {
	database, _, run, _ := setupTest(t)
	step, err := database.InsertStepResult(run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	command := "curl https://user:secret@example.com/" + strings.Repeat("🧪", maxCommandEvidenceBytes)
	sctx := &StepContext{DB: database, StepResultID: step.ID, Round: 1}
	zero := 0
	sctx.RecordCommand(command, &zero, nil)
	stored, err := database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := stored.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Commands) != 1 {
		t.Fatalf("commands = %+v", evidence.Commands)
	}
	display := evidence.Commands[0].Command
	if len(display) > maxCommandEvidenceBytes {
		t.Fatalf("display command = %d bytes, want <= %d", len(display), maxCommandEvidenceBytes)
	}
	if !utf8.ValidString(display) {
		t.Fatal("display command is not valid UTF-8")
	}
	if strings.Contains(display, "secret") || !strings.Contains(display, "redacted@example.com") {
		t.Fatalf("display command was not redacted: %q", display)
	}
	if !strings.Contains(display, "command truncated") {
		t.Fatalf("display command lacks truncation marker: %q", display)
	}
}

func TestStepContext_RecordResolvedCommandPersistsRunnerProvenance(t *testing.T) {
	database, _, run, _ := setupTest(t)
	step, err := database.InsertStepResult(run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	version := "5.2.26"
	resolved := runner.Resolved{
		Script:        "make build-linux",
		CommandSource: runner.SourceLinux,
		Provenance: runner.Provenance{
			SchemaVersion: runner.SchemaVersion,
			Platform:      "linux",
			Source:        runner.SourceDefault,
			Executable:    "zsh",
			Args:          []string{"-lc"},
			Version:       &version,
		},
	}
	sctx := &StepContext{DB: database, StepResultID: step.ID, Round: 1}
	zero := 0
	sctx.RecordResolvedCommandAtSequence(resolved, sctx.NextCommandSequence(), &zero, nil)

	stored, err := database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := stored.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Commands) != 1 {
		t.Fatalf("commands = %+v", evidence.Commands)
	}
	command := evidence.Commands[0]
	if command.Command != resolved.Script || command.CommandSource != runner.SourceLinux || command.Runner == nil || command.Runner.Executable != "zsh" || command.Runner.Version == nil || *command.Runner.Version != version {
		t.Fatalf("command evidence = %+v", command)
	}
}

func TestExecutor_PersistsEffectiveAutoFixLimit(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 2}}

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(steps))
	}
	if steps[0].AutoFixLimit == nil || *steps[0].AutoFixLimit != 2 {
		t.Fatalf("auto-fix limit = %v, want 2", steps[0].AutoFixLimit)
	}
}

func TestExecutor_AutoFixRespectsMaxAttempts(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	// Config with auto-fix limited to 2 attempts for lint
	cfg := &config.Config{AutoFix: config.AutoFix{Lint: 2}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepLint,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount > 1 {
				writeTestFile(t, workDir, "lint-fix.txt", fmt.Sprintf("progress %d\n", callCount))
			}
			// Always return NeedsApproval to exhaust auto-fix attempts
			return &StepOutcome{
				NeedsApproval: true,
				AutoFixable:   true,
				Findings:      failureJSON(fmt.Sprintf("style issue %d", callCount)),
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	// After 2 auto-fix attempts fail, should fall back to manual approval
	// 1 initial + 2 auto-fix = 3 calls, then waits for approval
	// Status is fix_review since auto-fix cycles ran (sctx.Fixing was true)
	waitForStepStatus(t, database, run.ID, types.StepLint, types.StepStatusFixReview)

	if callCount != 3 {
		t.Errorf("expected 3 calls (1 initial + 2 auto-fix), got %d", callCount)
	}

	// Now approve manually to finish
	exec.Respond(types.StepLint, types.ActionApprove, nil)
	waitExecutorDone(t, done)
}

func TestExecutor_AutoFixDisabledWithZero(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Config with auto-fix disabled for review
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 0}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"error","description":"bug","action":"auto-fix"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	// Should immediately wait for approval (no auto-fix)
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if callCount != 1 {
		t.Errorf("expected 1 call (no auto-fix), got %d", callCount)
	}

	exec.Respond(types.StepReview, types.ActionApprove, nil)
	waitExecutorDone(t, done)
}

func TestExecutor_AutoFixNilConfigUsesDefaults(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// nil config - executor should not panic and should use no auto-fix (backwards compat)
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"error","description":"bug","action":"auto-fix"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	// With nil config, should wait for approval
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if callCount != 1 {
		t.Errorf("expected 1 call (nil config, no auto-fix), got %d", callCount)
	}

	exec.Respond(types.StepReview, types.ActionAbort, nil)
	<-done
}

func TestExecutor_AutoFixEmitsEvents(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	cfg := &config.Config{AutoFix: config.AutoFix{Lint: 1}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepLint,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings:      `{"findings":[{"severity":"warning","description":"issue","action":"auto-fix"}],"summary":"1 issue"}`,
				}, nil
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	events := collectEvents(exec)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Should have a fixing status event from auto-fix
	fixingEvent := events.findLast(ipc.EventStepCompleted, string(types.StepStatusFixing))
	if fixingEvent == nil {
		t.Error("expected step_completed event with fixing status during auto-fix")
	}
}

func TestExecutor_DoesNotAutoFixManualApprovalOutcome(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	cfg := &config.Config{AutoFix: config.AutoFix{Test: 3}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"info","description":"new test file written by agent: agent_test.go","action":"no-op"}],"summary":"tests passed, but agent wrote new test files"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingApproval)

	if callCount != 1 {
		t.Fatalf("expected 1 call for manual approval outcome, got %d", callCount)
	}

	if err := exec.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
		t.Fatalf("respond error: %v", err)
	}
	waitExecutorDone(t, done)
}

func TestExecutor_AutoFixInfoFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				// Info findings that are auto-fixable (not blocking, but fixable)
				return &StepOutcome{
					NeedsApproval: false,
					AutoFixable:   true,
					Findings:      `{"findings":[{"severity":"info","description":"could simplify","action":"auto-fix"}],"summary":"1 suggestion"}`,
				}, nil
			}
			// After auto-fix, step passes clean
			if !sctx.Fixing {
				t.Error("expected Fixing to be true on auto-fix re-execution")
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 calls (initial + auto-fix), got %d", callCount)
	}
}

func TestExecutor_AutoFixSkipsHumanReviewFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			// All findings are ask-user - auto-fix should not trigger
			return &StepOutcome{
				NeedsApproval: true,
				AutoFixable:   true,
				Findings:      `{"findings":[{"severity":"warning","description":"design choice","action":"ask-user"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	// Should go straight to user approval without auto-fix
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if callCount != 1 {
		t.Fatalf("expected 1 call (no auto-fix for ask-user findings), got %d", callCount)
	}

	exec.Respond(types.StepReview, types.ActionApprove, nil)
	waitExecutorDone(t, done)
}

func TestExecutor_HumanReviewFindingsRequireApprovalWithoutNeedsApprovalFlag(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: false,
				AutoFixable:   true,
				Findings:      `{"findings":[{"severity":"info","description":"design choice","action":"ask-user"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Review: 3}}, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	exec.Respond(types.StepReview, types.ActionApprove, nil)
	waitExecutorDone(t, done)
}

func TestExecutor_AutoFixMixedFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				// Mix: one auto-fixable, one ask-user
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings: `{"findings":[
						{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"},
						{"id":"review-2","severity":"warning","description":"design choice","action":"ask-user"}
					],"summary":"2 issues","risk_level":"medium","risk_rationale":"mixed"}`,
				}, nil
			}
			// After auto-fix: verify only fixable finding was sent
			if sctx.PreviousFindings == "" {
				t.Error("expected PreviousFindings")
			}
			parsed, _ := types.ParseFindingsJSON(sctx.PreviousFindings)
			if len(parsed.Items) != 1 {
				t.Errorf("expected 1 fixable finding passed to fix, got %d", len(parsed.Items))
			}
			if len(parsed.Items) > 0 && parsed.Items[0].Description != "bug" {
				t.Errorf("expected fixable finding 'bug', got %q", parsed.Items[0].Description)
			}
			// Return only the ask-user finding remaining
			return &StepOutcome{
				NeedsApproval: true,
				AutoFixable:   true,
				Findings:      `{"findings":[{"id":"review-2","severity":"warning","description":"design choice","action":"ask-user"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	// After auto-fixing the bug, only ask-user finding remains.
	// No more fixable findings, so falls through to user approval.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	if callCount != 2 {
		t.Errorf("expected 2 calls (initial + 1 auto-fix), got %d", callCount)
	}

	exec.Respond(types.StepReview, types.ActionApprove, nil)
	waitExecutorDone(t, done)
}

func TestExecutor_ParkedStepReleasesLogFileAfterCancel(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{AutoFix: config.AutoFix{Lint: 0}}
	step := &adaptiveCallStep{
		name: types.StepLint,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"warning","description":"style issue","action":"auto-fix"}],"summary":"lint issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, cancel := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepLint, types.StepStatusAwaitingApproval)
	logPath := filepath.Join(p.RunLogDir(run.ID), "lint.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("expected lint.log while parked: %v", err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("executor did not return after cancel")
	}
	// Windows refuses unlinkat on a handle the parked executor still holds
	// (run 31829193856). Cancel must close the log before cleanup.
	if err := os.Remove(logPath); err != nil {
		t.Fatalf("parked step must close lint.log on cancel so the worktree can be removed: %v", err)
	}
}

// Skip provenance has to be persisted where it is known. Every skipped step
// otherwise renders with one generic sentence that guesses at a cause, and a
// step named in the run's skip list, a step that skipped itself, and a step
// that never ran because the run ended are three different facts.
func TestExecutor_RecordsDistinctSkipProvenance(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	preSkipped := &adaptiveCallStep{
		name: types.StepDocument,
		fn: func(*StepContext) (*StepOutcome, error) {
			t.Fatal("pre-skipped step executed")
			return nil, nil
		},
	}
	selfSkipped := &adaptiveCallStep{
		name: types.StepLint,
		fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{Skipped: true, SkipReason: "Lint had nothing to check."}, nil
		},
	}
	terminal := &adaptiveCallStep{
		name: types.StepPush,
		fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{SkipRemaining: true}, nil
		},
	}
	trailing := &adaptiveCallStep{
		name: types.StepPR,
		fn: func(*StepContext) (*StepOutcome, error) {
			t.Fatal("trailing step executed after a terminal outcome")
			return nil, nil
		},
	}

	executor := NewExecutor(database, paths, &config.Config{}, nil, []Step{preSkipped, selfSkipped, terminal, trailing}, nil)
	executor.SetSkippedSteps([]types.StepName{types.StepDocument})
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	explanations := map[types.StepName]string{}
	for _, sr := range steps {
		evidence, err := sr.Evidence()
		if err != nil {
			t.Fatal(err)
		}
		explanations[sr.StepName] = evidence.Explanation
	}
	for step, want := range map[types.StepName]string{
		types.StepDocument: "skip list",
		types.StepLint:     "Lint had nothing to check.",
		types.StepPR:       "ended the run",
	} {
		if !strings.Contains(explanations[step], want) {
			t.Errorf("%s explanation = %q, want it to mention %q", step, explanations[step], want)
		}
	}
}
