package steps

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDocumentStepDoesNotPerformUnconfiguredLintWork(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"documentation current"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Prompts = config.PromptConfig{
		Shared:   "shared guidance",
		Document: "document guidance",
		Lint:     "lint guidance",
	}

	if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("document agent calls = %d, want 1", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "shared guidance") || !strings.Contains(prompt, "document guidance") {
		t.Fatalf("document prompt lost its configured guidance:\n%s", prompt)
	}
	if strings.Contains(prompt, "lint guidance") || strings.Contains(prompt, "Combined lint duty") {
		t.Fatalf("document prompt must not perform lint work:\n%s", prompt)
	}
}

func TestLintStepPlansAndExecutesCommandWhenUnconfigured(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("successful planned lint command must not park")
	}
	if len(ag.calls) != 1 || !strings.Contains(ag.calls[0].Prompt, "read-only command-planning pass") {
		t.Fatalf("lint planner calls/prompt = %d/%q", len(ag.calls), ag.calls[0].Prompt)
	}
}

func TestPipeline_DocumentAndLintPlanningAreSeparateAgentInvocations(t *testing.T) {
	workDir, baseSHA, headSHA := setupGitRepo(t)

	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", headSHA, baseSHA)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	calls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"documentation current"}`)}, nil
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}

	cfg := &config.Config{Agent: types.AgentClaude}
	exec := pipeline.NewExecutor(database, paths.WithRoot(t.TempDir()), cfg, ag, []pipeline.Step{&DocumentStep{}, &LintStep{}}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if calls != 2 {
		t.Fatalf("document+lint cost %d agent invocations, want document pass plus lint planning", calls)
	}
	if strings.Contains(ag.calls[0].Prompt, "lint") {
		t.Fatalf("document prompt must not perform lint work:\n%s", ag.calls[0].Prompt)
	}
	if !strings.Contains(ag.calls[1].Prompt, "read-only command-planning pass") {
		t.Fatalf("lint prompt must be read-only command planning:\n%s", ag.calls[1].Prompt)
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	for _, step := range steps {
		if step.Status != types.StepStatusCompleted {
			t.Fatalf("step %s = %s, want completed", step.StepName, step.Status)
		}
	}
}

func TestPipeline_ConfiguredLintCommandStaysFirstClassGate(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"noop"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "exit 3"})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("failing configured lint command must stay a first-class gate failure")
	}
	if !outcome.AutoFixable {
		t.Fatal("failing configured lint command must stay auto-fixable")
	}
	if outcome.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", outcome.ExitCode)
	}
}
