//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestSetupBuildCommandsRunInPipeline proves setup and build are first-class
// pipeline steps: a gated push with commands.setup and commands.build
// configured runs both commands inside the run worktree, in order (setup
// before build, both after rebase and before review), records them as
// completed steps, and exposes their transcripts via `axi logs`. The build
// command asserts on the setup marker so a wrong execution order fails the
// run instead of merely reordering records.
func TestSetupBuildCommandsRunInPipeline(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	markerDir := t.TempDir()
	setupMarker := filepath.Join(markerDir, "setup-ran")
	buildMarker := filepath.Join(markerDir, "build-ran")

	branch := "feature/setup-build"
	h.CommitChange(branch, "feature.txt", "change gated by setup+build\n", "add feature change")
	config := fmt.Sprintf("ignore_patterns:\n  - 'vendor/**'\ncommands:\n  setup: \"echo setup-ok > %s\"\n  build: \"test -f %s && echo build-ok > %s\"\n", setupMarker, setupMarker, buildMarker)
	h.CommitChange(branch, ".no-mistakes.yaml", config, "configure setup and build commands")
	worktree := h.AddWorktree(branch)

	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}

	// The commands really executed: setup wrote its marker, and build (which
	// requires the setup marker to already exist) wrote its own.
	if got := readMarker(t, setupMarker); got != "setup-ok" {
		t.Fatalf("setup marker = %q, want setup-ok", got)
	}
	if got := readMarker(t, buildMarker); got != "build-ok" {
		t.Fatalf("build marker = %q, want build-ok (build runs after setup)", got)
	}

	// The run records setup and build as completed steps in the documented
	// order: intent, rebase, setup, build, review, ...
	assertPipelineStepsInOrder(t, run.Steps)
	for _, name := range []types.StepName{types.StepSetup, types.StepBuild} {
		step, ok := findStep(run.Steps, name)
		if !ok {
			t.Fatalf("%s step missing from run results", name)
		}
		if step.Status != types.StepStatusCompleted {
			t.Fatalf("%s step status = %s, want completed", name, step.Status)
		}
	}

	// The end-user log surface exposes each command transcript.
	for _, tc := range []struct{ step, want string }{
		{"setup", "running setup command: echo setup-ok"},
		{"setup", "setup command passed"},
		{"build", "running build command: test -f"},
		{"build", "build command passed"},
	} {
		out, err := h.RunInDir(worktree, "axi", "logs", "--step", tc.step)
		if err != nil {
			t.Fatalf("axi logs --step %s: %v\n%s", tc.step, err, out)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("axi logs --step %s missing %q in:\n%s", tc.step, tc.want, out)
		}
		t.Logf("axi logs --step %s:\n%s", tc.step, out)
	}
}

// TestSetupBuildCommandsBlockedByDefault proves the new commands.setup and
// commands.build fields ride the same trust boundary as the other
// code-executing fields: without the maintainer's allow_repo_commands opt-in
// on the trusted default branch, setup/build commands shipped on a
// contributor's pushed branch must never execute; the steps skip instead.
func TestSetupBuildCommandsBlockedByDefault(t *testing.T) {
	optOut := false
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	markerDir := t.TempDir()
	setupMarker := filepath.Join(markerDir, "setup-pwned")
	buildMarker := filepath.Join(markerDir, "build-pwned")

	branch := "setup-build-untrusted"
	h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")
	config := fmt.Sprintf("ignore_patterns:\n  - 'vendor/**'\ncommands:\n  setup: \"echo pwned > %s\"\n  build: \"echo pwned > %s\"\n", setupMarker, buildMarker)
	h.CommitChange(branch, ".no-mistakes.yaml", config, "configure malicious setup and build commands")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}

	for name, marker := range map[string]string{"setup": setupMarker, "build": buildMarker} {
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("SECURITY REGRESSION: pushed-branch %s command executed (marker %s exists); commands.%s must be loaded from the trusted default branch, not the pushed SHA", name, marker, name)
		}
	}

	// Sanity: both steps ran and skipped (the trusted default branch has no
	// setup/build commands), so the missing markers are a real result rather
	// than a pipeline that never reached the steps.
	for _, name := range []types.StepName{types.StepSetup, types.StepBuild} {
		step, ok := findStep(run.Steps, name)
		if !ok {
			t.Fatalf("%s step missing from run results", name)
		}
		if step.Status != types.StepStatusSkipped {
			t.Fatalf("%s step status = %s, want skipped", name, step.Status)
		}
	}
}

func readMarker(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read marker %s: %v", path, err)
	}
	return strings.TrimSpace(string(data))
}
