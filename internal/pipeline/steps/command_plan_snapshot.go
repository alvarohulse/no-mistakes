package steps

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

type commandPlanningSnapshot struct {
	headSHA     string
	headRef     string
	status      string
	indexTree   string
	indexPath   string
	indexMode   os.FileMode
	indexData   []byte
	backupDir   string
	files       map[string]commandPlanningFile
	directories map[string]os.FileMode
}

type commandPlanningFile struct {
	exists     bool
	mode       os.FileMode
	digest     [sha256.Size]byte
	linkTarget string
}

func captureCommandPlanningSnapshot(ctx context.Context, workDir string) (*commandPlanningSnapshot, error) {
	return readCommandPlanningSnapshot(ctx, workDir, true)
}

func inspectCommandPlanningSnapshot(ctx context.Context, workDir string) (*commandPlanningSnapshot, error) {
	return readCommandPlanningSnapshot(ctx, workDir, false)
}

func readCommandPlanningSnapshot(ctx context.Context, workDir string, backup bool) (_ *commandPlanningSnapshot, retErr error) {
	headSHA, err := git.Run(ctx, workDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("inspect HEAD: %w", err)
	}
	headRef, err := git.Run(ctx, workDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("inspect HEAD attachment: %w", err)
	}
	status, err := git.Run(ctx, workDir, "status", "--porcelain=v1", "-z")
	if err != nil {
		return nil, fmt.Errorf("inspect Git status: %w", err)
	}
	indexTree, err := git.Run(ctx, workDir, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("inspect Git index: %w", err)
	}
	paths, err := commandPlanningMutablePaths(ctx, workDir)
	if err != nil {
		return nil, err
	}
	directories, err := commandPlanningDirectoryModes(ctx, workDir)
	if err != nil {
		return nil, err
	}

	snapshot := &commandPlanningSnapshot{
		headSHA:     headSHA,
		headRef:     headRef,
		status:      status,
		indexTree:   indexTree,
		files:       make(map[string]commandPlanningFile, len(paths)),
		directories: directories,
	}
	if backup {
		snapshot.backupDir, err = os.MkdirTemp("", "nm-command-plan-snapshot-*")
		if err != nil {
			return nil, fmt.Errorf("create command-planning snapshot: %w", err)
		}
		defer func() {
			if retErr != nil {
				_ = os.RemoveAll(snapshot.backupDir)
			}
		}()
		if err := snapshot.captureIndex(ctx, workDir); err != nil {
			return nil, err
		}
	}
	for _, path := range paths {
		file, err := captureCommandPlanningFile(ctx, workDir, snapshot.backupDir, path)
		if err != nil {
			return nil, err
		}
		snapshot.files[path] = file
	}
	return snapshot, nil
}

func (s *commandPlanningSnapshot) captureIndex(ctx context.Context, workDir string) error {
	indexPath, err := git.Run(ctx, workDir, "rev-parse", "--git-path", "index")
	if err != nil {
		return fmt.Errorf("resolve Git index: %w", err)
	}
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(workDir, indexPath)
	}
	s.indexPath = filepath.Clean(indexPath)
	info, err := os.Stat(s.indexPath)
	if err != nil {
		return fmt.Errorf("inspect Git index file: %w", err)
	}
	s.indexMode = info.Mode()
	s.indexData, err = os.ReadFile(s.indexPath)
	if err != nil {
		return fmt.Errorf("snapshot Git index file: %w", err)
	}
	return nil
}

func (s *commandPlanningSnapshot) equal(other *commandPlanningSnapshot) bool {
	if s == nil || other == nil || s.headSHA != other.headSHA || s.headRef != other.headRef || s.status != other.status || s.indexTree != other.indexTree || len(s.files) != len(other.files) || len(s.directories) != len(other.directories) {
		return false
	}
	for path, expected := range s.files {
		actual, ok := other.files[path]
		if !ok || expected != actual {
			return false
		}
	}
	for path, expected := range s.directories {
		if actual, ok := other.directories[path]; !ok || expected != actual {
			return false
		}
	}
	return true
}

