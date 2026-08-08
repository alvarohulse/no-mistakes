package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

func writeDoctorGlobalConfig(t *testing.T, nmHome, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorListsOverrideKeysOutsideARepository(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	writeDoctorGlobalConfig(t, nmHome, "overrides:\n  scaleapi/scaleapi:\n    commands:\n      lint: make lint\n  other/project:\n    commands:\n      test: make test\n")
	chdir(t, t.TempDir())

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	for _, want := range []string{"repo overrides", "other/project, scaleapi/scaleapi", "not inside a repository"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output should contain %q, got:\n%s", want, out)
		}
	}
}

func TestDoctorReportsWhetherTheCurrentRepositoryMatchesAnOverride(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	for _, tt := range []struct {
		name   string
		origin string
		want   string
	}{
		{name: "match", origin: "git@github.com:ScaleAPI/scaleapi.git", want: "scaleapi/scaleapi applies to this repository"},
		{name: "no match", origin: "https://github.com/other/project.git", want: "none apply to this repository"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			nmHome := t.TempDir()
			t.Setenv("NM_HOME", nmHome)
			writeDoctorGlobalConfig(t, nmHome, "overrides:\n  scaleapi/scaleapi:\n    commands:\n      lint: make lint\n")
			repoDir := t.TempDir()
			run(t, repoDir, "git", "init")
			run(t, repoDir, "git", "remote", "add", "origin", tt.origin)
			chdir(t, repoDir)

			out, err := executeCmd("doctor")
			if err != nil {
				t.Fatalf("doctor failed: %v\n%s", err, out)
			}
			if !strings.Contains(out, "repo overrides") || !strings.Contains(out, tt.want) {
				t.Errorf("doctor output should contain repo overrides with %q, got:\n%s", tt.want, out)
			}
		})
	}
}

func TestDoctorFailsLoudlyOnMalformedOverridesKey(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	writeDoctorGlobalConfig(t, nmHome, "overrides:\n  not-owner-repo:\n    commands:\n      lint: make lint\n")

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	for _, want := range []string{"gate validation", "overrides key", "some checks failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output should contain %q, got:\n%s", want, out)
		}
	}
}
