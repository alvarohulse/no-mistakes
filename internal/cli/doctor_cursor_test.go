package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

func TestDoctorACPAliasRequiresBothBinaries(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	t.Setenv("NM_HOME", t.TempDir())

	binDir := t.TempDir()
	writeFakeBinary(t, binDir, "cursor-agent")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}

	if !strings.Contains(out, "cursor") {
		t.Fatalf("doctor report missing cursor alias entry:\n%s", out)
	}
	if !strings.Contains(out, "acpx") {
		t.Fatalf("doctor should name the missing acpx binary for cursor alias:\n%s", out)
	}
}

func TestDoctorACPAliasDetectedWithBothBinaries(t *testing.T) {
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

	for _, want := range []string{cursorPath, acpxPath} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor did not report cursor alias binary %q:\n%s", want, out)
		}
	}
}

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
