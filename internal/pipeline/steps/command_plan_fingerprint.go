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
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

type commandPlanningFingerprint struct {
	headSHA    string
	headRef    string
	status     string
	indexTree  string
	indexFlags string
	refs       string
	config     string
	submodules string
	controller string
	tracked    string
}

func inspectCommandPlanningFingerprint(ctx context.Context, workDir string) (*commandPlanningFingerprint, error) {
	fingerprint := &commandPlanningFingerprint{}
	var stagedPaths string
	var trackedPaths string
	commands := []struct {
		name   string
		args   []string
		target *string
	}{
		{name: "HEAD", args: []string{"rev-parse", "HEAD"}, target: &fingerprint.headSHA},
		{name: "HEAD attachment", args: []string{"rev-parse", "--abbrev-ref", "HEAD"}, target: &fingerprint.headRef},
		{name: "Git status", args: []string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching", "--ignore-submodules=none"}, target: &fingerprint.status},
		{name: "Git index", args: []string{"write-tree"}, target: &fingerprint.indexTree},
		{name: "Git index flags", args: []string{"ls-files", "-v", "-z"}, target: &fingerprint.indexFlags},
		{name: "Git refs", args: []string{"for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)"}, target: &fingerprint.refs},
		{name: "Git config", args: []string{"config", "--local", "--list", "-z"}, target: &fingerprint.config},
		{name: "Git submodules", args: []string{"submodule", "status", "--recursive"}, target: &fingerprint.submodules},
		{name: "Git index entries", args: []string{"ls-files", "--stage", "-z"}, target: &stagedPaths},
		{name: "tracked paths", args: []string{"ls-files", "-z"}, target: &trackedPaths},
	}
	for _, command := range commands {
		output, err := git.RunWithEnv(ctx, workDir, pipeline.CommandPlanningGitEnv(), command.args...)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", command.name, err)
		}
		*command.target = output
	}
	controller, err := inspectCommandPlanningControllerMetadata(workDir)
	if err != nil {
		return nil, err
	}
	fingerprint.controller = controller
	tracked, err := inspectCommandPlanningTrackedLayout(ctx, workDir, stagedPaths, trackedPaths)
	if err != nil {
		return nil, err
	}
	fingerprint.tracked = tracked
	return fingerprint, nil
}

func (f *commandPlanningFingerprint) equal(other *commandPlanningFingerprint) bool {
	return f != nil && other != nil && *f == *other
}

func inspectCommandPlanningControllerMetadata(workDir string) (string, error) {
	markerPath := filepath.Join(workDir, ".git", "no-mistakes-command-planner")
	markerInfo, err := os.Lstat(markerPath)
	if err != nil {
		return "", fmt.Errorf("inspect command planning ownership marker: %w", err)
	}
	if !markerInfo.Mode().IsRegular() {
		return "", fmt.Errorf("inspect command planning ownership marker: not a regular file")
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		return "", fmt.Errorf("read command planning ownership marker: %w", err)
	}

	hooksDir := filepath.Join(workDir, ".git", "hooks")
	hooksInfo, err := os.Lstat(hooksDir)
	if err != nil {
		return "", fmt.Errorf("inspect command planning hooks: %w", err)
	}
	if !hooksInfo.IsDir() || hooksInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("inspect command planning hooks: not a directory")
	}
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		return "", fmt.Errorf("list command planning hooks: %w", err)
	}
	var hooks strings.Builder
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return "", fmt.Errorf("inspect command planning hook %s: %w", entry.Name(), err)
		}
		fmt.Fprintf(&hooks, "%s:%s:%o\x00", entry.Name(), info.Mode().Type(), info.Mode().Perm())
	}
	return fmt.Sprintf("marker:%o:%s\x00hooks:%o:%s", markerInfo.Mode().Perm(), marker, hooksInfo.Mode().Perm(), hooks.String()), nil
}

func inspectCommandPlanningTrackedLayout(ctx context.Context, workDir, stagedPaths, trackedPaths string) (string, error) {
	gitlinks := make(map[string]struct{})
	for _, record := range strings.Split(stagedPaths, "\x00") {
		metadata, path, ok := strings.Cut(record, "\t")
		if !ok || !strings.HasPrefix(metadata, "160000 ") {
			continue
		}
		cleanPath, err := commandPlanningTrackedPath(path)
		if err != nil {
			return "", err
		}
		gitlinks[cleanPath] = struct{}{}
	}

	ancestors := make(map[string]struct{})
	for _, path := range strings.Split(trackedPaths, "\x00") {
		if path == "" {
			continue
		}
		cleanPath, err := commandPlanningTrackedPath(path)
		if err != nil {
			return "", err
		}
		for ancestor := filepath.Dir(cleanPath); ancestor != "."; ancestor = filepath.Dir(ancestor) {
			ancestors[ancestor] = struct{}{}
		}
	}

	paths := make([]string, 0, len(gitlinks)+len(ancestors))
	for path := range gitlinks {
		paths = append(paths, path)
	}
	for path := range ancestors {
		paths = append(paths, filepath.Join(path, ".git"))
	}
	sort.Strings(paths)
	var fingerprint strings.Builder
	for _, path := range paths {
		if err := appendCommandPlanningPathFingerprint(ctx, workDir, path, &fingerprint); err != nil {
			return "", err
		}
	}
	return fingerprint.String(), nil
}

func commandPlanningTrackedPath(path string) (string, error) {
	cleanPath := filepath.Clean(filepath.FromSlash(path))
	if cleanPath == "." || filepath.IsAbs(cleanPath) || cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refuse invalid tracked command-planning path %q", path)
	}
	return cleanPath, nil
}

func appendCommandPlanningPathFingerprint(ctx context.Context, workDir, relativePath string, fingerprint *strings.Builder) error {
	fullPath := filepath.Join(workDir, relativePath)
	info, err := os.Lstat(fullPath)
	if os.IsNotExist(err) {
		fmt.Fprintf(fingerprint, "%s:missing\x00", filepath.ToSlash(relativePath))
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect command planning path %s: %w", relativePath, err)
	}
	if !info.IsDir() {
		return appendCommandPlanningEntryFingerprint(ctx, workDir, fullPath, info, fingerprint)
	}
	return filepath.WalkDir(fullPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return appendCommandPlanningEntryFingerprint(ctx, workDir, path, info, fingerprint)
	})
}

func appendCommandPlanningEntryFingerprint(ctx context.Context, workDir, path string, info os.FileInfo, fingerprint *strings.Builder) error {
	relativePath, err := filepath.Rel(workDir, path)
	if err != nil {
		return err
	}
	fmt.Fprintf(fingerprint, "%s:%s:%o", filepath.ToSlash(relativePath), info.Mode().Type(), info.Mode().Perm())
	switch {
	case info.Mode().IsRegular():
		digest, err := digestCommandPlanningPath(ctx, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(fingerprint, ":%x", digest)
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(fingerprint, ":%s", target)
	}
	fingerprint.WriteByte(0)
	return nil
}

func digestCommandPlanningPath(ctx context.Context, path string) ([sha256.Size]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, &commandPlanningFingerprintReader{ctx: ctx, reader: file})
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

type commandPlanningFingerprintReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *commandPlanningFingerprintReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
