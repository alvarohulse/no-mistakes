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
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"build passed","tested":["go build ./..."]}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.Findings == "" {
		t.Fatalf("outcome = %#v, want successful structured agent build", outcome)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	for _, clause := range []string{
		"detect and run the appropriate build or compile command",
		"Do NOT run tests",
		"Do NOT run linters",
		"Do NOT update documentation",
	} {
		if !strings.Contains(prompt, clause) {
			t.Errorf("prompt missing %q", clause)
		}
	}
}

func TestBuildStepWithoutCommandDoesNotPassWithoutExecutedBuild(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"nothing to build","tested":[]}`)}, nil
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
