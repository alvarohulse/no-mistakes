package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

const commandPlanningCleanupTimeout = 30 * time.Second

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

// Prepare returns a private planning clone at the pipeline worktree's current
// committed HEAD. The clone borrows Git objects locally but never shares refs,
// index, worktree metadata, remotes, or uncommitted source state.
func (w *CommandPlanningWorkspace) Prepare(ctx context.Context) (string, error) {
	if w == nil {
		return "", fmt.Errorf("command planning workspace is unavailable")
	}
	headSHA, err := git.Run(ctx, w.sourceDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("inspect pipeline HEAD for command planning: %w", err)
	}
	if w.created {
		if _, err := os.Stat(w.dir); os.IsNotExist(err) {
			w.created = false
			w.headSHA = ""
		} else if err != nil {
			return "", fmt.Errorf("inspect command planning workspace: %w", err)
		} else {
			if headSHA != w.headSHA {
				if err := w.refresh(ctx, headSHA); err != nil {
					return "", errors.Join(err, w.Discard())
				}
			}
			return w.dir, nil
		}
	}

	if err := w.Discard(); err != nil {
		return "", fmt.Errorf("discard preserved command planning workspace: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(w.dir), 0o755); err != nil {
		return "", fmt.Errorf("create command planning workspace parent: %w", err)
	}
	if _, err := git.Run(ctx, w.sourceDir, "clone", "--shared", "--no-checkout", "--no-tags", w.sourceDir, w.dir); err != nil {
		return "", errors.Join(fmt.Errorf("create command planning workspace: %w", err), w.Discard())
	}
	w.created = true
	if err := w.initialize(ctx, headSHA); err != nil {
		return "", errors.Join(err, w.Discard())
	}
	w.headSHA = headSHA
	return w.dir, nil
}

func (w *CommandPlanningWorkspace) initialize(ctx context.Context, headSHA string) error {
	hooksDir := filepath.Join(w.dir, ".git", "no-mistakes-hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create empty command planning hooks directory: %w", err)
	}
	if _, err := git.Run(ctx, w.dir, "config", "--local", "core.hooksPath", hooksDir); err != nil {
		return fmt.Errorf("disable command planning Git hooks: %w", err)
	}
	if _, err := git.Run(ctx, w.dir, "remote", "remove", "origin"); err != nil {
		return fmt.Errorf("remove command planning source remote: %w", err)
	}
	if _, err := git.Run(ctx, w.dir, "checkout", "--detach", "--force", headSHA); err != nil {
		return fmt.Errorf("checkout command planning HEAD: %w", err)
	}
	refs, err := git.Run(ctx, w.dir, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return fmt.Errorf("list command planning refs: %w", err)
	}
	for _, ref := range strings.Split(refs, "\n") {
		if ref == "" {
			continue
		}
		if _, err := git.Run(ctx, w.dir, "update-ref", "-d", ref); err != nil {
			return fmt.Errorf("remove command planning ref %s: %w", ref, err)
		}
	}
	return nil
}

func (w *CommandPlanningWorkspace) refresh(ctx context.Context, headSHA string) error {
	if _, err := git.Run(ctx, w.dir, "checkout", "--detach", "--force", headSHA); err != nil {
		return fmt.Errorf("refresh command planning workspace: %w", err)
	}
	if _, err := git.Run(ctx, w.dir, "clean", "-ffdx"); err != nil {
		return fmt.Errorf("clean refreshed command planning workspace: %w", err)
	}
	w.headSHA = headSHA
	return nil
}

// Discard removes the workspace with a cleanup context independent of the
// planner invocation. It remains usable after cancellation or inspection
// timeout and forces the next Prepare call to create a fresh clone.
func (w *CommandPlanningWorkspace) Discard() error {
	ctx, cancel := context.WithTimeout(context.Background(), commandPlanningCleanupTimeout)
	defer cancel()
	return w.Close(ctx)
}

// Close removes the run-scoped planning clone. It is safe before Prepare and
// after a successful close.
func (w *CommandPlanningWorkspace) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if err := w.removeLegacyWorktree(ctx); err != nil {
		return err
	}
	permissionErr := makeCommandPlanningDirectoriesWritable(ctx, w.dir)
	removeErr := os.RemoveAll(w.dir)
	if removeErr == nil {
		w.created = false
		w.headSHA = ""
	}
	return errors.Join(permissionErr, removeErr)
}

func (w *CommandPlanningWorkspace) removeLegacyWorktree(ctx context.Context) error {
	gitPath := filepath.Join(w.dir, ".git")
	info, err := os.Lstat(gitPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect command planning Git metadata: %w", err)
	}
	if info.IsDir() {
		return nil
	}
	sourceCommonDir, err := commandPlanningCommonDir(ctx, w.sourceDir)
	if err != nil {
		return fmt.Errorf("inspect pipeline Git common directory: %w", err)
	}
	plannerCommonDir, err := commandPlanningCommonDir(ctx, w.dir)
	if err != nil {
		return fmt.Errorf("inspect legacy planner Git common directory: %w", err)
	}
	if plannerCommonDir != sourceCommonDir {
		return nil
	}
	if err := git.WorktreeRemove(ctx, w.sourceDir, w.dir); err != nil {
		return fmt.Errorf("unregister legacy command planning worktree: %w", err)
	}
	return nil
}

func commandPlanningCommonDir(ctx context.Context, workDir string) (string, error) {
	commonDir, err := git.Run(ctx, workDir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(workDir, commonDir)
	}
	absolute, err := filepath.Abs(commonDir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func makeCommandPlanningDirectoriesWritable(ctx context.Context, root string) error {
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return nil
	})
}