func (s *commandPlanningSnapshot) restore(ctx context.Context, workDir string) error {
	if s == nil || s.backupDir == "" {
		return fmt.Errorf("command-planning snapshot has no restorable backup")
	}
	if s.headRef == "HEAD" {
		if _, err := git.Run(ctx, workDir, "checkout", "--detach", "--force", s.headSHA); err != nil {
			return fmt.Errorf("restore detached planning HEAD: %w", err)
		}
	} else {
		if _, err := git.Run(ctx, workDir, "checkout", "--force", s.headRef); err != nil {
			return fmt.Errorf("restore planning branch %s: %w", s.headRef, err)
		}
		if _, err := git.Run(ctx, workDir, "reset", "--hard", s.headSHA); err != nil {
			return fmt.Errorf("restore planning branch HEAD: %w", err)
		}
	}
	if err := makeCommandPlanningDirectoriesWritable(ctx, workDir); err != nil {
		return fmt.Errorf("prepare planner-created files for removal: %w", err)
	}
	if _, err := git.Run(ctx, workDir, "clean", "-ffdx"); err != nil {
		return fmt.Errorf("remove planner-created files: %w", err)
	}
	if err := os.WriteFile(s.indexPath, s.indexData, s.indexMode.Perm()); err != nil {
		return fmt.Errorf("restore Git index: %w", err)
	}
	if err := os.Chmod(s.indexPath, s.indexMode.Perm()); err != nil {
		return fmt.Errorf("restore Git index mode: %w", err)
	}
	paths := make([]string, 0, len(s.files))
	for path := range s.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := s.restoreFile(ctx, workDir, path, s.files[path]); err != nil {
			return err
		}
	}
	directories := make([]string, 0, len(s.directories))
	for path := range s.directories {
		directories = append(directories, path)
	}
	sort.Strings(directories)
	for _, path := range directories {
		fullPath, err := commandPlanningPath(workDir, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(fullPath, 0o700); err != nil {
			return fmt.Errorf("restore snapshot directory %s: %w", path, err)
		}
	}
	for index := len(directories) - 1; index >= 0; index-- {
		path := directories[index]
		fullPath, err := commandPlanningPath(workDir, path)
		if err != nil {
			return err
		}
		if err := os.Chmod(fullPath, s.directories[path].Perm()); err != nil {
			return fmt.Errorf("restore snapshot directory mode %s: %w", path, err)
		}
	}
	return nil
}

func (s *commandPlanningSnapshot) restoreFile(ctx context.Context, workDir, path string, file commandPlanningFile) error {
	fullPath, err := commandPlanningPath(workDir, path)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(fullPath); err != nil {
		return fmt.Errorf("replace restored path %s: %w", path, err)
	}
	if !file.exists {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("create parent for restored path %s: %w", path, err)
	}
	if file.mode.IsDir() {
		if err := os.Mkdir(fullPath, 0o700); err != nil {
			return fmt.Errorf("restore snapshot directory %s: %w", path, err)
		}
		return nil
	}
	if file.mode&os.ModeSymlink != 0 {
		if err := os.Symlink(file.linkTarget, fullPath); err != nil {
			return fmt.Errorf("restore snapshot symlink %s: %w", path, err)
		}
		return nil
	}
	backupPath, err := commandPlanningPath(s.backupDir, path)
	if err != nil {
		return err
	}
	if err := copyCommandPlanningFile(ctx, backupPath, fullPath, file.mode); err != nil {
		return fmt.Errorf("restore snapshot file %s: %w", path, err)
	}
	return nil
}

func (s *commandPlanningSnapshot) cleanup() {
	if s != nil && s.backupDir != "" {
		_ = os.RemoveAll(s.backupDir)
	}
}

