package steps

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const (
	commandPlanningIgnoredDiscoveryMaxBytes = 8 * 1024
	commandPlanningIgnoredMaxRoots          = 64
	commandPlanningIgnoredMaxEntries        = 512
	commandPlanningIgnoredMaxEntriesPerRoot = 128
	commandPlanningIgnoredMaxBytes          = 2 * 1024 * 1024
	commandPlanningIgnoredMaxBytesPerRoot   = 512 * 1024
)

type commandPlanningIgnoredSnapshot struct {
	roots     []commandPlanningIgnoredRoot
	truncated bool
}

type commandPlanningIgnoredRoot struct {
	path    string
	entries []commandPlanningIgnoredEntry
	skipped bool
}

type commandPlanningIgnoredEntry struct {
	path       string
	mode       os.FileMode
	data       []byte
	linkTarget string
}

type commandPlanningIgnoredBudget struct {
	entries int
	bytes   int64
}

func captureCommandPlanningIgnoredSnapshot(ctx context.Context, workDir string) (commandPlanningIgnoredSnapshot, error) {
	paths, truncated, err := commandPlanningIgnoredRoots(ctx, workDir)
	if err != nil {
		return commandPlanningIgnoredSnapshot{}, err
	}
	snapshot := commandPlanningIgnoredSnapshot{truncated: truncated}
	budget := &commandPlanningIgnoredBudget{}
	for _, path := range paths {
		root, err := captureCommandPlanningIgnoredRoot(ctx, workDir, path, budget)
		if err != nil {
			return commandPlanningIgnoredSnapshot{}, err
		}
		snapshot.roots = append(snapshot.roots, root)
	}
	return snapshot, nil
}

func commandPlanningIgnoredRoots(ctx context.Context, workDir string) ([]string, bool, error) {
	output, outputTruncated, err := git.RunWithEnvOutputLimit(ctx, workDir, pipeline.CommandPlanningGitEnv(), commandPlanningIgnoredDiscoveryMaxBytes,
		"status", "--porcelain=v1", "-z", "--ignored=matching", "--untracked-files=normal", "--ignore-submodules=none")
	if err != nil {
		return nil, false, fmt.Errorf("list ignored command planning source paths: %w", err)
	}
	paths := make([]string, 0)
	records := strings.Split(output, "\x00")
	if outputTruncated && !strings.HasSuffix(output, "\x00") {
		records = records[:len(records)-1]
	}
	for _, record := range records {
		if !strings.HasPrefix(record, "!! ") {
			continue
		}
		path, err := commandPlanningTrackedPath(strings.TrimPrefix(record, "!! "))
		if err != nil {
			return nil, false, err
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	paths = compactCommandPlanningIgnoredRoots(paths)
	if len(paths) <= commandPlanningIgnoredMaxRoots {
		return paths, outputTruncated, nil
	}
	return paths[:commandPlanningIgnoredMaxRoots], true, nil
}

func compactCommandPlanningIgnoredRoots(paths []string) []string {
	// Byte-wise sorting does not keep a root adjacent to its descendants
	// ("build-cache" sorts between "build" and "build/out"), so every path is
	// tested against the whole retained set rather than the previous element.
	retained := make(map[string]struct{}, len(paths))
	compacted := paths[:0]
	for _, path := range paths {
		if commandPlanningRootRetained(retained, path) {
			continue
		}
		retained[path] = struct{}{}
		compacted = append(compacted, path)
	}
	return compacted
}

func commandPlanningRootRetained(retained map[string]struct{}, path string) bool {
	for ancestor := path; ; {
		if _, ok := retained[ancestor]; ok {
			return true
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return false
		}
		ancestor = parent
	}
}

func captureCommandPlanningIgnoredRoot(ctx context.Context, workDir, path string, budget *commandPlanningIgnoredBudget) (commandPlanningIgnoredRoot, error) {
	root := commandPlanningIgnoredRoot{path: path}
	if budget.entries >= commandPlanningIgnoredMaxEntries {
		root.skipped = true
		return root, nil
	}
	budget.entries++
	rootEntries := 1
	var rootBytes int64
	queue := []string{path}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return commandPlanningIgnoredRoot{}, err
		}
		entryPath := queue[0]
		queue = queue[1:]
		fullPath := filepath.Join(workDir, entryPath)
		info, err := os.Lstat(fullPath)
		if err != nil {
			return commandPlanningIgnoredRoot{}, fmt.Errorf("inspect ignored command planning source path %s: %w", entryPath, err)
		}
		entry := commandPlanningIgnoredEntry{path: entryPath, mode: info.Mode()}
		switch {
		case info.IsDir():
			children, complete, err := readCommandPlanningIgnoredChildren(fullPath, rootEntries, budget.entries)
			if err != nil {
				return commandPlanningIgnoredRoot{}, fmt.Errorf("inspect ignored command planning source directory %s: %w", entryPath, err)
			}
			rootEntries += len(children)
			budget.entries += len(children)
			if !complete {
				root.skipped = true
				return root, nil
			}
			for _, child := range children {
				queue = append(queue, filepath.Join(entryPath, child.Name()))
			}
		case info.Mode().IsRegular():
			remainingBytes := min(int64(commandPlanningIgnoredMaxBytesPerRoot)-rootBytes, int64(commandPlanningIgnoredMaxBytes)-budget.bytes)
			if info.Size() < 0 || info.Size() > remainingBytes {
				root.skipped = true
				return root, nil
			}
			data, complete, err := readCommandPlanningIgnoredFile(fullPath, remainingBytes)
			if err != nil {
				return commandPlanningIgnoredRoot{}, fmt.Errorf("read ignored command planning source file %s: %w", entryPath, err)
			}
			if !complete {
				root.skipped = true
				return root, nil
			}
			entry.data = data
			rootBytes += int64(len(data))
			budget.bytes += int64(len(data))
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(fullPath)
			if err != nil {
				return commandPlanningIgnoredRoot{}, fmt.Errorf("read ignored command planning source symlink %s: %w", entryPath, err)
			}
			size := int64(len(target))
			if rootBytes+size > commandPlanningIgnoredMaxBytesPerRoot || budget.bytes+size > commandPlanningIgnoredMaxBytes {
				root.skipped = true
				return root, nil
			}
			entry.linkTarget = target
			rootBytes += size
			budget.bytes += size
		default:
			root.skipped = true
			return root, nil
		}
		root.entries = append(root.entries, entry)
	}
	sort.Slice(root.entries, func(i, j int) bool { return root.entries[i].path < root.entries[j].path })
	return root, nil
}

