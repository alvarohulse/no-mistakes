package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

func TestDoctorRejectsRelativeMachineRepoConfigPath(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	t.Setenv("NM_HOME", t.TempDir())
	t.Setenv("NM_REPO_CONFIG", "config/repo.yaml")

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	for _, want := range []string{"machine config", "NM_REPO_CONFIG must be an absolute path", "some checks failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output should contain %q, got:\n%s", want, out)
		}
	}
}

func TestDoctorAcceptsAbsoluteMachineRepoConfigPath(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	t.Setenv("NM_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "repo.yaml")
	if err := os.WriteFile(path, []byte("repo: https://github.com/owner/project\ncommands:\n  test: go test ./...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_REPO_CONFIG", path)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	for _, want := range []string{"machine config", path} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output should contain %q, got:\n%s", want, out)
		}
	}
}
