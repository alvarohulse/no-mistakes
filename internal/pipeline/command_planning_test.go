package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitutil "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestCommandPlanningWorkspaceCopiesPreparedState(t *testing.T) {
	sourceDir := newCommandPlanningRepo(t)
	if err := os.WriteFile(filepath.Join(sourceDir, "tracked.txt"), []byte("prepared tracked\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(sourceDir, "tracked.txt"), 0o750); err != nil {
		t.Fatal(err)
	}
	preparedDir := filepath.Join(sourceDir, "prepared-output")
	if err := os.Mkdir(preparedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	preparedFile := filepath.Join(preparedDir, "cache.bin")
	if err := os.WriteFile(preparedFile, []byte("prepared ignored\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(preparedDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(preparedDir, ".git", "config"), []byte("nested metadata\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "untracked.txt"), []byte("prepared untracked\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join("prepared-output", "cache.bin"), filepath.Join(sourceDir, "prepared-link")); err != nil {
			t.Fatal(err)
		}
	}

	workspace := newCommandPlanningWorkspaceForTest(t, sourceDir)
	plannerDir, err := workspace.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	assertFileContent(t, filepath.Join(plannerDir, "tracked.txt"), "prepared tracked\n")
	assertFileContent(t, filepath.Join(plannerDir, "prepared-output", "cache.bin"), "prepared ignored\n")
	assertFileContent(t, filepath.Join(plannerDir, "prepared-output", ".git", "config"), "nested metadata\n")
	assertFileContent(t, filepath.Join(plannerDir, "untracked.txt"), "prepared untracked\n")
	sourceInfo, err := os.Stat(preparedFile)
	if err != nil {
		t.Fatal(err)
	}
	plannerInfo, err := os.Stat(filepath.Join(plannerDir, "prepared-output", "cache.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(sourceInfo, plannerInfo) {
		t.Fatal("prepared regular file was hard-linked into the planner workspace")
	}
	if runtime.GOOS != "windows" {
		trackedInfo, err := os.Stat(filepath.Join(plannerDir, "tracked.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if got := trackedInfo.Mode().Perm(); got != 0o750 {
			t.Fatalf("prepared tracked file mode = %o, want 750", got)
		}
		if got := plannerInfo.Mode().Perm(); got != 0o751 {
			t.Fatalf("prepared file mode = %o, want 751", got)
		}
		directoryInfo, err := os.Stat(filepath.Join(plannerDir, "prepared-output"))
		if err != nil {
			t.Fatal(err)
		}
		if got := directoryInfo.Mode().Perm(); got != 0o750 {
			t.Fatalf("prepared directory mode = %o, want 750", got)
		}
		target, err := os.Readlink(filepath.Join(plannerDir, "prepared-link"))
		if err != nil {
			t.Fatal(err)
		}
		if target != filepath.Join("prepared-output", "cache.bin") {
			t.Fatalf("prepared symlink target = %q", target)
		}
	}

	if err := os.WriteFile(filepath.Join(plannerDir, "prepared-output", "cache.bin"), []byte("planner-only\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, preparedFile, "prepared ignored\n")
}

func TestCommandPlanningWorkspaceRefreshesPreparedStateAfterHeadAdvance(t *testing.T) {
	sourceDir := newCommandPlanningRepo(t)
	preparedDir := filepath.Join(sourceDir, "prepared-output")
	if err := os.Mkdir(preparedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	preparedFile := filepath.Join(preparedDir, "cache.bin")
	if err := os.WriteFile(preparedFile, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(preparedDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(preparedDir, 0o700) })
	}

	workspace := newCommandPlanningWorkspaceForTest(t, sourceDir)
	plannerDir, err := workspace.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(plannerDir, "prepared-output", "cache.bin"), "first\n")

	if err := os.Chmod(preparedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preparedFile, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(preparedDir, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "next.txt"), []byte("next head\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commandPlanningGit(t, sourceDir, "add", "next.txt")
	commandPlanningGit(t, sourceDir, "commit", "-m", "advance head")

	refreshedDir, err := workspace.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshedDir != plannerDir {
		t.Fatalf("refreshed planner dir = %q, want %q", refreshedDir, plannerDir)
	}
	assertFileContent(t, filepath.Join(plannerDir, "next.txt"), "next head\n")
	assertFileContent(t, filepath.Join(plannerDir, "prepared-output", "cache.bin"), "second\n")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(plannerDir, "prepared-output"))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o500 {
			t.Fatalf("refreshed prepared directory mode = %o, want 500", got)
		}
	}
}

func newCommandPlanningRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	commandPlanningGit(t, dir, "init")
	commandPlanningGit(t, dir, "config", "user.email", "test@example.com")
	commandPlanningGit(t, dir, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("prepared-output/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commandPlanningGit(t, dir, "add", ".gitignore", "tracked.txt")
	commandPlanningGit(t, dir, "commit", "-m", "initial")
	return dir
}

func newCommandPlanningWorkspaceForTest(t *testing.T, sourceDir string) *CommandPlanningWorkspace {
	t.Helper()
	workspace := NewCommandPlanningWorkspace(
		paths.WithRoot(t.TempDir()),
		nil,
		&db.Run{ID: "run"},
		&db.Repo{ID: "repo"},
		sourceDir,
	)
	t.Cleanup(func() {
		if err := workspace.Close(context.Background()); err != nil {
			t.Errorf("close command planning workspace: %v", err)
		}
	})
	return workspace
}

func commandPlanningGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := gitutil.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return output
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(content); got != want {
		t.Fatalf("%s content = %q, want %q", path, got, want)
	}
}