func readCommandPlanningIgnoredFile(path string, remainingBytes int64) ([]byte, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, remainingBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, false, readErr
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	return data, int64(len(data)) <= remainingBytes, nil
}

func readCommandPlanningIgnoredChildren(path string, rootEntries, totalEntries int) ([]os.DirEntry, bool, error) {
	remaining := min(commandPlanningIgnoredMaxEntriesPerRoot-rootEntries, commandPlanningIgnoredMaxEntries-totalEntries)
	if remaining <= 0 {
		return nil, false, nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	// One entry beyond the budget is read so a directory holding exactly
	// `remaining` entries still reaches io.EOF and counts as complete;
	// os.File.ReadDir only reports io.EOF once it returns nothing.
	children := make([]os.DirEntry, 0, remaining+1)
	complete := false
	var readErr error
	for len(children) <= remaining {
		var batch []os.DirEntry
		batch, readErr = dir.ReadDir(remaining + 1 - len(children))
		children = append(children, batch...)
		if errors.Is(readErr, io.EOF) {
			complete = true
			break
		}
		if readErr != nil {
			break
		}
	}
	closeErr := dir.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, false, readErr
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	if len(children) > remaining {
		return nil, false, nil
	}
	return children, complete, nil
}

func (s commandPlanningIgnoredSnapshot) fingerprint() string {
	var fingerprint strings.Builder
	fmt.Fprintf(&fingerprint, "truncated:%t\x00", s.truncated)
	for _, root := range s.roots {
		fmt.Fprintf(&fingerprint, "root:%s:skipped:%t\x00", filepath.ToSlash(root.path), root.skipped)
		if root.skipped {
			continue
		}
		for _, entry := range root.entries {
			digest := sha256.Sum256(entry.data)
			fmt.Fprintf(&fingerprint, "%s:%s:%o:%x:%s\x00", filepath.ToSlash(entry.path), entry.mode.Type(), entry.mode.Perm(), digest, entry.linkTarget)
		}
	}
	return fingerprint.String()
}

func (s commandPlanningIgnoredSnapshot) restore(ctx context.Context, workDir string) error {
	current, err := captureCommandPlanningIgnoredSnapshot(ctx, workDir)
	if err != nil {
		return err
	}
	before := make(map[string]commandPlanningIgnoredRoot, len(s.roots))
	after := make(map[string]commandPlanningIgnoredRoot, len(current.roots))
	paths := make(map[string]struct{}, len(s.roots)+len(current.roots))
	for _, root := range s.roots {
		before[root.path] = root
		paths[root.path] = struct{}{}
	}
	for _, root := range current.roots {
		after[root.path] = root
		paths[root.path] = struct{}{}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	var restoreErr error
	for _, path := range ordered {
		beforeRoot, hadBefore := before[path]
		afterRoot, hasAfter := after[path]
		if !hadBefore && hasAfter && s.truncated {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("refuse to remove ignored command planning source root %s omitted by the truncated before snapshot", path))
			continue
		}
		if hadBefore && !hasAfter && current.truncated {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("refuse to restore ignored command planning source root %s omitted by the truncated after snapshot", path))
			continue
		}
		if !hadBefore && hasAfter && afterRoot.skipped {
			cleanPath, err := commandPlanningTrackedPath(path)
			if err != nil {
				restoreErr = errors.Join(restoreErr, err)
				continue
			}
			if err := os.RemoveAll(filepath.Join(workDir, cleanPath)); err != nil {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("remove new oversized ignored command planning source root %s: %w", path, err))
			}
			continue
		}
		if (hadBefore && beforeRoot.skipped) || (hasAfter && afterRoot.skipped) {
			if hadBefore && hasAfter && beforeRoot.skipped && afterRoot.skipped {
				continue
			}
			restoreErr = errors.Join(restoreErr, fmt.Errorf("ignored command planning source root %s exceeded the restore budget", path))
			continue
		}
		if err := restoreCommandPlanningIgnoredRoot(workDir, beforeRoot, hadBefore, afterRoot, hasAfter); err != nil {
			restoreErr = errors.Join(restoreErr, err)
		}
	}
	return restoreErr
}

