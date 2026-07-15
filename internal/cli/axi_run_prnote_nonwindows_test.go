//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResolvePRNoteStopsReadingOversizedStream(t *testing.T) {
	noteFile := filepath.Join(t.TempDir(), "note.pipe")
	if err := syscall.Mkfifo(noteFile, 0o600); err != nil {
		t.Fatalf("create note pipe: %v", err)
	}

	releaseWriter := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseWriter)
		}
	}()
	writerDone := make(chan error, 1)
	go func() {
		file, err := os.OpenFile(noteFile, os.O_WRONLY, 0)
		if err == nil {
			_, err = file.Write([]byte(strings.Repeat("x", maxPRNotePushOptionBytes+1)))
			<-releaseWriter
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
		}
		writerDone <- err
	}()

	result := make(chan error, 1)
	go func() {
		_, err := resolvePRNote("", noteFile)
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "too large for the push-option transport") {
			t.Fatalf("resolvePRNote() error = %v, want actionable transport-size error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolvePRNote() did not stop after reading the transport limit")
	}
	close(releaseWriter)
	released = true
	if err := <-writerDone; err != nil {
		t.Fatalf("write note pipe: %v", err)
	}
}
