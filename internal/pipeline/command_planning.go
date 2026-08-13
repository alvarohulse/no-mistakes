package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/worktreehook"
)

// CommandPlanningWorkspace is a lazy, run-scoped checkout used only for
// read-only Build, Test, and Lint command selection.
type CommandPlanningWorkspace struct {
	sourceDir          string
	dir                string
	config             *config.Config
	created            bool
	headSHA            string
	preservedUntracked map[string]struct{}
}

// NewCommandPlanningWorkspace returns an uncreated workspace. Prepare creates
// it beneath the managed worktree root on first use.
func NewCommandPlanningWorkspace(p *paths.Paths, cfg *config.Config, run *db.Run, repo *db.Repo, sourceDir string) *CommandPlanningWorkspace {
	if p == nil || run == nil || repo == nil {
		return nil
	}
	return &CommandPlanningWorkspace{
		sourceDir: sourceDir,
		dir:       p.WorktreeDir(repo.ID, run.ID+"-command-plan"),
		config:    cfg,
	}
}

// Prepare returns the hook-prepared planning checkout at the pipeline
// worktree's current HEAD. The trusted hook runs only when the workspace is
// first created; ignored dependency outputs remain available across steps.
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
			if _, err := git.Run(ctx, w.dir, "reset", "--hard", headSHA); err != nil {
				return "", fmt.Errorf("refresh command planning workspace: %w", err)
			}
			w.headSHA = headSHA
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
	if err := worktreehook.Run(ctx, w.dir, w.config); err != nil {
		return "", errors.Join(err, w.Close(context.Background()))
	}
	w.preservedUntracked, err = planningUntrackedPaths(ctx, w.dir)
	if err != nil {
		return "", errors.Join(fmt.Errorf("inspect prepared command planning workspace: %w", err), w.Close(context.Background()))
	}
	return w.dir, nil
}

// Restore discards planner-created Git-visible changes while retaining
// untracked files produced by the one-time preparation hook.
func (w *CommandPlanningWorkspace) Restore(ctx context.Context) error {
	if w == nil || !w.created {
		return nil
	}
	if _, err := git.Run(ctx, w.dir, "reset", "--hard", w.headSHA); err != nil {
		return fmt.Errorf("restore command planning workspace HEAD: %w", err)
	}
	current, err := planningUntrackedPaths(ctx, w.dir)
	if err != nil {
		return fmt.Errorf("inspect command planning workspace during restore: %w", err)
	}
	for path := range current {
		if _, preserve := w.preservedUntracked[path]; preserve {
			continue
		}
		cleanPath := filepath.Clean(filepath.FromSlash(path))
		if cleanPath == "." || filepath.IsAbs(cleanPath) || cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refuse to remove invalid planner-created path %q", path)
		}
		if err := os.RemoveAll(filepath.Join(w.dir, cleanPath)); err != nil {
			return fmt.Errorf("remove planner-created path %s: %w", path, err)
		}
	}
	return nil
}

// Close removes the run-scoped planning worktree. It is safe before Prepare
// and after a successful close.
func (w *CommandPlanningWorkspace) Close(ctx context.Context) error {
	if w == nil || !w.created {
		return nil
	}
	worktreeErr := git.WorktreeRemove(ctx, w.sourceDir, w.dir)
	removeErr := os.RemoveAll(w.dir)
	if worktreeErr == nil {
		w.created = false
	}
	return errors.Join(worktreeErr, removeErr)
}

func planningUntrackedPaths(ctx context.Context, workDir string) (map[string]struct{}, error) {
	output, err := git.Run(ctx, workDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	paths := make(map[string]struct{})
	for _, path := range strings.Split(output, "\x00") {
		if path != "" {
			paths[path] = struct{}{}
		}
	}
	return paths, nil
}
