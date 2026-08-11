//go:build windows

package ipc_test

import (
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// TestServerCloseRemovesEndpointFileHeldByRacingReader is the regression test
// for a Windows-only daemon-stop failure. The Windows transport advertises the
// daemon through a regular endpoint file, and its removal is what tells a
// stopping caller the daemon is gone. Windows refuses DeleteFile with a
// sharing violation while any other handle on the file is open without
// FILE_SHARE_DELETE - which is how the Go runtime opens files - so a dial that
// races the daemon's listener close used to defeat the one best-effort Remove
// a shutdown performs, stranding the endpoint file forever. `daemon stop` then
// polled an ambiguous "connect to daemon socket" error for its whole budget
// and failed in the kill-by-PID fallback instead of reporting the graceful
// stop that had already happened.
func TestServerCloseRemovesEndpointFileHeldByRacingReader(t *testing.T) {
	endpoint := socketPath(t)
	srv := ipc.NewServer()
	if err := srv.Listen(endpoint); err != nil {
		t.Fatalf("listen: %v", err)
	}

	// Hold the endpoint file exactly the way a racing dial does: an ordinary
	// os.Open handle is enough to make DeleteFile fail while it is alive.
	held, err := os.Open(endpoint)
	if err != nil {
		t.Fatalf("open endpoint: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(200 * time.Millisecond)
		held.Close()
	}()
	t.Cleanup(func() {
		<-released
	})

	srv.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(endpoint); os.IsNotExist(err) {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatal("endpoint file survived server close while a racing reader held it open; a stopping caller can never observe the daemon disappearing")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
