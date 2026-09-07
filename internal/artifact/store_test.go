package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestStoreCreatesAndReadsOneImmutableCommandOutput(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	store, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}

	artifact, err := store.CreateCommandOutput("run-1", "attempt-1", []byte("command output\n"))
	if err != nil {
		t.Fatalf("create command output: %v", err)
	}
	if artifact.StorageRoot != db.ArtifactStorageRootRun || artifact.RelativePath != "run-1/command-output/attempt-1.log" || artifact.SourceBytes != int64(len("command output\n")) || artifact.SHA256 == "" {
		t.Fatalf("command output artifact = %+v", artifact)
	}
	contents, err := store.Read(&artifact)
	if err != nil {
		t.Fatalf("read stored output: %v", err)
	}
	if got, want := string(contents), "command output\n"; got != want {
		t.Fatalf("stored contents = %q, want %q", got, want)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{p.RunsDir(), filepath.Join(p.RunsDir(), "run-1"), filepath.Join(p.RunsDir(), "run-1", "command-output")} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o700 {
				t.Fatalf("directory mode for %s = %o, want 700", path, info.Mode().Perm())
			}
		}
		info, err := os.Stat(filepath.Join(p.RunsDir(), filepath.FromSlash(artifact.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("output mode = %o, want 600", info.Mode().Perm())
		}
	}
	if _, err := store.CreateCommandOutput("run-1", "attempt-1", []byte("replacement")); err == nil {
		t.Fatal("second command output creation succeeded")
	}
}

func TestStoreCreatesAndReadsEmptyCommandOutput(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	store, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateCommandOutput("run-empty", "attempt-empty", nil)
	if err != nil {
		t.Fatalf("create empty command output: %v", err)
	}
	if artifact.SourceBytes != 0 {
		t.Fatalf("empty output source bytes = %d", artifact.SourceBytes)
	}
	contents, err := store.Read(&artifact)
	if err != nil {
		t.Fatalf("read empty command output: %v", err)
	}
	if len(contents) != 0 {
		t.Fatalf("empty command output = %q", contents)
	}
}

func TestStoreCreatesAndReadsBinaryCommandOutput(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	store, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	output := []byte{0xff, 0x00, 'o', 'k', '\n'}

	artifact, err := store.CreateCommandOutput("run-binary", "attempt-binary", output)
	if err != nil {
		t.Fatalf("create binary command output: %v", err)
	}
	digest := sha256.Sum256(output)
	if artifact.MediaType != "application/octet-stream" || artifact.Encoding != "binary" || artifact.SHA256 != hex.EncodeToString(digest[:]) || artifact.SourceBytes != int64(len(output)) {
		t.Fatalf("binary command output metadata = %+v", artifact)
	}
	contents, err := store.Read(&artifact)
	if err != nil {
		t.Fatalf("read binary command output: %v", err)
	}
	if !bytes.Equal(contents, output) {
		t.Fatalf("binary command output = %x, want %x", contents, output)
	}
}

func TestStoreClassifiesNULCommandOutputAsBinary(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	store, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateCommandOutput("run-nul", "attempt-nul", []byte("before\x00after"))
	if err != nil {
		t.Fatalf("create NUL command output: %v", err)
	}
	if artifact.MediaType != "application/octet-stream" || artifact.Encoding != "binary" {
		t.Fatalf("NUL command output metadata = %+v", artifact)
	}
}

