package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func setupAgentGoBuildRepo(t *testing.T) (string, string, string) {
	t.Helper()
	dir, baseSHA, _ := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/buildprobe\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "buildprobe.go"), []byte("package buildprobe\n\nfunc Build() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "go.mod", "buildprobe.go")
	gitCmd(t, dir, "commit", "-m", "add build probe")
	return dir, baseSHA, gitCmd(t, dir, "rev-parse", "HEAD")
}

func TestBuildStepRunsAgentSelectedCommand(t *testing.T) {
	dir, baseSHA, headSHA := setupAgentGoBuildRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			for _, required := range []string{"Do NOT execute commands", "go build [safe flags] ./...", "trusted commands.build"} {
				if !strings.Contains(opts.Prompt, required) {
					t.Fatalf("selection prompt missing %q:\n%s", required, opts.Prompt)
				}
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"selected Go compile probe","build_command":"go build -v ./..."}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	var logs []string
	sctx.Log = func(message string) { logs = append(logs, message) }

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %#v, want successful build", outcome)
	}
	got := strings.Join(logs, "\n")
	if !strings.Contains(got, "agent selected build command: go build -v ./...") || !strings.Contains(got, "example.com/buildprobe") {
		t.Fatalf("build logs = %q, want selected command and its real output", got)
	}
}

func TestBuildStepExecutesValidatedTokensNotRawAgentText(t *testing.T) {
	dir, baseSHA, headSHA := setupAgentGoBuildRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"probe","build_command":"go  build\t-v   ./..."}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	var logs []string
	sctx.Log = func(message string) { logs = append(logs, message) }

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %#v, want successful build", outcome)
	}
	if got := strings.Join(logs, "\n"); !strings.Contains(got, "running build: go build -v ./...") {
		t.Fatalf("build logs = %q, want canonical validated command", got)
	}
}

func TestBuildStepWithoutCommandParksWhenAgentCannotSelectOne(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"no build metadata exists","action":"ask-user"}],"summary":"no build command","build_command":""}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want ask-user gate", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !types.HasAskUserFindings(findings) {
		t.Fatalf("findings = %#v, want ask-user finding", findings.Items)
	}
}

func TestBuildStepRefusesAgentSelectedShellExecution(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	marker := filepath.Join(dir, "shell-command-ran")
	payload, err := json.Marshal(buildPlan{
		Summary:      "selected shell",
		BuildCommand: fmt.Sprintf("sh -c touch$IFS%s", marker),
	})
	if err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{
		name: "builder",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want rejected command to require approval", outcome)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("agent-selected shell command executed: %v", err)
	}
}