func commandPlanningMutablePaths(ctx context.Context, workDir string) ([]string, error) {
	paths := make(map[string]struct{})
	commands := [][]string{
		{"diff", "--name-only", "-z"},
		{"diff", "--cached", "--name-only", "-z"},
		{"ls-files", "--others", "--exclude-standard", "--directory", "--no-empty-directory", "-z"},
		{"ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory", "-z"},
	}
	for _, args := range commands {
		output, err := git.Run(ctx, workDir, args...)
		if err != nil {
			return nil, fmt.Errorf("inspect mutable command-planning paths: %w", err)
		}
		for _, path := range strings.Split(output, "\x00") {
			if path != "" {
				if err := addCommandPlanningPath(workDir, strings.TrimSuffix(path, "/"), paths); err != nil {
					return nil, err
				}
			}
		}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	return ordered, nil
}

func addCommandPlanningPath(workDir, path string, paths map[string]struct{}) error {
	fullPath, err := commandPlanningPath(workDir, path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(fullPath)
	if os.IsNotExist(err) {
		paths[path] = struct{}{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect mutable path %s: %w", path, err)
	}
	if !info.IsDir() {
		paths[path] = struct{}{}
		return nil
	}
	return filepath.WalkDir(fullPath, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(workDir, current)
		if err != nil {
			return err
		}
		paths[filepath.ToSlash(relative)] = struct{}{}
		return nil
	})
}

func commandPlanningDirectoryModes(ctx context.Context, workDir string) (map[string]os.FileMode, error) {
	modes := make(map[string]os.FileMode)
	err := filepath.WalkDir(workDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == filepath.Join(workDir, ".git") && entry.IsDir() {
			return filepath.SkipDir
		}
		if path == workDir || !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(workDir, path)
		if err != nil {
			return err
		}
		modes[filepath.ToSlash(relative)] = info.Mode()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inspect command-planning directory modes: %w", err)
	}
	return modes, nil
}

func makeCommandPlanningDirectoriesWritable(ctx context.Context, workDir string) error {
	return filepath.WalkDir(workDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == filepath.Join(workDir, ".git") && entry.IsDir() {
			return filepath.SkipDir
		}
		if path != workDir && entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return nil
	})
}

func captureCommandPlanningFile(ctx context.Context, workDir, backupDir, path string) (commandPlanningFile, error) {
	fullPath, err := commandPlanningPath(workDir, path)
	if err != nil {
		return commandPlanningFile{}, err
	}
	info, err := os.Lstat(fullPath)
	if os.IsNotExist(err) {
		return commandPlanningFile{}, nil
	}
	if err != nil {
		return commandPlanningFile{}, fmt.Errorf("inspect mutable path %s: %w", path, err)
	}
	file := commandPlanningFile{exists: true, mode: info.Mode()}
	switch {
	case info.IsDir():
		return file, nil
	case info.Mode().IsRegular():
		file.digest, err = digestCommandPlanningFile(ctx, fullPath)
		if err == nil && backupDir != "" {
			backupPath, pathErr := commandPlanningPath(backupDir, path)
			if pathErr != nil {
				return commandPlanningFile{}, pathErr
			}
			if err = os.MkdirAll(filepath.Dir(backupPath), 0o755); err == nil {
				err = copyCommandPlanningFile(ctx, fullPath, backupPath, info.Mode())
			}
		}
	case info.Mode()&os.ModeSymlink != 0:
		file.linkTarget, err = os.Readlink(fullPath)
	default:
		return commandPlanningFile{}, fmt.Errorf("unsupported mutable path type %s", path)
	}
	if err != nil {
		return commandPlanningFile{}, fmt.Errorf("snapshot mutable path %s: %w", path, err)
	}
	return file, nil
}

func digestCommandPlanningFile(ctx context.Context, path string) ([sha256.Size]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, &commandPlanningContextReader{ctx: ctx, reader: file})
	closeErr := file.Close()
	if copyErr != nil {
		return [sha256.Size]byte{}, copyErr
	}
	if closeErr != nil {
		return [sha256.Size]byte{}, closeErr
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func copyCommandPlanningFile(ctx context.Context, sourcePath, targetPath string, mode os.FileMode) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	target, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = source.Close()
		return err
	}
	_, copyErr := io.Copy(target, &commandPlanningContextReader{ctx: ctx, reader: source})
	closeTargetErr := target.Close()
	closeSourceErr := source.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeTargetErr != nil {
		return closeTargetErr
	}
	if closeSourceErr != nil {
		return closeSourceErr
	}
	if err := os.Chmod(targetPath, mode.Perm()); err != nil {
		return err
	}
	return nil
}

type commandPlanningContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *commandPlanningContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func commandPlanningPath(workDir, path string) (string, error) {
	cleanPath := filepath.Clean(filepath.FromSlash(path))
	if cleanPath == "." || filepath.IsAbs(cleanPath) || cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refuse invalid command-planning path %q", path)
	}
	return filepath.Join(workDir, cleanPath), nil
}
