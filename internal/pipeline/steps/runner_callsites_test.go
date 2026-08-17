package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestBuildTestAndLintUseStructuredRunnerCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	tests := []struct {
		name string
		step pipeline.Step
	}{
		{name: "build", step: &BuildStep{}},
		{name: "test", step: &TestStep{}},
		{name: "lint", step: &LintStep{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			marker := filepath.Join(dir, test.name+"-runner")
			cfg, wantSource := structuredPlatformCommandConfig(t, test.name, marker)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, cfg.Commands)
			sctx.Config = cfg
			stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, test.step.Name())
			if err != nil {
				t.Fatal(err)
			}
			sctx.StepResultID = stepResult.ID
			sctx.Round = 1

			outcome, err := test.step.Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.NeedsApproval || outcome.ExitCode != 0 {
				t.Fatalf("outcome = %+v", outcome)
			}
			assertPlatformRunnerMarker(t, marker)
			assertFirstCommandRunnerReceipt(t, sctx, stepResult.ID, wantSource)
		})
	}
}

func TestPushFormatUsesStructuredRunnerCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	marker := filepath.Join(dir, "format-runner")
	cfg, wantSource := structuredPlatformCommandConfig(t, "format", marker)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, cfg.Commands)
	sctx.Config = cfg
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	recordReviewApproval(t, sctx, headSHA)
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID
	sctx.Round = 1

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	assertPlatformRunnerMarker(t, marker)
	if !fileAtRef(t, upstream, "refs/heads/feature", filepath.Base(marker)) {
		t.Fatal("formatted file was not committed and pushed")
	}
	body := gitCmd(t, dir, "log", "-1", "--pretty=%B")
	if body != "chore(format): apply configured formatting" {
		t.Fatalf("formatter commit body = %q", body)
	}
	if strings.Contains(body, "Co-authored-by:") || strings.Contains(body, "No-Mistakes-Model:") {
		t.Fatalf("formatter commit falsely carries agent attribution: %q", body)
	}
	assertFirstCommandRunnerReceipt(t, sctx, stepResult.ID, wantSource)
}

func structuredPlatformCommandConfig(t *testing.T, name, marker string) (*config.Config, string) {
	t.Helper()
	platform := runtime.GOOS
	wantSource := runner.SourceLinux
	if platform == "darwin" {
		platform = "macos"
		wantSource = runner.SourceMacOS
	}
	baseScript := "printf base > " + marker
	platformScript := "printf platform > " + marker
	repo, err := config.LoadRepoFromBytes([]byte(fmt.Sprintf(`commands:
  %s:
    run: %q
    %s:
      run: %q
      runner: {executable: zsh, args: [-lc]}
`, name, baseScript, platform, platformScript)))
	if err != nil {
		t.Fatal(err)
	}
	return config.Merge(config.DefaultGlobalConfig(), repo), wantSource
}

func assertPlatformRunnerMarker(t *testing.T, marker string) {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "platform" {
		t.Fatalf("runner marker = %q, want platform", data)
	}
}

func assertFirstCommandRunnerReceipt(t *testing.T, sctx *pipeline.StepContext, stepID, wantSource string) {
	t.Helper()
	stored, err := sctx.DB.GetStepResult(stepID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := stored.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Commands) == 0 {
		t.Fatal("missing command receipt")
	}
	receipt := evidence.Commands[0]
	if receipt.CommandSource != wantSource || receipt.Runner == nil || receipt.Runner.Source != wantSource || receipt.Runner.Executable != "zsh" || receipt.Runner.Version == nil {
		t.Fatalf("command receipt = %+v", receipt)
	}
}
