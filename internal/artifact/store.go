// Package artifact owns owner-local immutable artifact files. The database
// registry records only metadata and paths; this package owns byte creation
// and verification before those bytes are consumed.
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

const commandOutputDirectory = "command-output"

// Store resolves artifact registry roots to owner-local filesystem roots.
// Evidence may use a configured external root, while command output always
// uses the managed run-artifact root.
type Store struct {
	runRoot      string
	evidenceRoot string
}

// NewStore constructs an artifact store for the application paths. It does
// not create any directories until an artifact is written.
func NewStore(p *paths.Paths, configuredEvidenceRoot string) (*Store, error) {
	if p == nil {
		return nil, fmt.Errorf("new artifact store: paths are nil")
	}
	runRoot, err := absoluteCleanPath(p.RunsDir())
	if err != nil {
		return nil, fmt.Errorf("new artifact store: run root: %w", err)
	}
	evidenceRoot, err := absoluteCleanPath(p.EvidenceRoot(configuredEvidenceRoot))
	if err != nil {
		return nil, fmt.Errorf("new artifact store: evidence root: %w", err)
	}
	return &Store{runRoot: runRoot, evidenceRoot: evidenceRoot}, nil
}

func absoluteCleanPath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("path is empty")
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// CreateCommandOutput writes one command attempt's output exactly once. Empty
// output is stored as a real zero-byte file rather than omitted.
func (s *Store) CreateCommandOutput(runID, attemptID string, output []byte) (artifact db.Artifact, err error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return db.Artifact{}, err
	}
	if err := validatePathComponent("attempt ID", attemptID); err != nil {
		return db.Artifact{}, err
	}
	if err := ensurePrivateDirectory(s.runRoot); err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: prepare runs root: %w", err)
	}
	runDir, err := ensurePrivateChildDirectory(s.runRoot, runID)
	if err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: prepare run directory: %w", err)
	}
	commandDir, err := ensurePrivateChildDirectory(runDir, commandOutputDirectory)
	if err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: prepare command output directory: %w", err)
	}

	relativePath := path.Join(runID, commandOutputDirectory, attemptID+".log")
	target := filepath.Join(commandDir, attemptID+".log")
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: create immutable file: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = file.Close()
			_ = os.Remove(target)
		}
	}()
	if err := protectArtifactFile(target); err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: protect file: %w", err)
	}
	if _, err := file.Write(output); err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: write file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: sync file: %w", err)
	}
	if err := file.Close(); err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: close file: %w", err)
	}
	if err := syncArtifactDirectory(commandDir); err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: sync output directory: %w", err)
	}
	published = true
	digest := sha256.Sum256(output)
	return db.Artifact{
		StorageRoot:  db.ArtifactStorageRootRun,
		RelativePath: relativePath,
		Purpose:      db.ArtifactPurposeCommandOutput,
		Label:        "Command output",
		Kind:         db.ArtifactKindCommandOutput,
		MediaType:    "text/plain",
		Encoding:     "utf-8",
		SHA256:       hex.EncodeToString(digest[:]),
		SourceBytes:  int64(len(output)),
		State:        db.ArtifactStateAvailable,
	}, nil
}

// Read validates a registered artifact's physical containment, file kind,
// byte count, and SHA-256 before returning its bytes.
func (s *Store) Read(artifact *db.Artifact) ([]byte, error) {
	if artifact == nil {
		return nil, fmt.Errorf("read artifact: artifact is nil")
	}
	if artifact.State != db.ArtifactStateAvailable {
		return nil, fmt.Errorf("read artifact: state %q is not readable", artifact.State)
	}
	if _, err := strictRelativePath(artifact.RelativePath); err != nil {
		return nil, fmt.Errorf("read artifact: relative path: %w", err)
	}
	if artifact.SourceBytes < 0 {
		return nil, fmt.Errorf("read artifact: source bytes are negative")
	}
	if !validSHA256(artifact.SHA256) {
		return nil, fmt.Errorf("read artifact: SHA-256 digest is invalid")
	}
	root, err := s.rootFor(artifact.StorageRoot)
	if err != nil {
		return nil, err
	}
	target, info, err := secureRegularArtifactPath(root, artifact.RelativePath)
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	if info.Size() != artifact.SourceBytes {
		return nil, fmt.Errorf("read artifact: source byte count mismatch: got %d, want %d", info.Size(), artifact.SourceBytes)
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		return nil, fmt.Errorf("read artifact: read file: %w", err)
	}
	if int64(len(contents)) != artifact.SourceBytes {
		return nil, fmt.Errorf("read artifact: byte count changed while reading")
	}
	digest := sha256.Sum256(contents)
	if hex.EncodeToString(digest[:]) != artifact.SHA256 {
		return nil, fmt.Errorf("read artifact: SHA-256 digest mismatch")
	}
	return contents, nil
}

func (s *Store) rootFor(storageRoot string) (string, error) {
	switch storageRoot {
	case db.ArtifactStorageRootRun:
		return s.runRoot, nil
	case db.ArtifactStorageRootEvidence:
		return s.evidenceRoot, nil
	default:
		return "", fmt.Errorf("read artifact: unsupported storage root %q", storageRoot)
	}
}

func validatePathComponent(label, value string) error {
	if strings.TrimSpace(value) == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\") || filepath.Base(value) != value {
		return fmt.Errorf("create command output: %s is not a safe path component", label)
	}
	return nil
}

func strictRelativePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("must be a non-empty normalized root-relative path")
	}
	normalized := path.Clean(value)
	if normalized != value || normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", fmt.Errorf("must stay within the supplied root")
	}
	return normalized, nil
}

func secureRegularArtifactPath(root, relativePath string) (string, fs.FileInfo, error) {
	if _, err := strictRelativePath(relativePath); err != nil {
		return "", nil, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, fmt.Errorf("resolve supplied root: %w", err)
	}
	current := root
	for _, component := range strings.Split(relativePath, "/") {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return "", nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("artifact path contains a symlink")
		}
	}
	info, err := os.Stat(current)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("artifact is not a regular file")
	}
	resolvedTarget, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", nil, fmt.Errorf("resolve artifact path: %w", err)
	}
	within, err := filepath.Rel(resolvedRoot, resolvedTarget)
	if err != nil {
		return "", nil, fmt.Errorf("compare supplied root: %w", err)
	}
	if within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) || filepath.IsAbs(within) {
		return "", nil, fmt.Errorf("artifact escapes supplied root")
	}
	return current, info, nil
}

func ensurePrivateDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("path is not a real directory")
	}
	return protectArtifactDirectory(directory)
}

func ensurePrivateChildDirectory(parent, name string) (string, error) {
	child := filepath.Join(parent, name)
	if err := os.Mkdir(child, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(child)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("path is not a real directory")
	}
	if err := protectArtifactDirectory(child); err != nil {
		return "", err
	}
	return child, nil
}

func validSHA256(value string) bool {
	if value != strings.ToLower(value) {
		return false
	}
	digest, err := hex.DecodeString(value)
	return err == nil && len(digest) == sha256.Size
}
