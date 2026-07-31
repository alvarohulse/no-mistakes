package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestAgentStepsScopeStackedChangesToEffectiveBase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		commands config.Commands
		execute  func(*pipeline.StepContext) error
	}{
		{
			name: "review",
			execute: func(sctx *pipeline.StepContext) error {
				_, err := (&ReviewStep{}).Execute(sctx)
				return err
			},
		},
		{
			name: "test",
			execute: func(sctx *pipeline.StepContext) error {
				_, err := (&TestStep{}).Execute(sctx)
				return err
			},
		},
		{
			name:     "document",
			commands: config.Commands{Lint: "true"},
			execute: func(sctx *pipeline.StepContext) error {
				_, err := (&DocumentStep{}).Execute(sctx)
				return err
			},
		},
		{
			name: "lint",
			execute: func(sctx *pipeline.StepContext) error {
				_, err := (&LintStep{}).Execute(sctx)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir, upstream, mainSHA, dependencySHA, featureSHA := setupStackedValidationRepo(t)
			ag := &mockAgent{name: "test"}
			sctx := newTestContextWithDBRecords(t, ag, dir, mainSHA, featureSHA, tt.commands)
			sctx.Run.StackedOn = "dependency"
			sctx.Repo.UpstreamURL = upstream

			if err := tt.execute(sctx); err != nil {
				t.Fatal(err)
			}
			if len(ag.calls) != 1 {
				t.Fatalf("agent calls = %d, want 1", len(ag.calls))
			}
			baseSHA := promptBaseCommit(t, ag.calls[0].Prompt)
			if baseSHA != dependencySHA {
				t.Fatalf("prompt base = %s, want stacked dependency %s (default %s)", baseSHA, dependencySHA, mainSHA)
			}
			changed := gitCmd(t, dir, "diff", "--name-only", baseSHA+".."+featureSHA)
			if changed != "feature.txt" {
				t.Fatalf("stacked change scope = %q, want only feature.txt", changed)
			}
		})
	}
}

func setupStackedValidationRepo(t *testing.T) (dir, upstream, mainSHA, dependencySHA, featureSHA string) {
	t.Helper()
	upstream = t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir = t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	mainSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "dependency")
	os.WriteFile(filepath.Join(dir, "dependency.txt"), []byte("dependency\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "dependency")
	dependencySHA = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "dependency")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	featureSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")
	return dir, upstream, mainSHA, dependencySHA, featureSHA
}

func promptBaseCommit(t *testing.T, prompt string) string {
	t.Helper()
	const prefix = "- base commit: "
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	t.Fatalf("prompt does not contain a base commit:\n%s", prompt)
	return ""
}
