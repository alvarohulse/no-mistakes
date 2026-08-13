package pipeline

import (
	"context"
	"crypto/sha256"
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

const (
	commandPlanningCleanupTimeout = 30 * time.Second
	commandPlanningOwnerMarker    = "no-mistakes-command-planner"
)

// CommandPlanningWorkspace is a lazy, run-scoped checkout used only for
// read-only Build, Test, and Lint command selection.
type CommandPlanningWorkspace struct {
	sourceDir string
	dir       string
	created   bool
	ownsPath  bool
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
			w.ownsPath = false
			w.headSHA = ""
		} else if err != nil {
			return "", fmt.Errorf("inspect command planning workspace: %w", err)
		} else {
			if err := w.validateReusable(ctx); err != nil {
				if discardErr := w.Discard(); discardErr != nil {
					return "", errors.Join(err, discardErr)
				}
			} else {
				if headSHA != w.headSHA {
					if err := w.refresh(ctx, headSHA); err != nil {
						return "", errors.Join(err, w.Discard())
					}
				}
				return w.dir, nil
			}
		}
	}

	if err := w.Discard(); err != nil {
		return "", fmt.Errorf("discard preserved command planning workspace: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(w.dir), 0o755); err != nil {
		return "", fmt.Errorf("create command planning workspace parent: %w", err)
	}
	if err := os.Mkdir(w.dir, 0o700); err != nil {
		return "", fmt.Errorf("claim command planning workspace path: %w", err)
	}
	w.ownsPath = true
	if _, err := git.Run(ctx, w.sourceDir, "clone", "--origin=origin", "--shared", "--no-checkout", "--no-tags", w.sourceDir, w.dir); err != nil {
		return "", errors.Join(fmt.Errorf("create command planning workspace: %w", err), w.Discard())
	}
	if err := w.writeOwnershipMarker(ctx); err != nil {
		return "", errors.Join(err, w.Discard())
	}
	if err := w.initialize(ctx, headSHA); err != nil {
		return "", errors.Join(err, w.Discard())
	}
	w.created = true
	w.headSHA = headSHA
	return w.dir, nil
}

