package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// CommandPlanningWorkspace is a lazy, run-scoped checkout used only for
// read-only Build, Test, and Lint command selection.
type CommandPlanningWorkspace struct {
	sourceDir string
	dir       string
	created   bool
	headSHA   string
}

// NewCommandPlanningWorkspace returns an uncreated workspace. Prepare creates
// it beneath the managed worktree root on first use.
func NewCommandPlanningWorkspace(p *paths.Paths, _ *config.Config, run *db.Run, repo *db.Repo, sourceDir string) *CommandPlanningWorkspace {
	if p == nil || run == nil || repo == nil {
		return nil
	}
	return &CommandPlanningWorkspace{
		sourceDir: sourceDir,
		dir:       p.WorktreeDir(repo.ID, paths.CommandPlanWorktreeID(run.ID)),
	}
}

// Prepare returns a planning checkout at the pipeline worktree's current HEAD.
// Prepared state is copied from the primary worktree, where the trusted hook
// has already run, and refreshed whenever the pipeline advances HEAD.
func (w *CommandPlanningWorkspace) Prepare(ctx context.Context) (string, error) {
	if w == nil {
		return "", fmt.Errorf("command planning workspace is unavailable")
	}
	headSHA, err := git.Run(ctx, w.sourceDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("inspect pipeline HEAD for command planning: %w", err)
	}
	if w.created {
		if headSHA != w.headSHA {
			if err := w.refresh(ctx, headSHA); err != nil {
				return "", err
			}
		}
		return w.dir, nil
	}
	adopted, err := w.adoptExisting(ctx)
	if err != nil {
		return "", err
	}
	if adopted {
		w.created = true
		if err := w.refresh(ctx, headSHA); err != nil {
			return "", errors.Join(err, w.Close(context.Background()))
		}
		return w.dir, nil
	}

	if err := os.MkdirAll(filepath.Dir(w.dir), 0o755); err != nil {
		return "", fmt.Errorf("create command planning workspace parent: %w", err)
	}
	if err := git.WorktreeAdd(ctx, w.sourceDir, w.dir, headSHA); err != nil {
		return "", fmt.Errorf("create command planning workspace: %w", err)
	}
	w.created = true
	w.headSHA = headSHA
	if err := w.copyPreparedState(ctx); err != nil {
		return "", errors.Join(err, w.Close(context.Background()))
	}
	return w.dir, nil
}

func (w *CommandPlanningWorkspace) adoptExisting(ctx context.Context) (bool, error) {
	info, err := os.Lstat(w.dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect preserved command planning workspace: %w", err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("preserved command planning workspace is not a directory")
	}

	expectedRoot, err := canonicalCommandPlanningPath(w.dir)
	if err != nil {
		return false, fmt.Errorf("resolve preserved command planning workspace: %w", err)
	}
	worktreeRoot, err := git.Run(ctx, w.dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return false, fmt.Errorf("inspect preserved command planning workspace: %w", err)
	}
	worktreeRoot, err = canonicalCommandPlanningPath(worktreeRoot)
	if err != nil {
		return false, fmt.Errorf("resolve preserved command planning worktree root: %w", err)
	}
	if worktreeRoot != expectedRoot {
		return false, fmt.Errorf("refuse to adopt command planning workspace with root %q", worktreeRoot)
	}

	sourceCommonDir, err := commandPlanningCommonDir(ctx, w.sourceDir)
	if err != nil {
		return false, fmt.Errorf("inspect pipeline git common directory: %w", err)
	}
	plannerCommonDir, err := commandPlanningCommonDir(ctx, w.dir)
	if err != nil {
		return false, fmt.Errorf("inspect preserved planner git common directory: %w", err)
	}
	if plannerCommonDir != sourceCommonDir {
		return false, fmt.Errorf("refuse to adopt command planning workspace from another repository")
	}
	return true, nil
}

func (w *CommandPlanningWorkspace) refresh(ctx context.Context, headSHA string) error {
	if _, err := git.Run(ctx, w.dir, "checkout", "--detach", "--force", headSHA); err != nil {
		return fmt.Errorf("refresh command planning workspace: %w", err)
	}
	if err := w.copyPreparedState(ctx); err != nil {
		return fmt.Errorf("refresh prepared command planning workspace: %w", err)
	}
	w.headSHA = headSHA
	return nil
}

func commandPlanningCommonDir(ctx context.Context, worktreeDir string) (string, error) {
	commonDir, err := git.Run(ctx, worktreeDir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktreeDir, commonDir)
	}
	return canonicalCommandPlanningPath(commonDir)
}

func canonicalCommandPlanningPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func (w *CommandPlanningWorkspace) copyPreparedState(ctx context.Context) error {
	if err := copyPreparedWorkspace(ctx, w.sourceDir, w.dir); err != nil {
		return fmt.Errorf("copy prepared pipeline worktree: %w", err)
	}
	return nil
}

// Close removes the run-scoped planning worktree. It is safe before Prepare
// and after a successful close.
func (w *CommandPlanningWorkspace) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if !w.created {
		adopted, err := w.adoptExisting(ctx)
		if err != nil {
			return err
		}
		if !adopted {
			return nil
		}
		w.created = true
	}
	permissionErr := makePreparedDirectoriesWritable(ctx, w.dir)
	worktreeErr := git.WorktreeRemove(ctx, w.sourceDir, w.dir)
	removeErr := os.RemoveAll(w.dir)
	if worktreeErr == nil {
		w.created = false
	}
	return errors.Join(permissionErr, worktreeErr, removeErr)
}
