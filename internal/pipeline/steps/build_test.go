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

func TestBuildStepConfiguredCommandRunsInWorktree(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	marker := filepath.Join(dir, "build-ran")
	command := "go env GOVERSION > build-ran"
	sctx := newTestContext(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{Build: command})
	var logs []string
	sctx.Log = func(message string) { logs = append(logs, message) }

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %#v, want successful build", outcome)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); !strings.HasPrefix(got, "go") {
		t.Fatalf("build marker = %q, want Go version", got)
	}
	if joined := strings.Join(logs, "\n"); !strings.Contains(joined, "running build: "+command) {
		t.Fatalf("logs = %q, want visible configured command", joined)
	}
	if (&BuildStep{}).Name() != types.StepBuild {
		t.Fatalf("step name = %q, want %q", (&BuildStep{}).Name(), types.StepBuild)
	}
}

func TestBuildStepConfiguredCommandFailureIsActionable(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	command := "go build ./definitely-missing-package"
	sctx := newTestContext(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{Build: command})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable || outcome.ExitCode == 0 {
		t.Fatalf("outcome = %#v, want auto-fixable build failure", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAutoFix {
		t.Fatalf("findings = %#v, want one auto-fix finding", findings.Items)
	}
	if !strings.Contains(findings.Summary, "go:") {
		t.Fatalf("summary = %q, want captured compiler output", findings.Summary)
	}
}

func TestBuildStepWithoutCommandAsksAgentToCompile(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"command":"go env GOVERSION"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	sctx.Round = 1
	sctx.RoundID = round.ID
	sctx.RoundTrigger = "initial"

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %#v, want successful pipeline-executed build", outcome)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	for _, clause := range []string{
		"return the exact build or compile command",
		"Do not run the selected command",
		"Do NOT run tests",
		"Do NOT run linters",
		"Do NOT update documentation",
	} {
		if !strings.Contains(prompt, clause) {
			t.Errorf("prompt missing %q", clause)
		}
	}
	definitions, err := sctx.DB.GetCommandDefinitionsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 || definitions[0].Script != "go env GOVERSION" || definitions[0].Source != "planned" {
		t.Fatalf("planned command definitions = %+v", definitions)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].CommandID != definitions[0].ID || attempts[0].RoundID != round.ID {
		t.Fatalf("planned command attempts = %+v", attempts)
	}

	if err := os.WriteFile(filepath.Join(dir, "repair.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "repair.txt")
	gitCmd(t, dir, "commit", "-m", "fix: simulate repair")
	repairedHead := gitCmd(t, dir, "rev-parse", "HEAD")
	secondRound, err := sctx.DB.InsertStepRound(step.ID, 2, "auto_fix", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Round = 2
	sctx.RoundID = secondRound.ID
	sctx.RoundTrigger = "auto_fix"
	if _, err := (&BuildStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	definitions, err = sctx.DB.GetCommandDefinitionsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err = sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 || len(attempts) != 2 {
		t.Fatalf("reused plan definitions/attempts = %d/%d, want 1/2", len(definitions), len(attempts))
	}
	if attempts[1].CommandID != definitions[0].ID || attempts[1].TestedSHA == nil || *attempts[1].TestedSHA != repairedHead || attempts[1].RetryOfAttemptID != nil {
		t.Fatalf("post-repair attempt = %+v", attempts[1])
	}
	if len(ag.calls) != 1 {
		t.Fatalf("planner calls after repair = %d, want original plan reused", len(ag.calls))
	}
}

func TestBuildStepLinksUnchangedAfterRepairRetry(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"command":"exit 1"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	firstRound, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID, sctx.Round, sctx.RoundID, sctx.RoundTrigger = step.ID, 1, firstRound.ID, "initial"
	if _, err := (&BuildStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	secondRound, err := sctx.DB.InsertStepRound(step.ID, 2, "auto_fix", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Round, sctx.RoundID, sctx.RoundTrigger = 2, secondRound.ID, "auto_fix"
	if _, err := (&BuildStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[1].RetryOfAttemptID == nil ||
		*attempts[1].RetryOfAttemptID != attempts[0].ID ||
		attempts[1].RetryReason == nil ||
		*attempts[1].RetryReason != "unchanged_after_repair" {
		t.Fatalf("attempts = %+v, want linked unchanged-after-repair retry", attempts)
	}
}

func TestBuildStepWithoutCommandDoesNotPassWithoutExecutedBuild(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"command":""}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want non-auto-fixable decision gate", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("findings = %#v, want ask-user build-not-established finding", findings.Items)
	}
}

func TestBuildStepFixModeRepairsRootCauseThenRebuilds(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/buildtest\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath, []byte("package main\nfunc main() { syntax error }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{
		name: "builder",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(mainPath, []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix compile syntax"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{Build: "go build ./..."})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"error","description":"main.go does not compile","action":"auto-fix"}]}`

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 || outcome.FixSummary != "fix compile syntax" {
		t.Fatalf("outcome = %#v, want repaired successful build", outcome)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want one repair call", len(ag.calls))
	}
	for _, clause := range []string{"smallest build root-cause fix", "Do NOT run tests", "Do NOT run linters", "Do NOT update documentation"} {
		if !strings.Contains(ag.calls[0].Prompt, clause) {
			t.Errorf("fix prompt missing %q", clause)
		}
	}
}