func TestStoreCreatesCommandOutputThroughOpenedDirectoryAfterPathSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows developer mode or elevated privileges")
	}
	p := paths.WithRoot(t.TempDir())
	store, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	runDir := filepath.Join(p.RunsDir(), "run-swap")
	originalDirectory := filepath.Join(runDir, commandOutputDirectory)
	relocatedDirectory := filepath.Join(runDir, "relocated-command-output")
	store.afterCommandDirectoryOpen = func() {
		if err := os.Rename(originalDirectory, relocatedDirectory); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, originalDirectory); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := store.CreateCommandOutput("run-swap", "attempt-swap", []byte("inside")); err != nil {
		t.Fatalf("create output after path swap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "attempt-swap.log")); !os.IsNotExist(err) {
		t.Fatalf("path swap wrote outside the run root: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(relocatedDirectory, "attempt-swap.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "inside" {
		t.Fatalf("output through relocated directory = %q", contents)
	}
}

func TestStoreIndexesExistingEvidenceFileWithoutChangingIt(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	store, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}

	contents := []byte("<h1>tested checkout</h1>\n")
	runID := "run-evidence"
	evidencePath := filepath.Join(p.RunEvidenceDir("", runID), "rendered", "checkout.html")
	if err := os.MkdirAll(filepath.Dir(evidencePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidencePath, contents, 0o644); err != nil {
		t.Fatal(err)
	}

	indexed, err := store.IndexEvidenceFile(runID, evidencePath)
	if err != nil {
		t.Fatalf("index evidence file: %v", err)
	}
	if indexed.StorageRoot != db.ArtifactStorageRootEvidence || indexed.RelativePath != "run-evidence/rendered/checkout.html" || indexed.SourceBytes != int64(len(contents)) || indexed.MediaType != "text/html" || indexed.Encoding != "utf-8" {
		t.Fatalf("indexed evidence = %+v", indexed)
	}
	stored, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, contents) {
		t.Fatalf("evidence contents changed: got %q, want %q", stored, contents)
	}
	read, err := store.Read(&indexed)
	if err != nil {
		t.Fatalf("read indexed evidence: %v", err)
	}
	if !bytes.Equal(read, contents) {
		t.Fatalf("indexed contents = %q, want %q", read, contents)
	}
}

func TestStoreIndexesEvidenceAcrossUTF8ReadBoundaries(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	store, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	contents := append(bytes.Repeat([]byte("a"), 32*1024-1), []byte("é")...)
	runID := "run-evidence-boundary"
	evidencePath := filepath.Join(p.RunEvidenceDir("", runID), "report.txt")
	if err := os.MkdirAll(filepath.Dir(evidencePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidencePath, contents, 0o644); err != nil {
		t.Fatal(err)
	}

	indexed, err := store.IndexEvidenceFile(runID, evidencePath)
	if err != nil {
		t.Fatalf("index evidence file: %v", err)
	}
	want := sha256.Sum256(contents)
	if indexed.MediaType != "text/plain" || indexed.Encoding != "utf-8" || indexed.SourceBytes != int64(len(contents)) || indexed.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("indexed evidence metadata = %+v", indexed)
	}
}

func TestStoreRejectsEvidenceOutsideItsRunRootAndSymlinks(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	store, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	runID := "run-evidence"
	runDir := p.RunEvidenceDir("", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.IndexEvidenceFile(runID, outside); err == nil {
		t.Fatal("outside evidence file was indexed")
	}
	if _, err := store.IndexEvidenceFile(runID, filepath.Join(runDir, "..", "outside.txt")); err == nil {
		t.Fatal("traversal evidence file was indexed")
	}
	if _, err := store.IndexEvidenceFile(runID, filepath.Join(runDir, "missing.txt")); err == nil {
		t.Fatal("missing evidence file was indexed")
	}
	if _, err := store.IndexEvidenceFile(runID, runDir); err == nil {
		t.Fatal("evidence directory was indexed as a file")
	}
	if runtime.GOOS == "windows" {
		return
	}
	escapedDir := filepath.Join(runDir, "escaped")
	if err := os.Symlink(filepath.Dir(outside), escapedDir); err != nil {
		t.Fatal(err)
	}
	if _, err := store.IndexEvidenceFile(runID, filepath.Join(escapedDir, filepath.Base(outside))); err == nil {
		t.Fatal("symlinked evidence path was indexed")
	}
}

func TestStoreIndexesEvidenceThroughValidatedDescriptorAfterPathSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows developer mode or elevated privileges")
	}
	p := paths.WithRoot(t.TempDir())
	store, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}

	const runID = "run-swap"
	inside := []byte("inside!")
	outside := []byte("secret!")
	target := filepath.Join(p.RunEvidenceDir("", runID), "report.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, inside, 0o644); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.txt")
	if err := os.WriteFile(external, outside, 0o644); err != nil {
		t.Fatal(err)
	}
	store.afterArtifactDescriptorOpen = func() {
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, target); err != nil {
			t.Fatal(err)
		}
	}

	indexed, err := store.IndexEvidenceFile(runID, target)
	if err != nil {
		t.Fatalf("index evidence after swap: %v", err)
	}
	want := sha256.Sum256(inside)
	if indexed.SourceBytes != int64(len(inside)) || indexed.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("indexed swapped evidence = %+v, want bytes and digest for %q", indexed, inside)
	}
}

