// Package artifact owns owner-local immutable artifact files. The database
// registry records only metadata and paths; this package owns byte creation
// and verification before those bytes are consumed.
package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

const commandOutputDirectory = "command-output"

// Store resolves artifact registry roots to owner-local filesystem roots.
// Evidence may use a configured external root, while command output always
// uses the managed run-artifact root.
type Store struct {
	runRoot                     string
	evidenceRoot                string
	afterArtifactDescriptorOpen func()
	afterCommandDirectoryOpen   func()
}

// NewStore constructs an artifact store for the application paths. It does
// not create any directories until an artifact is written.
func NewStore(p *paths.Paths, configuredEvidenceRoot string) (*Store, error) {
	if p == nil {
		return nil, fmt.Errorf("new artifact store: paths are nil")
	}
	runRoot, err := canonicalArtifactRoot(p.RunsDir())
	if err != nil {
		return nil, fmt.Errorf("new artifact store: run root: %w", err)
	}
	evidenceRoot, err := canonicalArtifactRoot(p.EvidenceRoot(configuredEvidenceRoot))
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

// canonicalArtifactRoot resolves aliases in the existing parent path while
// preserving the artifact root itself for descriptor-based no-follow checks.
// macOS commonly exposes temporary directories through /var, which is a
// system symlink to /private/var; opening that alias with O_NOFOLLOW would
// otherwise reject a valid managed root before any artifact can be written.
func canonicalArtifactRoot(value string) (string, error) {
	root, err := absoluteCleanPath(value)
	if err != nil {
		return "", err
	}
	parent, err := canonicalExistingDirectory(filepath.Dir(root))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(root)), nil
}

// canonicalExistingDirectory resolves the longest existing directory prefix,
// retaining any missing suffix so NewStore remains side-effect free.
func canonicalExistingDirectory(value string) (string, error) {
	current := value
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			info, err := os.Stat(resolved)
			if err != nil {
				return "", err
			}
			if !info.IsDir() {
				return "", fmt.Errorf("supplied root parent %q is not a directory", current)
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
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
	relativePath := path.Join(runID, commandOutputDirectory, attemptID+".log")
	outputFile, err := createCommandOutputFile(s.runRoot, runID, attemptID+".log", s.afterCommandDirectoryOpen)
	if err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: create immutable file: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = outputFile.discard()
		}
	}()
	if err := outputFile.protect(); err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: protect file: %w", err)
	}
	if written, err := outputFile.file.Write(output); err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: write file: %w", err)
	} else if written != len(output) {
		return db.Artifact{}, fmt.Errorf("create command output: write file: %w", io.ErrShortWrite)
	}
	if err := outputFile.file.Sync(); err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: sync file: %w", err)
	}
	if err := outputFile.closeAndSync(); err != nil {
		return db.Artifact{}, fmt.Errorf("create command output: close output file: %w", err)
	}
	published = true
	digest := sha256.Sum256(output)
	mediaType, encoding := commandOutputFormat(output)
	return db.Artifact{
		StorageRoot:  db.ArtifactStorageRootRun,
		RelativePath: relativePath,
		Purpose:      db.ArtifactPurposeCommandOutput,
		Label:        "Command output",
		Kind:         db.ArtifactKindCommandOutput,
		MediaType:    mediaType,
		Encoding:     encoding,
		SHA256:       hex.EncodeToString(digest[:]),
		SourceBytes:  int64(len(output)),
		State:        db.ArtifactStateAvailable,
	}, nil
}

func commandOutputFormat(output []byte) (mediaType, encoding string) {
	if looksLikeTextArtifact(output) {
		return "text/plain", "utf-8"
	}
	return "application/octet-stream", "binary"
}

// IndexEvidenceFile reads an existing test-evidence file without moving or
// modifying it, returning verified metadata rooted at the configured evidence
// directory. The file must belong to the named run and every path component
// below the evidence root must be a real directory or regular file.
func (s *Store) IndexEvidenceFile(runID, reportedPath string) (db.Artifact, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return db.Artifact{}, err
	}
	relativePath, err := s.evidenceRelativePath(runID, reportedPath)
	if err != nil {
		return db.Artifact{}, fmt.Errorf("index evidence file: %w", err)
	}
	metadata, err := s.inspectRegularArtifactFile(s.evidenceRoot, relativePath)
	if err != nil {
		return db.Artifact{}, fmt.Errorf("index evidence file: %w", err)
	}
	mediaType, encoding := evidenceFileFormat(relativePath, metadata.contentTypeSniff, metadata.isText)
	return db.Artifact{
		StorageRoot:  db.ArtifactStorageRootEvidence,
		RelativePath: relativePath,
		MediaType:    mediaType,
		Encoding:     encoding,
		SHA256:       metadata.sha256,
		SourceBytes:  metadata.sourceBytes,
		State:        db.ArtifactStateAvailable,
	}, nil
}

func (s *Store) evidenceRelativePath(runID, reportedPath string) (string, error) {
	if strings.TrimSpace(reportedPath) == "" {
		return "", fmt.Errorf("evidence path is empty")
	}
	target := reportedPath
	if !filepath.IsAbs(target) {
		target = filepath.Join(s.evidenceRoot, runID, target)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve reported path: %w", err)
	}
	absTarget, err = canonicalArtifactRoot(absTarget)
	if err != nil {
		return "", fmt.Errorf("resolve reported path: %w", err)
	}
	relativePath, err := filepath.Rel(s.evidenceRoot, filepath.Clean(absTarget))
	if err != nil {
		return "", fmt.Errorf("compare evidence root: %w", err)
	}
	normalized, err := strictRelativePath(filepath.ToSlash(relativePath))
	if err != nil {
		return "", fmt.Errorf("evidence path must stay within the evidence root: %w", err)
	}
	if normalized == runID || !strings.HasPrefix(normalized, runID+"/") {
		return "", fmt.Errorf("evidence path must stay within run %q", runID)
	}
	return normalized, nil
}