func restoreCommandPlanningIgnoredRoot(workDir string, before commandPlanningIgnoredRoot, hadBefore bool, after commandPlanningIgnoredRoot, hasAfter bool) error {
	beforeEntries := make(map[string]commandPlanningIgnoredEntry)
	afterEntries := make(map[string]commandPlanningIgnoredEntry)
	if hadBefore {
		for _, entry := range before.entries {
			beforeEntries[entry.path] = entry
		}
	}
	if hasAfter {
		for _, entry := range after.entries {
			afterEntries[entry.path] = entry
		}
	}
	removePaths := make([]string, 0)
	for path, entry := range afterEntries {
		beforeEntry, ok := beforeEntries[path]
		if !ok || beforeEntry.mode.Type() != entry.mode.Type() {
			removePaths = append(removePaths, path)
		}
	}
	sort.Slice(removePaths, func(i, j int) bool {
		return commandPlanningPathDepth(removePaths[i]) > commandPlanningPathDepth(removePaths[j])
	})
	for _, path := range removePaths {
		if err := os.Remove(filepath.Join(workDir, path)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove ignored command planning source creation %s: %w", path, err)
		}
	}

	entries := make([]commandPlanningIgnoredEntry, 0, len(beforeEntries))
	for _, entry := range beforeEntries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return commandPlanningPathDepth(entries[i].path) < commandPlanningPathDepth(entries[j].path)
	})
	for _, entry := range entries {
		if !entry.mode.IsDir() {
			continue
		}
		path := filepath.Join(workDir, entry.path)
		if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
			return fmt.Errorf("restore ignored command planning source directory %s: %w", entry.path, err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("prepare ignored command planning source directory %s: %w", entry.path, err)
		}
	}
	for _, entry := range entries {
		if entry.mode.IsDir() {
			continue
		}
		path := filepath.Join(workDir, entry.path)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("replace ignored command planning source path %s: %w", entry.path, err)
		}
		switch {
		case entry.mode.IsRegular():
			if err := os.WriteFile(path, entry.data, entry.mode.Perm()); err != nil {
				return fmt.Errorf("restore ignored command planning source file %s: %w", entry.path, err)
			}
		case entry.mode&os.ModeSymlink != 0:
			if err := os.Symlink(entry.linkTarget, path); err != nil {
				return fmt.Errorf("restore ignored command planning source symlink %s: %w", entry.path, err)
			}
		}
	}
	for _, entry := range entries {
		if entry.mode&os.ModeSymlink != 0 {
			continue
		}
		if err := os.Chmod(filepath.Join(workDir, entry.path), entry.mode.Perm()); err != nil {
			return fmt.Errorf("restore ignored command planning source mode %s: %w", entry.path, err)
		}
	}
	return nil
}

func commandPlanningPathDepth(path string) int {
	return strings.Count(filepath.Clean(path), string(filepath.Separator))
}