func TestStoreReadsArtifactThroughValidatedDescriptorAfterPathSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows developer mode or elevated privileges")
	}
	p := paths.WithRoot(t.TempDir())
	store, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}

	inside := []byte("inside!")
	outside := []byte("secret!")
	target := filepath.Join(p.RunsDir(), "run-swap", "output.log")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, inside, 0o600); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.txt")
	if err := os.WriteFile(external, outside, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(inside)
	artifact := &db.Artifact{
		StorageRoot:  db.ArtifactStorageRootRun,
		RelativePath: "run-swap/output.log",
		SourceBytes:  int64(len(inside)),
		SHA256:       hex.EncodeToString(digest[:]),
		State:        db.ArtifactStateAvailable,
	}
	store.afterArtifactDescriptorOpen = func() {
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, target); err != nil {
			t.Fatal(err)
		}
	}

	contents, err := store.Read(artifact)
	if err != nil {
		t.Fatalf("read artifact after swap: %v", err)
	}
	if !bytes.Equal(contents, inside) {
		t.Fatalf("read swapped artifact = %q, want %q", contents, inside)
	}
}

func TestStoreRejectsUnsafeMissingAndTamperedArtifacts(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	store, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(readableArtifact("../outside.log")); err == nil {
		t.Fatal("traversal artifact was readable")
	}

	root := filepath.Join(p.RunsDir(), "run-checks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(readableArtifact("run-checks/missing.log")); err == nil {
		t.Fatal("missing artifact was readable")
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(readableArtifact("run-checks/directory")); err == nil {
		t.Fatal("non-regular artifact was readable")
	}

	data := []byte("recorded output")
	path := filepath.Join(root, "recorded.log")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	valid := db.Artifact{
		StorageRoot:  db.ArtifactStorageRootRun,
		RelativePath: "run-checks/recorded.log",
		SourceBytes:  int64(len(data)),
		SHA256:       hex.EncodeToString(digest[:]),
		State:        db.ArtifactStateAvailable,
	}
	if err := os.WriteFile(path, append(data, '!'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(&valid); err == nil {
		t.Fatal("byte-size drift artifact was readable")
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	valid.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := store.Read(&valid); err == nil {
		t.Fatal("digest mismatch artifact was readable")
	}

	if runtime.GOOS == "windows" {
		return
	}
	escapeRoot := t.TempDir()
	if err := os.Symlink(escapeRoot, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(readableArtifact("run-checks/escape/output.log")); err == nil {
		t.Fatal("symlink escape artifact was readable")
	}
}

func readableArtifact(relativePath string) *db.Artifact {
	digest := sha256.Sum256(nil)
	return &db.Artifact{
		StorageRoot:  db.ArtifactStorageRootRun,
		RelativePath: relativePath,
		SHA256:       hex.EncodeToString(digest[:]),
		State:        db.ArtifactStateAvailable,
	}
}
