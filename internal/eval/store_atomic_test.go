package eval

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWritePrivateFileAtomicallyReplacesDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "labels.json")
	if err := writePrivateFile(path, []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldLink := filepath.Join(dir, "old-labels.json")
	if err := os.Link(path, oldLink); err != nil {
		t.Fatal(err)
	}

	if err := writePrivateFile(path, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else if string(got) != "new\n" {
		t.Fatalf("destination = %q, want new contents", got)
	}
	if got, err := os.ReadFile(oldLink); err != nil {
		t.Fatal(err)
	} else if string(got) != "old\n" {
		t.Fatalf("old inode = %q, want old contents after atomic replacement", got)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil {
			t.Fatal(err)
		} else if info.Mode().Perm() != 0o600 {
			t.Fatalf("destination mode = %o, want 600", info.Mode().Perm())
		}
	}
	if leftovers, err := filepath.Glob(filepath.Join(dir, ".labels.json.tmp-*")); err != nil {
		t.Fatal(err)
	} else if len(leftovers) != 0 {
		t.Fatalf("temporary files remain after replacement: %v", leftovers)
	}
}

func TestWritePrivateFileCleansTemporaryFileWhenPublishFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "labels.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := writePrivateFile(path, []byte("new\n")); err == nil {
		t.Fatal("writePrivateFile succeeded over a directory")
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if !info.IsDir() {
		t.Fatalf("destination changed after failed publish: %v", info.Mode())
	}
	if leftovers, err := filepath.Glob(filepath.Join(dir, ".labels.json.tmp-*")); err != nil {
		t.Fatal(err)
	} else if len(leftovers) != 0 {
		t.Fatalf("temporary files remain after failed publish: %v", leftovers)
	}
}

func TestWriteJSONMarshalFailurePreservesDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "labels.json")
	if err := writePrivateFile(path, []byte("valid\n")); err != nil {
		t.Fatal(err)
	}

	if err := writeJSON(path, make(chan int)); err == nil {
		t.Fatal("writeJSON accepted an unsupported value")
	}
	if got, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else if string(got) != "valid\n" {
		t.Fatalf("destination = %q, want original contents after failed encode", got)
	}
}
