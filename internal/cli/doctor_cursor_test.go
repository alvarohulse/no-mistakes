package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

// TestDoctorCursorRequiresBothBinaries verifies the cursor agent row reflects the
// reality that cursor runs cursor-agent through the acpx shim, so both binaries
// must be present. With only cursor-agent installed, doctor must report cursor as
// not found and name the missing acpx binary, rather than a misleading green line.
func TestDoctorCursorRequiresBothBinaries(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	t.Setenv("NM_HOME", t.TempDir())

	binDir := t.TempDir()
	writeFakeBinary(t, binDir, "cursor-agent")
	// acpx is intentionally absent. Scope PATH to only our fake dir so an acpx
	// that happens to be installed on the host cannot mask the missing binary.
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	t.Logf("rendered `no-mistakes doctor` report:\n%s", out)

	if !strings.Contains(out, "cursor") {
		t.Fatalf("doctor report missing cursor agent entry:\n%s", out)
	}
	if !strings.Contains(out, "acpx") {
		t.Fatalf("doctor should name the missing acpx binary for cursor:\n%s", out)
	}
}

// TestDoctorCursorDetectedWithBothBinaries confirms the cursor row goes green and
// reports both binary paths when cursor-agent and acpx are both installed.
func TestDoctorCursorDetectedWithBothBinaries(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	t.Setenv("NM_HOME", t.TempDir())

	binDir := t.TempDir()
	cursorPath := writeFakeBinary(t, binDir, "cursor-agent")
	acpxPath := writeFakeBinary(t, binDir, "acpx")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	t.Logf("rendered `no-mistakes doctor` report:\n%s", out)

	for _, want := range []string{cursorPath, acpxPath} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor did not report cursor binary %q:\n%s", want, out)
		}
	}
}

// writeFakeBinary writes a stub executable that doctor's LookPath can resolve.
// doctor never executes it, so a no-op script is enough.
func writeFakeBinary(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		dst := filepath.Join(dir, name+".cmd")
		if err := os.WriteFile(dst, []byte("@echo off\r\nexit /b 0\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dst
	}
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}
