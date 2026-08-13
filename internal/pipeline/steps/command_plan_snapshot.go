package steps

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

type commandPlanningSnapshot struct {
	headSHA   string
	status    string
	untracked map[string]commandPlanningFile
}

type commandPlanningFile struct {
	mode       os.FileMode
	data       []byte
	linkTarget string
}

func captureCommandPlanningSnapshot(ctx context.Context, workDir string) (*commandPlanningSnapshot, error) {
	headSHA, err := git.Run(ctx, workDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("inspect HEAD: %w", err)
	}
	status, err := git.Run(ctx, workDir, "status", "--porcelain=v1", "-z")
	if err != nil {
		return nil, fmt.Errorf("inspect Git status: %w", err)
	}
	paths, err := commandPlanningUntrackedPaths(ctx, workDir)
	if err != nil {
		return nil, fmt.Errorf("inspect untracked files: %w", err)
	}
	untracked := make(map[string]commandPlanningFile, len(paths))
	for _, path := range paths {
		file, err := captureCommandPlanningFile(workDir, path)
		if err != nil {
			return nil, err
		}
		untracked[path] = file
	}
	return &commandPlanningSnapshot{headSHA: headSHA, status: status, untracked: untracked}, nil
}

func (s *commandPlanningSnapshot) equal(other *commandPlanningSnapshot) bool {
	if s == nil || other == nil || s.headSHA != other.headSHA || s.status != other.status || len(s.untracked) != len(other.untracked) {
		return false
	}
	for path, expected := range s.untracked {
		actual, ok := other.untracked[path]
		if !ok || expected.mode != actual.mode || expected.linkTarget != actual.linkTarget || !bytes.Equal(expected.data, actual.data) {
			return false
		}
	}
	return true
}

func (s *commandPlanningSnapshot) restore(ctx context.Context, workDir string) error {
	currentHead, err := git.Run(ctx, workDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("inspect planning worktree HEAD during restore: %w", err)
	}
	currentStatus, err := git.Run(ctx, workDir, "status", "--porcelain=v1", "-z")
	if err != nil {
		return fmt.Errorf("inspect planning worktree status during restore: %w", err)
	}
	if currentHead != s.headSHA || currentStatus != s.status {
		if _, err := git.Run(ctx, workDir, "reset", "--hard", s.headSHA); err != nil {
			return fmt.Errorf("restore planning worktree HEAD: %w", err)
		}
	}
	currentPaths, err := commandPlanningUntrackedPaths(ctx, workDir)
	if err != nil {
		return fmt.Errorf("inspect planning worktree during restore: %w", err)
	}
	for _, path := range currentPaths {
		if _, preserve := s.untracked[path]; preserve {
			continue
		}
		fullPath, err := commandPlanningPath(workDir, path)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(fullPath); err != nil {
			return fmt.Errorf("remove planner-created path %s: %w", path, err)
		}
	}
	paths := make([]string, 0, len(s.untracked))
	for path := range s.untracked {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := restoreCommandPlanningFile(workDir, path, s.untracked[path]); err != nil {
			return err
		}
	}
	return nil
}

func commandPlanningUntrackedPaths(ctx context.Context, workDir string) ([]string, error) {
	output, err := git.Run(ctx, workDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0)
	for _, path := range strings.Split(output, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

func captureCommandPlanningFile(workDir, path string) (commandPlanningFile, error) {
	fullPath, err := commandPlanningPath(workDir, path)
	if err != nil {
		return commandPlanningFile{}, err
	}
	info, err := os.Lstat(fullPath)
	if err != nil {
		return commandPlanningFile{}, fmt.Errorf("inspect untracked path %s: %w", path, err)
	}
	file := commandPlanningFile{mode: info.Mode()}
	switch {
	case info.Mode().IsRegular():
		file.data, err = os.ReadFile(fullPath)
	case info.Mode()&os.ModeSymlink != 0:
		file.linkTarget, err = os.Readlink(fullPath)
	default:
		return commandPlanningFile{}, fmt.Errorf("unsupported untracked path type %s", path)
	}
	if err != nil {
		return commandPlanningFile{}, fmt.Errorf("snapshot untracked path %s: %w", path, err)
	}
	return file, nil
}

func restoreCommandPlanningFile(workDir, path string, file commandPlanningFile) error {
	fullPath, err := commandPlanningPath(workDir, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("create parent for restored path %s: %w", path, err)
	}
	if err := os.RemoveAll(fullPath); err != nil {
		return fmt.Errorf("replace restored path %s: %w", path, err)
	}
	if file.mode&os.ModeSymlink != 0 {
		if err := os.Symlink(file.linkTarget, fullPath); err != nil {
			return fmt.Errorf("restore untracked symlink %s: %w", path, err)
		}
		return nil
	}
	if err := os.WriteFile(fullPath, file.data, file.mode.Perm()); err != nil {
		return fmt.Errorf("restore untracked file %s: %w", path, err)
	}
	if err := os.Chmod(fullPath, file.mode.Perm()); err != nil {
		return fmt.Errorf("restore untracked file mode %s: %w", path, err)
	}
	return nil
}

func commandPlanningPath(workDir, path string) (string, error) {
	cleanPath := filepath.Clean(filepath.FromSlash(path))
	if cleanPath == "." || filepath.IsAbs(cleanPath) || cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refuse invalid command-planning path %q", path)
	}
	return filepath.Join(workDir, cleanPath), nil
}
