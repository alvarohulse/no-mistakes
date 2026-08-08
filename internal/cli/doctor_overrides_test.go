package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
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
	for _, want := range []string{"repo overrides", "other/project, scaleapi/scaleapi", "not inside a git repository"} {
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

// A run matches overrides against the registered upstream URL, so doctor must
// answer the same question: a registered repository whose checkout origin has
// drifted still reports the override that would actually apply.
func TestDoctorMatchesOverridesByRegisteredUpstreamAheadOfOrigin(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	writeDoctorGlobalConfig(t, nmHome, "overrides:\n  scaleapi/scaleapi:\n    commands:\n      lint: make lint\n")

	repoDir := t.TempDir()
	run(t, repoDir, "git", "init")
	run(t, repoDir, "git", "remote", "add", "origin", "https://github.com/other/project.git")
	root, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		root = repoDir
	}
	chdir(t, root)

	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := database.InsertRepoWithID("repo-1", root, "https://github.com/ScaleAPI/scaleapi.git", "main"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	database.Close()

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "scaleapi/scaleapi applies to this repository") {
		t.Errorf("doctor should match the registered upstream, got:\n%s", out)
	}
}

// A remote with no <owner>/<repo> identity can never match a key, so doctor
// says that instead of claiming the directory is not a repository.
func TestDoctorReportsARemoteWithNoOwnerRepoIdentity(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	writeDoctorGlobalConfig(t, nmHome, "overrides:\n  scaleapi/scaleapi:\n    commands:\n      lint: make lint\n")

	repoDir := t.TempDir()
	run(t, repoDir, "git", "init")
	run(t, repoDir, "git", "remote", "add", "origin", "../sibling-mirror.git")
	chdir(t, repoDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no <owner>/<repo> identity") || strings.Contains(out, "not inside a git repository") {
		t.Errorf("doctor should report the unusable remote identity, got:\n%s", out)
	}
}

// NM_REPO_CONFIG is retired. A machine that still exports it silently loses the
// commands, hooks, and agent routes it used to supply, so doctor must say so.
func TestDoctorReportsRetiredRepoConfigEnvAsAMigrationSignal(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	t.Setenv("NM_HOME", t.TempDir())
	t.Setenv(retiredRepoConfigEnv, filepath.Join(t.TempDir(), "machine-repo-config.yaml"))
	chdir(t, t.TempDir())

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	for _, want := range []string{retiredRepoConfigEnv, "no longer supported", "overrides"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output should contain %q, got:\n%s", want, out)
		}
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
