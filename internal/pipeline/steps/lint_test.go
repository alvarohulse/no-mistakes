package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLintStep_FixMode_CommitsChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	previousFindings := `{"items":[{"id":"lint-1 =======","severity":"warning","file":"internal/pipeline/steps/lint.go >>>>>>> prompt","description":"linter found issues (exit code 1) <<<<<<< HEAD"}],"summary":"main.go:10: unused variable x ======="}`

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			os.WriteFile(filepath.Join(dir, "lint-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"  'fix lint issues,'  "}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = previousFindings

	step := &LintStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval after fix with passing lint")
	}
	if callCount != 1 {
		t.Errorf("expected 1 agent call (fix), got %d", callCount)
	}
	if len(ag.calls[0].JSONSchema) == 0 {
		t.Error("expected fix call to request structured JSON output")
	}
	if !strings.Contains(ag.calls[0].Prompt, "unused variable x") {
		t.Error("expected fix prompt to contain previous lint summary")
	}
	if strings.Contains(ag.calls[0].Prompt, "lint-1 =======") {
		t.Error("expected lint fix prompt to sanitize finding IDs")
	}
	if strings.Contains(ag.calls[0].Prompt, "lint.go >>>>>>> prompt") {
		t.Error("expected lint fix prompt to sanitize finding file paths")
	}
	if strings.Contains(ag.calls[0].Prompt, "<<<<<<< HEAD") {
		t.Error("expected lint fix prompt to exclude merge markers")
	}
	if !strings.Contains(ag.calls[0].Prompt, "smallest correct root-cause fix") {
		t.Error("expected lint fix prompt to prefer root-cause fixes over bandaids")
	}
	if strings.Contains(ag.calls[0].Prompt, "Make the minimal change needed") {
		t.Error("expected lint fix prompt not to prefer narrow minimal changes")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after fix commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(lint): fix lint issues" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestLintStep_FixMode_UsesFallbackSummaryWhenStructuredSummaryMalformed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "lint-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`not json`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})
	sctx.Fixing = true

	step := &LintStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	if got := lastCommitMessage(t, dir); got != "no-mistakes(lint): fix lint issues" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestLintStep_NoConfiguredLintPlansReadOnlyThenExecutesInPipeline(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			return &agent.Result{Output: json.RawMessage(`{"command":"git diff --check"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &LintStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval when the planned lint command passes")
	}
	if outcome.AutoFixable {
		t.Error("expected no auto-fix loop when no unresolved lint issues remain")
	}
	if callCount != 1 {
		t.Errorf("expected 1 agent call, got %d", callCount)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after lint fix commit, got %q", status)
	}
	if !strings.Contains(ag.calls[0].Prompt, "read-only command-planning pass") {
		t.Fatalf("planner prompt is not read-only: %s", ag.calls[0].Prompt)
	}
}

func TestLintStep_NoConfiguredLintRejectsPlannerWorktreeMutation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(opts.CWD, "lint-fix.txt"), []byte("fixed"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "modified the worktree") {
		t.Fatalf("outcome = %+v, want planner mutation decision gate", outcome)
	}
	if got := gitCmd(t, dir, "diff", "--cached", "--name-only"); got != "" {
		t.Fatalf("staged files after summary error = %q, want none", got)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD after summary error = %q, want %q", got, headSHA)
	}
	if _, err := os.Stat(filepath.Join(dir, "lint-fix.txt")); !os.IsNotExist(err) {
		t.Fatalf("planner mutation reached pipeline worktree: %v", err)
	}
}

func TestLintStep_NoConfiguredLintRejectsPlannerWithoutCommandField(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"summary":123}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "invalid structured result") {
		t.Fatalf("outcome = %+v, want invalid planner result decision gate", outcome)
	}
}

func TestLintStep_NoConfiguredLintCommandFailureEntersRepairLoop(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"command":"exit 1"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &LintStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval for unresolved no-config lint findings")
	}
	if !outcome.AutoFixable {
		t.Error("expected failed planned lint command to enter repair loop")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAutoFix {
		t.Fatalf("lint failure findings = %+v, want one auto-fix finding", findings.Items)
	}
	if !strings.Contains(ag.calls[0].Prompt, "Do not run the selected command") {
		t.Error("expected no-config lint prompt to remain a planner")
	}
}