func TestBuildStepParksWhenAutomaticGoBuildCannotCoverWorktree(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		content    string
		wantReason string
	}{
		{name: "workspace", path: "go.work", content: "go 1.25\n\nuse .\n", wantReason: "go.work"},
		{name: "nested module", path: "svc/go.mod", content: "module example.com/buildprobe/svc\n\ngo 1.25\n", wantReason: "svc/go.mod"},
		{name: "quoted nested module", path: "sérvice/go.mod", content: "module example.com/buildprobe/svc\n\ngo 1.25\n", wantReason: "sérvice/go.mod"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, baseSHA, _ := setupAgentGoBuildRepo(t)
			path := filepath.Join(dir, tt.path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", tt.path)
			gitCmd(t, dir, "commit", "-m", "add module layout")
			headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
			ag := &mockAgent{
				name: "builder",
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"selected Go build","build_command":"go build ./..."}`)}, nil
				},
			}
			sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

			outcome, err := (&BuildStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.NeedsApproval || outcome.AutoFixable {
				t.Fatalf("outcome = %#v, want ask-user park", outcome)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if !types.HasAskUserFindings(findings) || !strings.Contains(findings.Summary, tt.wantReason) {
				t.Fatalf("findings = %#v, want reason %q", findings, tt.wantReason)
			}
		})
	}
}

func TestBuildStepParksNonGoRepoForTrustedCommandsBuild(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"selected Go build","build_command":"go build ./..."}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want ask-user park for a non-Go repo", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !types.HasAskUserFindings(findings) || !strings.Contains(findings.Summary, "no root Go module") {
		t.Fatalf("findings = %#v, want no-root-Go-module park reason", findings)
	}
}

func TestParseAgentBuildCommandRejectsUnsafeOrPartialCommands(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{name: "project runner", command: "make build", want: "restricted automatic form"},
		{name: "execution hook", command: "go build -toolexec=helper ./...", want: "not compile-safe"},
		{name: "partial target", command: "go build ./cmd/example", want: "cover the full module"},
		{name: "shell syntax", command: "go build ./... && touch marker", want: "target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseAgentBuildCommand(tt.command)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("parseAgentBuildCommand(%q) error = %v, want %q", tt.command, err, tt.want)
			}
		})
	}
}

func TestBuildStepRefusesAgentSelectionSideEffects(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(opts.CWD, "agent-output.txt"), []byte("unexpected\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"selected command","build_command":"go build ./..."}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	_, err := (&BuildStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "side-effect free") || !strings.Contains(err.Error(), "agent-output.txt") {
		t.Fatalf("Execute() error = %v, want agent-side-effect refusal", err)
	}
}

func TestBuildStepRunsAgentSelectedCommandInStepEnvironment(t *testing.T) {
	dir, baseSHA, headSHA := setupAgentGoBuildRepo(t)
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "go")
	ag := &mockAgent{
		name: "builder",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"selected Go build","build_command":"go build ./..."}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeCLIEnv(binDir, map[string]string{
		"BUILD_STEP_ENV": "present",
		"FAKE_CLI_MODE":  "build-env",
	})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %#v, want successful build with step environment", outcome)
	}
}

func TestBuildStepConfiguredCommandRunsWithoutAgent(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	command := "go env GOVERSION"
	ag := &mockAgent{name: "unused"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{Build: command})
	var logs []string
	sctx.Log = func(message string) { logs = append(logs, message) }

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %#v, want successful build", outcome)
	}
	if got := strings.Join(logs, "\n"); !strings.Contains(got, "running build: "+command) || !strings.Contains(got, "go1.") {
		t.Fatalf("build logs = %q, want command and Go version", got)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("agent calls = %d, want 0", len(ag.calls))
	}
}

func TestBuildStepConfiguredCommandRefusesSideEffectsEvenOnFailure(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{
		Build: "go env GOVERSION > build-output.txt && exit 17",
	})

	_, err := (&BuildStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "exit code 17") || !strings.Contains(err.Error(), "side-effect free") || !strings.Contains(err.Error(), "build-output.txt") {
		t.Fatalf("Execute() error = %v, want build failure plus side-effect refusal", err)
	}
}

func TestBuildStepConfiguredCommandAllowsIgnoredOutput(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("build-output.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".gitignore")
	gitCmd(t, dir, "commit", "-m", "ignore build output")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	sctx := newTestContext(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{
		Build: "go env GOVERSION > build-output.txt",
	})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %#v, want successful build with ignored output", outcome)
	}
	if _, err := os.Stat(filepath.Join(dir, "build-output.txt")); err != nil {
		t.Fatalf("ignored build output: %v", err)
	}
}

func TestBuildStepConfiguredCommandFailureIsActionable(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{
		Build: "go env GOVERSION && exit 17",
	})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable || outcome.ExitCode != 17 {
		t.Fatalf("outcome = %#v, want auto-fixable build failure", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAutoFix || !strings.Contains(findings.Summary, "go1.") {
		t.Fatalf("findings = %#v, want compiler output and auto-fix action", findings)
	}
}

func TestBuildStepFixModeRepairsThenRebuilds(t *testing.T) {
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
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(mainPath, []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix compile syntax"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Build: "go build -o .git/buildtest ./..."})
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
	repairedHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if sctx.Run.HeadSHA != repairedHead {
		t.Fatalf("run head = %s, want agent repair commit %s", sctx.Run.HeadSHA, repairedHead)
	}
	stored, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.HeadSHA != repairedHead {
		t.Fatalf("stored head = %s, want agent repair commit %s", stored.HeadSHA, repairedHead)
	}
}

func TestReviewStepRefusesUnrecordedCommitAfterBuild(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "unrecorded.go"), []byte("package unrecorded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "unrecorded.go")
	gitCmd(t, dir, "commit", "-m", "unrecorded forward commit")
	sctx := newTestContext(t, &mockAgent{name: "reviewer"}, dir, baseSHA, headSHA, config.Commands{})

	_, err := (&ReviewStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "does not match the pipeline's recorded head") {
		t.Fatalf("ReviewStep.Execute() error = %v, want unrecorded-head refusal", err)
	}
}
