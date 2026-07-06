package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func worktreeHashTestRepo(t *testing.T) string {
	t.Helper()
	dir := initTestRepo(t)
	writeFile(t, filepath.Join(dir, "README.md"), "# test edited\n")
	writeFile(t, filepath.Join(dir, "notes.txt"), "untracked notes\n")
	if err := os.MkdirAll(filepath.Join(dir, "evidence"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "evidence", "run.log"), "evidence\n")
	return dir
}

func TestWorktreeContentHash_StableAndReadOnly(t *testing.T) {
	dir := worktreeHashTestRepo(t)
	ctx := context.Background()
	statusBefore := run(t, dir, "git", "status", "--porcelain")

	first, err := WorktreeContentHash(ctx, dir)
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	if first == "" {
		t.Fatal("expected non-empty hash")
	}
	second, err := WorktreeContentHash(ctx, dir)
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	if first != second {
		t.Fatalf("hash not stable: %q vs %q", first, second)
	}
	if statusAfter := run(t, dir, "git", "status", "--porcelain"); statusAfter != statusBefore {
		t.Fatalf("snapshot mutated real index/status:\nbefore: %q\nafter: %q", statusBefore, statusAfter)
	}
}

func TestWorktreeContentHash_DetectsUntrackedContentEdit(t *testing.T) {
	dir := worktreeHashTestRepo(t)
	ctx := context.Background()

	before, err := WorktreeContentHash(ctx, dir)
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	writeFile(t, filepath.Join(dir, "notes.txt"), "untracked notes edited\n")
	after, err := WorktreeContentHash(ctx, dir)
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	if before == after {
		t.Fatal("hash did not change after editing untracked file content")
	}
}

func TestWorktreeContentHash_DetectsNewFileInUntrackedDir(t *testing.T) {
	dir := worktreeHashTestRepo(t)
	ctx := context.Background()

	before, err := WorktreeContentHash(ctx, dir)
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	writeFile(t, filepath.Join(dir, "evidence", "extra.log"), "more evidence\n")
	after, err := WorktreeContentHash(ctx, dir)
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	if before == after {
		t.Fatal("hash did not change after adding file inside untracked dir")
	}
}

func TestWorktreeContentHash_DetectsTrackedEdit(t *testing.T) {
	dir := worktreeHashTestRepo(t)
	ctx := context.Background()

	before, err := WorktreeContentHash(ctx, dir)
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	writeFile(t, filepath.Join(dir, "README.md"), "# test edited again\n")
	after, err := WorktreeContentHash(ctx, dir)
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	if before == after {
		t.Fatal("hash did not change after editing tracked file")
	}
}

func TestWorktreeContentHash_ForceIncludeDetectsIgnoredContentEdit(t *testing.T) {
	dir := worktreeHashTestRepo(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(dir, ".gitignore"), "*.png\n")
	run(t, dir, "git", "add", ".gitignore")
	run(t, dir, "git", "commit", "-m", "ignore pngs")
	writeFile(t, filepath.Join(dir, "evidence", "shot.png"), "png before\n")

	plainBefore, err := WorktreeContentHash(ctx, dir)
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	before, err := WorktreeContentHash(ctx, dir, "evidence")
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	if before == plainBefore {
		t.Fatal("force-included hash should differ from plain hash when ignored content exists")
	}

	writeFile(t, filepath.Join(dir, "evidence", "shot.png"), "png after\n")
	plainAfter, err := WorktreeContentHash(ctx, dir)
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	if plainBefore != plainAfter {
		t.Fatal("plain hash changed for gitignored content that git add -A cannot stage")
	}
	after, err := WorktreeContentHash(ctx, dir, "evidence")
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	if before == after {
		t.Fatal("force-included hash did not change after editing ignored file in force-included dir")
	}
}

func TestWorktreeContentHash_IgnoresGitignoredFiles(t *testing.T) {
	dir := worktreeHashTestRepo(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(dir, ".gitignore"), "cache/\n")
	run(t, dir, "git", "add", ".gitignore")
	run(t, dir, "git", "commit", "-m", "ignore cache")
	if err := os.MkdirAll(filepath.Join(dir, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "cache", "tmp.bin"), "cached\n")

	before, err := WorktreeContentHash(ctx, dir)
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	writeFile(t, filepath.Join(dir, "cache", "tmp.bin"), "cached edited\n")
	after, err := WorktreeContentHash(ctx, dir)
	if err != nil {
		t.Fatalf("WorktreeContentHash failed: %v", err)
	}
	if before != after {
		t.Fatal("hash changed for gitignored content that git add -A cannot stage")
	}
}
