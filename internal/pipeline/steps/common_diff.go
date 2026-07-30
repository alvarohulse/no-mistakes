package steps

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

type testFileChangeKind string

const (
	testFileCreated  testFileChangeKind = "created"
	testFileModified testFileChangeKind = "modified"
)

type testFileChange struct {
	Path string
	Kind testFileChangeKind
}

// isTestFile returns true if the file path matches common test file naming patterns.
func isTestFile(path string) bool {
	base := filepath.Base(path)
	if base == "" {
		return false
	}

	// Go: *_test.go
	if strings.HasSuffix(base, "_test.go") {
		return true
	}
	// Rust: *_test.rs
	if strings.HasSuffix(base, "_test.rs") {
		return true
	}
	// Python: test_*.py or *_test.py
	if strings.HasSuffix(base, ".py") {
		name := strings.TrimSuffix(base, ".py")
		if strings.HasPrefix(name, "test_") || strings.HasSuffix(name, "_test") {
			return true
		}
	}
	// Ruby: test_*.rb
	if strings.HasSuffix(base, ".rb") && strings.HasPrefix(filepath.Base(path), "test_") {
		return true
	}
	// Java: *Test.java or *Tests.java
	if strings.HasSuffix(base, "Test.java") || strings.HasSuffix(base, "Tests.java") {
		return true
	}
	// JS/TS: *.test.{js,ts,jsx,tsx} or *.spec.{js,ts,jsx,tsx}
	for _, ext := range []string{".js", ".ts", ".jsx", ".tsx"} {
		if strings.HasSuffix(base, ".test"+ext) || strings.HasSuffix(base, ".spec"+ext) {
			return true
		}
	}
	return false
}

// detectTestFileChanges returns test files created or modified in the working
// tree. NUL-delimited output preserves paths containing whitespace.
func detectTestFileChanges(ctx context.Context, dir string) ([]testFileChange, error) {
	changes := make(map[string]testFileChangeKind)

	untracked, err := git.Run(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z", "-t")
	if err != nil {
		return nil, fmt.Errorf("list untracked test files: %w", err)
	}
	for _, record := range splitNullRecords(untracked) {
		if len(record) < 2 || !isTestFile(record[2:]) {
			continue
		}
		changes[record[2:]] = testFileCreated
	}

	for _, args := range [][]string{
		{"diff", "--cached", "--name-status", "-z", "--diff-filter=AM"},
		{"diff", "--name-status", "-z", "--diff-filter=AM"},
	} {
		out, err := git.Run(ctx, dir, args...)
		if err != nil {
			return nil, fmt.Errorf("list changed test files: %w", err)
		}
		records := splitNullRecords(out)
		for i := 0; i+1 < len(records); i += 2 {
			status, path := records[i], records[i+1]
			if !isTestFile(path) {
				continue
			}
			if status == "A" {
				changes[path] = testFileCreated
				continue
			}
			if _, created := changes[path]; !created && status == "M" {
				changes[path] = testFileModified
			}
		}
	}

	paths := make([]string, 0, len(changes))
	for path := range changes {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	result := make([]testFileChange, 0, len(paths))
	for _, path := range paths {
		result = append(result, testFileChange{Path: path, Kind: changes[path]})
	}
	return result, nil
}

func splitNullRecords(out string) []string {
	if out == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
}

// matchIgnorePattern checks if a file path matches an ignore pattern.
// Patterns follow gitignore-like semantics:
//   - No slash: match against filename only (e.g., "*.generated.go" matches "pkg/foo.generated.go")
//   - Ends with "/**": match any file under that directory (e.g., "vendor/**" matches "vendor/pkg/foo.go")
//   - Otherwise: filepath.Match against the full path
func matchIgnorePattern(path, pattern string) bool {
	// "vendor/**" → matches anything under "vendor/"
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return path == prefix || strings.HasPrefix(path, prefix+"/")
	}
	// No slash in pattern → match against basename only
	if !strings.Contains(pattern, "/") {
		base := filepath.Base(path)
		matched, _ := filepath.Match(pattern, base)
		return matched
	}
	// Full path match
	matched, _ := filepath.Match(pattern, path)
	return matched
}

// filterDiff removes diff sections for files matching any of the ignore patterns.
// Input is a unified diff; output is the same diff with matching file sections removed.
// Returns the original diff unchanged if patterns is empty.
func filterDiff(diff string, patterns []string) string {
	if len(patterns) == 0 || diff == "" {
		return diff
	}

	lines := strings.Split(diff, "\n")
	var result []string
	skip := false

	for _, line := range lines {
		// Detect start of a new file section
		if strings.HasPrefix(line, "diff --git ") {
			// Extract path from "diff --git a/<path> b/<path>"
			path := extractDiffPath(line)
			skip = false
			for _, p := range patterns {
				if matchIgnorePattern(path, p) {
					skip = true
					break
				}
			}
		}
		if !skip {
			result = append(result, line)
		}
	}

	return strings.Join(result, "\n")
}

// extractDiffPath extracts the file path from a "diff --git a/<path> b/<path>" header.
// For non-rename diffs both paths are identical, so we derive the path length from
// the known structure rather than splitting on " b/" which could appear in filenames.
func extractDiffPath(diffLine string) string {
	const prefix = "diff --git a/"
	rest := strings.TrimPrefix(diffLine, prefix)
	if rest == diffLine {
		return ""
	}
	// Non-rename: rest is "<path> b/<path>" where both paths are equal.
	// Total length = 2*pathLen + len(" b/") = 2*pathLen + 3.
	pathLen := (len(rest) - 3) / 2
	if pathLen > 0 && pathLen+3 <= len(rest) && rest[pathLen:pathLen+3] == " b/" {
		return rest[:pathLen]
	}
	// Fallback for renames or unexpected format: split on first " b/".
	parts := strings.SplitN(rest, " b/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return ""
}