func (w *CommandPlanningWorkspace) initialize(ctx context.Context, headSHA string) error {
	if _, err := git.Run(ctx, w.dir, "remote", "remove", "origin"); err != nil {
		return fmt.Errorf("remove command planning source remote: %w", err)
	}
	if _, err := runCommandPlanningGitWithEmptyHooks(ctx, w.dir, "checkout", "--detach", "--force", headSHA); err != nil {
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
	if err := resetCommandPlanningHooks(w.dir); err != nil {
		return err
	}
	hooksDir := commandPlanningHooksDir(w.dir)
	if _, err := git.Run(ctx, w.dir, "config", "--local", "core.hooksPath", hooksDir); err != nil {
		return fmt.Errorf("isolate command planning Git hooks: %w", err)
	}
	return nil
}

func (w *CommandPlanningWorkspace) refresh(ctx context.Context, headSHA string) error {
	if _, err := runCommandPlanningGitWithEmptyHooks(ctx, w.dir, "checkout", "--detach", "--force", headSHA); err != nil {
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
	if _, err := os.Lstat(w.dir); os.IsNotExist(err) {
		w.created = false
		w.ownsPath = false
		w.headSHA = ""
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect command planning workspace: %w", err)
	}
	if !w.ownsPath {
		removed, err := w.removeLegacyWorktree(ctx)
		if err != nil {
			return err
		}
		if removed {
			w.created = false
			w.headSHA = ""
			return nil
		}
		if err := w.validateOwnership(ctx); err != nil {
			return err
		}
	}
	permissionErr := makeCommandPlanningDirectoriesWritable(ctx, w.dir)
	removeErr := os.RemoveAll(w.dir)
	if removeErr == nil {
		w.created = false
		w.ownsPath = false
		w.headSHA = ""
	}
	return errors.Join(permissionErr, removeErr)
}

func (w *CommandPlanningWorkspace) removeLegacyWorktree(ctx context.Context) (bool, error) {
	gitPath := filepath.Join(w.dir, ".git")
	info, err := os.Lstat(gitPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect command planning Git metadata: %w", err)
	}
	if info.IsDir() {
		return false, nil
	}
	sourceCommonDir, err := commandPlanningCommonDir(ctx, w.sourceDir)
	if err != nil {
		return false, fmt.Errorf("inspect pipeline Git common directory: %w", err)
	}
	plannerCommonDir, err := commandPlanningCommonDir(ctx, w.dir)
	if err != nil {
		return false, fmt.Errorf("refuse command planning path with unrecognized Git metadata: %w", err)
	}
	if plannerCommonDir != sourceCommonDir {
		return false, fmt.Errorf("refuse to remove command planning path linked to another repository")
	}
	if err := git.WorktreeRemove(ctx, w.sourceDir, w.dir); err != nil {
		return false, fmt.Errorf("unregister legacy command planning worktree: %w", err)
	}
	return true, nil
}

func (w *CommandPlanningWorkspace) validateReusable(ctx context.Context) error {
	if err := w.validateOwnership(ctx); err != nil {
		return err
	}
	hooksDir := commandPlanningHooksDir(w.dir)
	configuredHooksDir, err := git.Run(ctx, w.dir, "config", "--local", "--get", "core.hooksPath")
	if err != nil {
		return fmt.Errorf("inspect command planning hooks configuration: %w", err)
	}
	if configuredHooksDir != hooksDir {
		return fmt.Errorf("command planning hooks configuration was modified")
	}
	info, err := os.Lstat(hooksDir)
	if err != nil {
		return fmt.Errorf("inspect command planning hooks: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("command planning hooks directory was replaced")
	}
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		return fmt.Errorf("inspect command planning hooks: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("command planning hooks were modified")
	}
	return nil
}

func (w *CommandPlanningWorkspace) writeOwnershipMarker(ctx context.Context) error {
	marker, err := w.expectedOwnershipMarker(ctx)
	if err != nil {
		return fmt.Errorf("build command planning ownership marker: %w", err)
	}
	if err := os.WriteFile(w.ownershipMarkerPath(), []byte(marker), 0o644); err != nil {
		return fmt.Errorf("write command planning ownership marker: %w", err)
	}
	return nil
}

func (w *CommandPlanningWorkspace) validateOwnership(ctx context.Context) error {
	marker, err := w.expectedOwnershipMarker(ctx)
	if err != nil {
		return fmt.Errorf("inspect command planning ownership: %w", err)
	}
	info, err := os.Lstat(w.ownershipMarkerPath())
	if err != nil {
		return fmt.Errorf("refuse to remove command planning path without valid ownership: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refuse to remove command planning path with invalid ownership marker")
	}
	data, err := os.ReadFile(w.ownershipMarkerPath())
	if err != nil {
		return fmt.Errorf("inspect command planning ownership marker: %w", err)
	}
	if string(data) != marker {
		return fmt.Errorf("refuse to remove command planning path owned by another workspace")
	}
	return nil
}

func (w *CommandPlanningWorkspace) expectedOwnershipMarker(ctx context.Context) (string, error) {
	sourceCommonDir, err := commandPlanningCommonDir(ctx, w.sourceDir)
	if err != nil {
		return "", err
	}
	workspaceDir, err := canonicalCommandPlanningPath(w.dir)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(sourceCommonDir + "\x00" + workspaceDir))
	return fmt.Sprintf("v1:%x\n", digest), nil
}

func (w *CommandPlanningWorkspace) ownershipMarkerPath() string {
	return filepath.Join(w.dir, ".git", commandPlanningOwnerMarker)
}

func commandPlanningCommonDir(ctx context.Context, workDir string) (string, error) {
	commonDir, err := git.Run(ctx, workDir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(workDir, commonDir)
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

func commandPlanningHooksDir(workDir string) string {
	return filepath.Join(workDir, ".git", "hooks")
}

func resetCommandPlanningHooks(workDir string) error {
	hooksDir := commandPlanningHooksDir(workDir)
	if err := os.RemoveAll(hooksDir); err != nil {
		return fmt.Errorf("remove inherited command planning hooks: %w", err)
	}
	if err := os.Mkdir(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create empty command planning hooks directory: %w", err)
	}
	return nil
}

func runCommandPlanningGitWithEmptyHooks(ctx context.Context, workDir string, args ...string) (string, error) {
	hooksDir, err := os.MkdirTemp("", "nm-command-plan-hooks-*")
	if err != nil {
		return "", fmt.Errorf("create command planning controller hooks directory: %w", err)
	}
	defer os.RemoveAll(hooksDir)
	gitArgs := append([]string{"-c", "core.hooksPath=" + hooksDir}, args...)
	return git.Run(ctx, workDir, gitArgs...)
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
