package steps

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const commandPlanningSourceRestoreTimeout = 5 * time.Minute

type commandPlanningSourceSnapshot struct {
	workDir     string
	tempDir     string
	indexFile   string
	fingerprint commandPlanningSourceFingerprint
	files       []commandPlanningSourceFile
}

type commandPlanningSourceFingerprint struct {
	worktreeTree string
	indexFlags   string
	files        string
}

type commandPlanningSourceFile struct {
	path   string
	exists bool
	mode   os.FileMode
	data   []byte
}

func captureCommandPlanningSource(ctx context.Context, workDir string) (*commandPlanningSourceSnapshot, error) {
	tempDir, err := os.MkdirTemp("", "nm-command-plan-source-*")
	if err != nil {
		return nil, fmt.Errorf("create command planning source snapshot: %w", err)
	}
	snapshot := &commandPlanningSourceSnapshot{
		workDir:   workDir,
		tempDir:   tempDir,
		indexFile: filepath.Join(tempDir, "worktree-index"),
	}
	if err := snapshot.capture(ctx); err != nil {
		return nil, errors.Join(err, snapshot.Close())
	}
	return snapshot, nil
}

func (s *commandPlanningSourceSnapshot) capture(ctx context.Context) error {
	for _, name := range []string{"index", "HEAD", "config.worktree"} {
		path, err := commandPlanningSourceGitPath(ctx, s.workDir, name)
		if err != nil {
			return fmt.Errorf("resolve command planning source %s path: %w", name, err)
		}
		file, err := captureCommandPlanningSourceFile(path)
		if err != nil {
			return fmt.Errorf("capture command planning source %s: %w", name, err)
		}
		s.files = append(s.files, file)
	}
	if !s.files[0].exists {
		return fmt.Errorf("capture command planning source index: index is missing")
	}
	if err := os.WriteFile(s.indexFile, s.files[0].data, 0o600); err != nil {
		return fmt.Errorf("copy command planning source index: %w", err)
	}
	if err := s.stageWorktree(ctx, s.indexFile); err != nil {
		return err
	}

	fingerprint, err := s.inspect(ctx, s.indexFile)
	if err != nil {
		return err
	}
	s.fingerprint = fingerprint
	return nil
}

func (s *commandPlanningSourceSnapshot) Changed(ctx context.Context) (bool, error) {
	indexFile := filepath.Join(s.tempDir, "after-index")
	data, err := os.ReadFile(s.indexFile)
	if err != nil {
		return false, fmt.Errorf("read command planning source snapshot index: %w", err)
	}
	if err := os.WriteFile(indexFile, data, 0o600); err != nil {
		return false, fmt.Errorf("copy command planning source comparison index: %w", err)
	}
	if err := s.stageWorktree(ctx, indexFile); err != nil {
		return false, err
	}
	fingerprint, err := s.inspect(ctx, indexFile)
	if err != nil {
		return false, err
	}
	return fingerprint != s.fingerprint, nil
}

func (s *commandPlanningSourceSnapshot) Restore() error {
	ctx, cancel := context.WithTimeout(context.Background(), commandPlanningSourceRestoreTimeout)
	defer cancel()

	var restoreErr error
	if err := s.files[2].restore(); err != nil {
		restoreErr = errors.Join(restoreErr, err)
	}
	if restoreErr != nil {
		return fmt.Errorf("restore command planning worktree configuration: %w", restoreErr)
	}

	gitEnv := append(pipeline.CommandPlanningGitEnv(),
		"GIT_INDEX_FILE="+s.indexFile,
		"GIT_WORK_TREE="+s.workDir,
	)
	if _, err := git.RunWithEnv(ctx, s.workDir, gitEnv, "checkout-index", "--all", "--force", "--ignore-skip-worktree-bits"); err != nil {
		restoreErr = errors.Join(restoreErr, fmt.Errorf("restore command planning source files: %w", err))
	}
	if _, err := git.RunWithEnv(ctx, s.workDir, gitEnv, "clean", "-ffd"); err != nil {
		restoreErr = errors.Join(restoreErr, fmt.Errorf("remove command planning source creations: %w", err))
	}
	for _, file := range s.files[:2] {
		if err := file.restore(); err != nil {
			restoreErr = errors.Join(restoreErr, err)
		}
	}
	if restoreErr != nil {
		return restoreErr
	}

	changed, err := s.Changed(ctx)
	if err != nil {
		return fmt.Errorf("verify restored command planning source: %w", err)
	}
	if changed {
		return fmt.Errorf("verify restored command planning source: source still differs from its snapshot")
	}
	return nil
}

