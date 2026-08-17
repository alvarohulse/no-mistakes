package steps

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunStepRunnerCommandUsesPlatformOverrideAndPersistsProvenance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	marker := filepath.Join(dir, "runner-selection")
	baseScript := "printf base > " + marker
	platformScript := "printf platform > " + marker
	zsh := &runner.Spec{Executable: "zsh", Args: []string{"-lc"}}
	override := &runner.Override{Run: &platformScript, Runner: zsh}
	command := runner.Command{Run: baseScript}
	wantSource := runner.SourceLinux
	if runtime.GOOS == "darwin" {
		command.MacOS = override
		wantSource = runner.SourceMacOS
	} else {
		command.Linux = override
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	sctx.Round = 1

	output, exitCode, err := runStepRunnerCommand(sctx, command)
	if err != nil || exitCode != 0 || output != "" {
		t.Fatalf("command result = output %q exit %d error %v", output, exitCode, err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "platform" {
		t.Fatalf("marker = %q, want platform override", data)
	}
	stored, err := sctx.DB.GetStepResult(step.ID)
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
	receipt := evidence.Commands[0]
	if receipt.Command != platformScript || receipt.CommandSource != wantSource || receipt.Runner == nil || receipt.Runner.Source != wantSource || receipt.Runner.Executable != "zsh" || receipt.Runner.Version == nil {
		t.Fatalf("runner receipt = %+v", receipt)
	}
}

func TestRunStepRunnerCommandRejectsInvalidSyntaxBeforeExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	marker := filepath.Join(dir, "must-not-run")
	command := runner.Command{Run: "printf ran > " + marker + "; if true; then"}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Runner = runner.Spec{Executable: "zsh", Args: []string{"-lc"}}
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	sctx.Round = 1

	_, _, err = runStepRunnerCommand(sctx, command)
	if err == nil || !errors.Is(err, runner.ErrInvalidSyntax) {
		t.Fatalf("command error = %v, want invalid syntax", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("invalid command executed before refusal: %v", statErr)
	}
	stored, err := sctx.DB.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := stored.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Commands) != 1 || evidence.Commands[0].Outcome != "error" || evidence.Commands[0].Runner == nil || evidence.Commands[0].Runner.Executable != "zsh" || !strings.Contains(evidence.Commands[0].Command, "if true; then") {
		t.Fatalf("syntax-error receipt = %+v", evidence.Commands)
	}
}

func TestRunStepRunnerCommandRetainsCompleteOutputForStepLogging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})

	output, exitCode, err := runStepRunnerCommand(sctx, runner.Command{Run: `head -c 131072 /dev/zero`})
	if err != nil || exitCode != 0 {
		t.Fatalf("command exit = %d, error %v", exitCode, err)
	}
	if len(output) != 131072 {
		t.Fatalf("output length = %d, want 131072", len(output))
	}
}
