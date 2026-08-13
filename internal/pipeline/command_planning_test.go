package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitutil "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestCommandPlanningWorkspaceUsesIndependentCommittedCheckout(t *testing.T) {
	sourceDir := newCommandPlanningRepo(t)
	if err := os.WriteFile(filepath.Join(sourceDir, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(sourceDir, "prepared-output"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "prepared-output", "cache.bin"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "untracked.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	workspace := newCommandPlanningWorkspaceForTest(t, sourceDir)
	plannerDir, err := workspace.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	assertFileContent(t, filepath.Join(plannerDir, "tracked.txt"), "tracked\n")
	for _, path := range []string{"prepared-output", "untracked.txt"} {
		if _, err := os.Stat(filepath.Join(plannerDir, path)); !os.IsNotExist(err) {
			t.Fatalf("source-only path %q exists in planner: %v", path, err)
		}
	}
	sourceCommonDir := commandPlanningGit(t, sourceDir, "rev-parse", "--git-common-dir")
	plannerCommonDir := commandPlanningGit(t, plannerDir, "rev-parse", "--git-common-dir")
	if canonicalTestPath(t, sourceDir, sourceCommonDir) == canonicalTestPath(t, plannerDir, plannerCommonDir) {
		t.Fatal("planner shares refs, index, and worktree metadata with the source")
	}
	if _, err := gitutil.Run(context.Background(), plannerDir, "remote", "get-url", "origin"); err == nil {
		t.Fatal("planner retained a usable source remote")
	}
}

func TestCommandPlanningWorkspaceDoesNotCopyLargeIgnoredTree(t *testing.T) {
	sourceDir := newCommandPlanningRepo(t)
	ignoredDir := filepath.Join(sourceDir, "prepared-output")
	if err := os.Mkdir(ignoredDir, 0o755); err != nil {
		t.Fatal(err)
	}
	largePath := filepath.Join(ignoredDir, "large-cache.bin")
	file, err := os.Create(largePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(64 << 20); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	plannerDir, err := newCommandPlanningWorkspaceForTest(t, sourceDir).Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(plannerDir, "prepared-output")); !os.IsNotExist(err) {
		t.Fatalf("large ignored source tree was copied into planner: %v", err)
	}
}

func TestCommandPlanningWorkspaceDoesNotCopyInitializedNestedSubmoduleMetadata(t *testing.T) {
	leafDir := newCommandPlanningRepo(t)
	middleDir := newCommandPlanningRepo(t)
	commandPlanningGit(t, middleDir, "-c", "protocol.file.allow=always", "submodule", "add", leafDir, "nested")
	commandPlanningGit(t, middleDir, "commit", "-am", "add nested dependency")

	sourceDir := newCommandPlanningRepo(t)
	commandPlanningGit(t, sourceDir, "-c", "protocol.file.allow=always", "submodule", "add", middleDir, "dependency")
	commandPlanningGit(t, sourceDir, "commit", "-am", "add dependency")
	commandPlanningGit(t, sourceDir, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")
	if _, err := os.Stat(filepath.Join(sourceDir, "dependency", "nested", ".git")); err != nil {
		t.Fatalf("source nested submodule is not initialized: %v", err)
	}

	plannerDir, err := newCommandPlanningWorkspaceForTest(t, sourceDir).Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join("dependency", ".git"),
		filepath.Join("dependency", "nested", ".git"),
	} {
		if _, err := os.Stat(filepath.Join(plannerDir, path)); !os.IsNotExist(err) {
			t.Fatalf("source submodule metadata %q was copied into planner: %v", path, err)
		}
	}
}

func TestCommandPlanningWorkspaceReusesCleanCloneAndRefreshesHead(t *testing.T) {
	sourceDir := newCommandPlanningRepo(t)
	workspace := newCommandPlanningWorkspaceForTest(t, sourceDir)
	plannerDir, err := workspace.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again, err := workspace.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	} else if again != plannerDir {
		t.Fatalf("reused planner dir = %q, want %q", again, plannerDir)
	}

	if err := os.WriteFile(filepath.Join(sourceDir, "next.txt"), []byte("next head\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commandPlanningGit(t, sourceDir, "add", "next.txt")
	commandPlanningGit(t, sourceDir, "commit", "-m", "advance head")
	wantHead := commandPlanningGit(t, sourceDir, "rev-parse", "HEAD")

	refreshedDir, err := workspace.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshedDir != plannerDir {
		t.Fatalf("refreshed planner dir = %q, want %q", refreshedDir, plannerDir)
	}
	if got := commandPlanningGit(t, plannerDir, "rev-parse", "HEAD"); got != wantHead {
		t.Fatalf("refreshed planner HEAD = %q, want %q", got, wantHead)
	}
	assertFileContent(t, filepath.Join(plannerDir, "next.txt"), "next head\n")
}

func TestCommandPlanningWorkspaceReplacesLegacyLinkedWorktree(t *testing.T) {
	sourceDir := newCommandPlanningRepo(t)
	workspace := newCommandPlanningWorkspaceForTest(t, sourceDir)
	headSHA := commandPlanningGit(t, sourceDir, "rev-parse", "HEAD")
	if err := gitutil.WorktreeAdd(context.Background(), sourceDir, workspace.dir, headSHA); err != nil {
		t.Fatal(err)
	}
	if worktrees := commandPlanningGit(t, sourceDir, "worktree", "list", "--porcelain"); !strings.Contains(worktrees, workspace.dir) {
		t.Fatalf("legacy planner %q is not registered:\n%s", workspace.dir, worktrees)
	}

	plannerDir, err := workspace.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if worktrees := commandPlanningGit(t, sourceDir, "worktree", "list", "--porcelain"); strings.Contains(worktrees, plannerDir) {
		t.Fatalf("legacy planner remains registered:\n%s", worktrees)
	}
	sourceCommonDir := commandPlanningGit(t, sourceDir, "rev-parse", "--git-common-dir")
	plannerCommonDir := commandPlanningGit(t, plannerDir, "rev-parse", "--git-common-dir")
	if canonicalTestPath(t, sourceDir, sourceCommonDir) == canonicalTestPath(t, plannerDir, plannerCommonDir) {
		t.Fatal("replacement planner still shares Git metadata with the source")
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

func canonicalTestPath(t *testing.T, workDir, path string) string {
	t.Helper()
	if !filepath.IsAbs(path) {
		path = filepath.Join(workDir, path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return path
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
