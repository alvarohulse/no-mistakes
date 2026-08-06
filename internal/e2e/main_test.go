//go:build e2e

package e2e

import (
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
)

// TestMain recovers any stale temporary-daemon inventory left by a prior
// interrupted suite, runs the package tests, then reaps again on exit.
//
// This is the in-process recovery boundary. It does not survive SIGKILL of
// the test process; scripts/e2e.sh EXIT/INT/TERM trap covers that case for
// the wrapper shell, and the next suite's pre-reap covers SIGKILL of the
// wrapper itself via the on-disk inventory.
func TestMain(m *testing.M) {
	// Ambient machine-local repo config must not leak into the suite. The
	// harness builds a throwaway repo per test, so an inherited NM_REPO_CONFIG
	// (for example when the suite itself runs inside a gated no-mistakes run)
	// binds to a different repository and the daemon refuses every push with
	// "registered repository remote cannot be validated for NM_REPO_CONFIG".
	// Same reasoning as the GIT_CONFIG_COUNT unset in git-touching packages; a
	// test that wants the overlay sets it explicitly with t.Setenv.
	_ = os.Unsetenv("NM_REPO_CONFIG")
	if inv, err := e2edaemon.Open(); err == nil {
		_ = inv.ReapAll()
	}
	code := m.Run()
	if inv, err := e2edaemon.Open(); err == nil {
		_ = inv.ReapAll()
	}
	os.Exit(code)
}