func (s *commandPlanningSourceSnapshot) Close() error {
	if s == nil || s.tempDir == "" {
		return nil
	}
	return os.RemoveAll(s.tempDir)
}

func (s *commandPlanningSourceSnapshot) stageWorktree(ctx context.Context, indexFile string) error {
	env := append(pipeline.CommandPlanningGitEnv(),
		"GIT_INDEX_FILE="+indexFile,
		"GIT_WORK_TREE="+s.workDir,
	)
	paths, err := git.RunWithEnv(ctx, s.workDir, env, "ls-files", "-z")
	if err != nil {
		return fmt.Errorf("list command planning source paths: %w", err)
	}
	if paths != "" {
		if _, err := git.RunWithEnvInput(ctx, s.workDir, env, []byte(paths), "update-index", "--no-assume-unchanged", "--no-skip-worktree", "-z", "--stdin"); err != nil {
			return fmt.Errorf("clear command planning source snapshot index flags: %w", err)
		}
	}
	if _, err := git.RunWithEnv(ctx, s.workDir, env, "add", "--all"); err != nil {
		return fmt.Errorf("snapshot command planning source files: %w", err)
	}
	return nil
}

func (s *commandPlanningSourceSnapshot) inspect(ctx context.Context, indexFile string) (commandPlanningSourceFingerprint, error) {
	var fingerprint commandPlanningSourceFingerprint
	commands := []struct {
		name   string
		args   []string
		target *string
	}{
		{name: "worktree tree", args: []string{"write-tree"}, target: &fingerprint.worktreeTree},
		{name: "index flags", args: []string{"ls-files", "-v", "-z"}, target: &fingerprint.indexFlags},
	}
	for i, command := range commands {
		env := pipeline.CommandPlanningGitEnv()
		if i == 0 {
			env = append(env, "GIT_INDEX_FILE="+indexFile, "GIT_WORK_TREE="+s.workDir)
		}
		output, err := git.RunWithEnv(ctx, s.workDir, env, command.args...)
		if err != nil {
			return commandPlanningSourceFingerprint{}, fmt.Errorf("inspect command planning source %s: %w", command.name, err)
		}
		*command.target = output
	}
	currentFiles := make([]commandPlanningSourceFile, 0, len(s.files))
	for _, file := range s.files {
		current, err := captureCommandPlanningSourceFile(file.path)
		if err != nil {
			return commandPlanningSourceFingerprint{}, fmt.Errorf("inspect command planning source metadata %s: %w", file.path, err)
		}
		currentFiles = append(currentFiles, current)
	}
	fingerprint.files = commandPlanningSourceFilesFingerprint(currentFiles)
	return fingerprint, nil
}

func commandPlanningSourceGitPath(ctx context.Context, workDir, name string) (string, error) {
	path, err := git.RunWithEnv(ctx, workDir, pipeline.CommandPlanningGitEnv(), "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workDir, path)
	}
	return filepath.Clean(path), nil
}

func captureCommandPlanningSourceFile(path string) (commandPlanningSourceFile, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return commandPlanningSourceFile{path: path}, nil
	}
	if err != nil {
		return commandPlanningSourceFile{}, err
	}
	if !info.Mode().IsRegular() {
		return commandPlanningSourceFile{}, fmt.Errorf("not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return commandPlanningSourceFile{}, err
	}
	return commandPlanningSourceFile{path: path, exists: true, mode: info.Mode().Perm(), data: data}, nil
}

func (f commandPlanningSourceFile) restore() error {
	if !f.exists {
		if err := os.RemoveAll(f.path); err != nil {
			return fmt.Errorf("remove created command planning source metadata %s: %w", f.path, err)
		}
		return nil
	}
	if info, err := os.Lstat(f.path); err == nil && !info.Mode().IsRegular() {
		if err := os.RemoveAll(f.path); err != nil {
			return fmt.Errorf("remove replaced command planning source metadata %s: %w", f.path, err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(f.path, f.data, f.mode); err != nil {
		return fmt.Errorf("restore command planning source metadata %s: %w", f.path, err)
	}
	return nil
}

func commandPlanningSourceFilesFingerprint(files []commandPlanningSourceFile) string {
	var fingerprint strings.Builder
	for _, file := range files {
		fmt.Fprintf(&fingerprint, "%s:%t:%o:", file.path, file.exists, file.mode)
		digest := sha256.Sum256(file.data)
		fmt.Fprintf(&fingerprint, "%x\x00", digest)
	}
	return fingerprint.String()
}