func evidenceFileFormat(filePath string, contentTypeSniff []byte, isText bool) (mediaType, encoding string) {
	mediaType = mime.TypeByExtension(strings.ToLower(filepath.Ext(filePath)))
	if mediaType == "" {
		mediaType = http.DetectContentType(contentTypeSniff)
	}
	if parsed, _, err := mime.ParseMediaType(mediaType); err == nil && parsed != "" {
		mediaType = parsed
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if isText {
		return mediaType, "utf-8"
	}
	return mediaType, "binary"
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
	contents, info, err := s.readRegularArtifactFile(root, artifact.RelativePath)
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	if info.Size() != artifact.SourceBytes {
		return nil, fmt.Errorf("read artifact: source byte count mismatch: got %d, want %d", info.Size(), artifact.SourceBytes)
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

// readRegularArtifactFile reads only the descriptor returned by the
// no-follow path walk. The test hook models an agent replacing the pathname
// after descriptor validation; it cannot change the already-open file.
func (s *Store) readRegularArtifactFile(root, relativePath string) ([]byte, os.FileInfo, error) {
	file, info, err := openRegularArtifactFile(root, relativePath)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	if s.afterArtifactDescriptorOpen != nil {
		s.afterArtifactDescriptorOpen()
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, fmt.Errorf("read file: %w", err)
	}
	return contents, info, nil
}

const contentTypeSniffBytes = 512

type artifactFileMetadata struct {
	contentTypeSniff []byte
	isText           bool
	sha256           string
	sourceBytes      int64
}

func (s *Store) inspectRegularArtifactFile(root, relativePath string) (artifactFileMetadata, error) {
	file, info, err := openRegularArtifactFile(root, relativePath)
	if err != nil {
		return artifactFileMetadata{}, err
	}
	defer file.Close()
	if s.afterArtifactDescriptorOpen != nil {
		s.afterArtifactDescriptorOpen()
	}

	digest := sha256.New()
	inspector := textArtifactInspector{validUTF8: true}
	sniff := make([]byte, 0, contentTypeSniffBytes)
	buffer := make([]byte, 32*1024)
	var sourceBytes int64
	for {
		read, readErr := file.Read(buffer)
		if read > 0 {
			chunk := buffer[:read]
			if remaining := contentTypeSniffBytes - len(sniff); remaining > 0 {
				if remaining > len(chunk) {
					remaining = len(chunk)
				}
				sniff = append(sniff, chunk[:remaining]...)
			}
			if _, err := digest.Write(chunk); err != nil {
				return artifactFileMetadata{}, fmt.Errorf("digest file: %w", err)
			}
			inspector.Write(chunk)
			sourceBytes += int64(read)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return artifactFileMetadata{}, fmt.Errorf("read file: %w", readErr)
		}
		if read == 0 {
			return artifactFileMetadata{}, fmt.Errorf("read file: %w", io.ErrNoProgress)
		}
	}
	if sourceBytes != info.Size() {
		return artifactFileMetadata{}, fmt.Errorf("file changed while reading")
	}
	return artifactFileMetadata{
		contentTypeSniff: sniff,
		isText:           inspector.IsText(),
		sha256:           hex.EncodeToString(digest.Sum(nil)),
		sourceBytes:      sourceBytes,
	}, nil
}

type textArtifactInspector struct {
	hasNUL    bool
	pending   []byte
	validUTF8 bool
}

func looksLikeTextArtifact(contents []byte) bool {
	return bytes.IndexByte(contents, 0) == -1 && utf8.Valid(contents)
}

func (i *textArtifactInspector) Write(contents []byte) {
	if bytes.IndexByte(contents, 0) >= 0 {
		i.hasNUL = true
	}
	if !i.validUTF8 {
		return
	}
	if len(i.pending) > 0 {
		for len(contents) > 0 && !utf8.FullRune(i.pending) {
			i.pending = append(i.pending, contents[0])
			contents = contents[1:]
		}
		if !utf8.FullRune(i.pending) {
			return
		}
		if !utf8.Valid(i.pending) {
			i.validUTF8 = false
			return
		}
		i.pending = i.pending[:0]
	}
	complete := completeUTF8Prefix(contents)
	if !utf8.Valid(contents[:complete]) {
		i.validUTF8 = false
		return
	}
	i.pending = append(i.pending[:0], contents[complete:]...)
}

func (i *textArtifactInspector) IsText() bool {
	return i.validUTF8 && !i.hasNUL && len(i.pending) == 0
}

func completeUTF8Prefix(contents []byte) int {
	if len(contents) == 0 {
		return 0
	}
	start := len(contents) - 1
	for start > 0 && contents[start]&0xc0 == 0x80 {
		start--
	}
	if !utf8.FullRune(contents[start:]) {
		return start
	}
	return len(contents)
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

func validSHA256(value string) bool {
	if value != strings.ToLower(value) {
		return false
	}
	digest, err := hex.DecodeString(value)
	return err == nil && len(digest) == sha256.Size
}
