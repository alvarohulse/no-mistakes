package steps

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestSetupStep_NoCommandSkips(t *testing.T) {
	t.Parallel()
	var logs []string
	outcome, err := (&SetupStep{}).Execute(&pipeline.StepContext{
		Config: &config.Config{},
		Log:    func(text string) { logs = append(logs, text) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.Skipped {
		t.Fatalf("outcome = %#v, want skipped", outcome)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "no setup command configured") {
		t.Fatalf("logs = %q, want skip message", logs)
	}
}

func TestBuildStep_RunsConfiguredCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "build-ran")
	var logs []string

	outcome, err := (&BuildStep{}).Execute(&pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: dir,
		Config:  &config.Config{Commands: config.Commands{Build: writeFileCommand(marker, "built")}},
		Log:     func(text string) { logs = append(logs, text) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %#v, want success", outcome)
	}
	if got := readTestFile(t, marker); got != "built" {
		t.Fatalf("marker = %q, want built", got)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "build command passed") {
		t.Fatalf("logs = %q, want success message", logs)
	}
}

func TestBuildStep_FailsOnNonZeroExit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var logs []string

	outcome, err := (&BuildStep{}).Execute(&pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: dir,
		Config:  &config.Config{Commands: config.Commands{Build: failingCommand()}},
		Log:     func(text string) { logs = append(logs, text) },
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if outcome == nil || outcome.ExitCode != 7 {
		t.Fatalf("outcome = %#v, want exit 7", outcome)
	}
	if !strings.Contains(err.Error(), "build command failed with exit code 7") {
		t.Fatalf("error = %v, want build failure", err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "nope") {
		t.Fatalf("logs = %q, want command output", logs)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeFileCommand(path, content string) string {
	if runtime.GOOS == "windows" {
		return "echo|set /p=" + content + " > " + shellQuote(path)
	}
	return "printf " + shellQuote(content) + " > " + shellQuote(path)
}

func failingCommand() string {
	if runtime.GOOS == "windows" {
		return "echo nope & exit /b 7"
	}
	return "echo nope; exit 7"
}

func shellQuote(value string) string {
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
