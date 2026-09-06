package artifact

import (
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
