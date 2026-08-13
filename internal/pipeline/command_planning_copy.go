package pipeline

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func copyPreparedWorkspace(ctx context.Context, sourceDir, targetDir string) error {
	sourceRoot, err := filepath.Abs(sourceDir)
	if err != nil {
		return fmt.Errorf("resolve source worktree: %w", err)
	}
	targetRoot, err := filepath.Abs(targetDir)
	if err != nil {
		return fmt.Errorf("resolve planning worktree: %w", err)
	}
	sourceRoot, err = filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		return fmt.Errorf("resolve source worktree symlinks: %w", err)
	}
	targetRoot, err = filepath.EvalSymlinks(targetRoot)
	if err != nil {
		return fmt.Errorf("resolve planning worktree symlinks: %w", err)
	}
	if pathContains(sourceRoot, targetRoot) || pathContains(targetRoot, sourceRoot) {
		return fmt.Errorf("source and planning worktrees must not overlap")
	}
	if _, err := os.Lstat(filepath.Join(targetRoot, ".git")); err != nil {
		return fmt.Errorf("inspect planning worktree metadata: %w", err)
	}

	targetEntries, err := os.ReadDir(targetRoot)
	if err != nil {
		return fmt.Errorf("list planning worktree: %w", err)
	}
	for _, entry := range targetEntries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Name() == ".git" {
			continue
		}
		if err := removePreparedPath(ctx, filepath.Join(targetRoot, entry.Name())); err != nil {
			return fmt.Errorf("clear planning path %s: %w", entry.Name(), err)
		}
	}

	sourceEntries, err := os.ReadDir(sourceRoot)
	if err != nil {
		return fmt.Errorf("list source worktree: %w", err)
	}
	for _, entry := range sourceEntries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Name() == ".git" {
			continue
		}
		if err := copyPreparedPath(ctx, filepath.Join(sourceRoot, entry.Name()), filepath.Join(targetRoot, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyPreparedPath(ctx context.Context, sourcePath, targetPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return fmt.Errorf("inspect prepared path %s: %w", sourcePath, err)
	}

	switch {
	case info.Mode().IsRegular():
		return copyPreparedFile(ctx, sourcePath, targetPath, info.Mode())
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(sourcePath)
		if err != nil {
			return fmt.Errorf("read prepared symlink %s: %w", sourcePath, err)
		}
		if err := os.Symlink(target, targetPath); err != nil {
			return fmt.Errorf("copy prepared symlink %s: %w", sourcePath, err)
		}
		return nil
	case info.IsDir():
		if err := os.Mkdir(targetPath, 0o700); err != nil {
			return fmt.Errorf("create prepared directory %s: %w", targetPath, err)
		}
		entries, err := os.ReadDir(sourcePath)
		if err != nil {
			return fmt.Errorf("list prepared directory %s: %w", sourcePath, err)
		}
		for _, entry := range entries {
			if err := copyPreparedPath(ctx, filepath.Join(sourcePath, entry.Name()), filepath.Join(targetPath, entry.Name())); err != nil {
				return err
			}
		}
		if err := os.Chmod(targetPath, copiedFileMode(info.Mode())); err != nil {
			return fmt.Errorf("copy prepared directory mode %s: %w", sourcePath, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported prepared path type %s", sourcePath)
	}
}

func copyPreparedFile(ctx context.Context, sourcePath, targetPath string, mode os.FileMode) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open prepared file %s: %w", sourcePath, err)
	}
	target, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = source.Close()
		return fmt.Errorf("create prepared file %s: %w", targetPath, err)
	}
	_, copyErr := io.Copy(target, &contextReader{ctx: ctx, reader: source})
	closeTargetErr := target.Close()
	closeSourceErr := source.Close()
	if copyErr != nil {
		return fmt.Errorf("copy prepared file %s: %w", sourcePath, copyErr)
	}
	if closeTargetErr != nil {
		return fmt.Errorf("close prepared file %s: %w", targetPath, closeTargetErr)
	}
	if closeSourceErr != nil {
		return fmt.Errorf("close source prepared file %s: %w", sourcePath, closeSourceErr)
	}
	if err := os.Chmod(targetPath, copiedFileMode(mode)); err != nil {
		return fmt.Errorf("copy prepared file mode %s: %w", sourcePath, err)
	}
	return nil
}

func removePreparedPath(ctx context.Context, path string) error {
	if err := makePreparedDirectoriesWritable(ctx, path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

func makePreparedDirectoriesWritable(ctx context.Context, path string) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := filepath.WalkDir(path, func(path string, entry os.DirEntry, err error) error {
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
	}); err != nil {
		return err
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func copiedFileMode(mode os.FileMode) os.FileMode {
	return mode & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

func pathContains(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
