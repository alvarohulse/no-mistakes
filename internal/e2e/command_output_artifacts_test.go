//go:build e2e

package e2e

import (
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/artifact"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestWholeRunCommandAttemptsRetainImmutableOutputArtifacts(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "command-output-artifacts"
	h.CommitChange(branch, ".no-mistakes.yaml", `allow_repo_commands: true
commands:
  build: |
    printf 'build artifact output\n'
  test: |
    :
  lint: |
    printf 'lint artifact output\n'
`, "configure command output artifacts")
	h.CommitChange(branch, "hello.txt", "hello command artifacts\n", "add command artifact fixture")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}

	p := paths.WithRoot(h.NMHome)
	database, err := db.OpenReadOnly(p.DB())
	if err != nil {
		t.Fatalf("open state database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	attempts, err := database.GetCommandAttemptsByRun(run.ID)
	if err != nil {
		t.Fatalf("get command attempts: %v", err)
	}
	if len(attempts) == 0 {
		t.Fatal("whole pipeline recorded no command attempts")
	}

	artifacts, err := database.GetArtifactsByRun(run.ID)
	if err != nil {
		t.Fatalf("get artifacts: %v", err)
	}
	outputArtifacts := make(map[string]*db.Artifact)
	for _, registered := range artifacts {
		if registered.Purpose != db.ArtifactPurposeCommandOutput {
			continue
		}
		if registered.CommandAttemptID == nil {
			t.Fatalf("command output artifact %s has no command attempt", registered.ID)
		}
		if prior, exists := outputArtifacts[registered.ID]; exists {
			t.Fatalf("command output artifact %s registered twice: %+v and %+v", registered.ID, prior, registered)
		}
		outputArtifacts[registered.ID] = registered
	}
	if len(outputArtifacts) != len(attempts) {
		t.Fatalf("registered command output artifacts = %d, want one per %d attempts", len(outputArtifacts), len(attempts))
	}

	store, err := artifact.NewStore(p, "")
	if err != nil {
		t.Fatalf("create artifact store: %v", err)
	}
	wantOutput := map[types.StepName]string{
		types.StepBuild: "build artifact output\n",
		types.StepTest:  "",
		types.StepLint:  "lint artifact output\n",
	}
	resolvedReferences := make(map[string]int)
	foundEmptyOutput := false
	foundPushAttempt := false
	for _, attempt := range attempts {
		if attempt.OutputArtifactID == nil {
			t.Fatalf("attempt %s (%s) has no output artifact reference", attempt.ID, attempt.Purpose)
		}
		resolvedReferences[*attempt.OutputArtifactID]++
		registered, err := database.GetArtifact(*attempt.OutputArtifactID)
		if err != nil {
			t.Fatalf("resolve output artifact %s for attempt %s: %v", *attempt.OutputArtifactID, attempt.ID, err)
		}
		if registered.CommandAttemptID == nil || *registered.CommandAttemptID != attempt.ID {
			t.Fatalf("output artifact %s command attempt = %v, want %s", registered.ID, registered.CommandAttemptID, attempt.ID)
		}
		if registered.StorageRoot != db.ArtifactStorageRootRun {
			t.Fatalf("output artifact %s storage root = %q, want %q", registered.ID, registered.StorageRoot, db.ArtifactStorageRootRun)
		}
		wantPath := path.Join(run.ID, "command-output", attempt.ID+".log")
		if registered.RelativePath != wantPath {
			t.Fatalf("output artifact %s path = %q, want %q", registered.ID, registered.RelativePath, wantPath)
		}
		physicalPath := filepath.Join(p.RunsDir(), filepath.FromSlash(registered.RelativePath))
		info, err := os.Stat(physicalPath)
		if err != nil {
			t.Fatalf("stat output artifact %s: %v", physicalPath, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("output artifact %s is not a regular file (%s)", physicalPath, info.Mode())
		}
		contents, err := store.Read(registered)
		if err != nil {
			t.Fatalf("integrity-read output artifact %s: %v", registered.ID, err)
		}
		step := types.StepName(attempt.Purpose)
		want, configuredCommand := wantOutput[step]
		switch {
		case configuredCommand:
			if string(contents) != want {
				t.Fatalf("output artifact %s contents = %q, want %q", registered.ID, contents, want)
			}
		case step == types.StepPush:
			foundPushAttempt = true
		default:
			t.Fatalf("unexpected command attempt %s for step %q", attempt.ID, attempt.Purpose)
		}
		if len(contents) == 0 {
			foundEmptyOutput = true
		}
	}
	if !foundPushAttempt {
		t.Fatal("whole pipeline recorded no Push command attempts")
	}
	if !foundEmptyOutput {
		t.Fatal("whole pipeline did not retain a zero-byte command output artifact")
	}
	for artifactID, references := range resolvedReferences {
		if references != 1 {
			t.Fatalf("output artifact %s is referenced by %d attempts, want exactly one", artifactID, references)
		}
		if _, ok := outputArtifacts[artifactID]; !ok {
			t.Fatalf("attempt output artifact %s is missing from the run registry", artifactID)
		}
	}
}
