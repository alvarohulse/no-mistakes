package prbody

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunHookDecodesFormatterStdout(t *testing.T) {
	t.Parallel()
	requireShell(t)

	result, err := RunHook(context.Background(), HookOptions{
		Command:  `cat > /dev/null; printf '%s\n' '{"version":1,"sections":[{"id":"custom","content":"## Custom\n\nbody"}]}'`,
		Contract: Sample(),
	})
	if err != nil {
		t.Fatalf("RunHook: %v", err)
	}
	if got := result.Patches.Sections[0].Content; got != "## Custom\n\nbody" {
		t.Fatalf("content = %q", got)
	}
}

func TestRunHookReturnsOwnedSectionPatches(t *testing.T) {
	t.Parallel()
	requireShell(t)

	result, err := RunHook(context.Background(), HookOptions{
		Command:  `cat > /dev/null; printf '%s\n' '{"version":1,"sections":[{"id":"summary","content":"## Summary\n\nformatted"}]}'`,
		Contract: Sample(),
	})
	if err != nil {
		t.Fatalf("RunHook: %v", err)
	}
	if result.Patches.Version != PatchVersion || len(result.Patches.Sections) != 1 {
		t.Fatalf("patches = %+v", result.Patches)
	}
	if got := result.Patches.Sections[0]; got.ID != "summary" || got.Content != "## Summary\n\nformatted" {
		t.Fatalf("section = %+v", got)
	}
}

func TestRunHookRejectsAFullBodyReplacement(t *testing.T) {
	t.Parallel()
	requireShell(t)

	_, err := RunHook(context.Background(), HookOptions{
		Command:  `cat > /dev/null; printf '%s\n' '{"version":1,"body":"replacement","sections":[{"id":"summary","content":"x"}]}'`,
		Contract: Sample(),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v, want full-body field rejection", err)
	}
}

func TestRunHookReceivesContractOnStdin(t *testing.T) {
	t.Parallel()
	requireShell(t)

	// The formatter's only input is stdin; if the contract is not piped, the
	// grep finds nothing and the command exits non-zero.
	result, err := RunHook(context.Background(), HookOptions{
		Command:  `payload=$(cat); printf '%s' "$payload" | grep -q '"version":4' && printf '%s' "$payload" | grep -q '"costs"' && printf '%s\n' '{"version":1,"sections":[{"id":"summary","content":"ok"}]}'`,
		Contract: Sample(),
	})
	if err != nil {
		t.Fatalf("RunHook: %v", err)
	}
	if got := result.Patches.Sections[0].Content; got != "ok" {
		t.Fatalf("content = %q, want the formatter to have seen the contract", got)
	}
}

func TestRunHookRejectsFailureModes(t *testing.T) {
	t.Parallel()
	requireShell(t)

	tests := []struct {
		name    string
		command string
		timeout time.Duration
		want    string
	}{
		{"non-zero exit", "cat > /dev/null; echo boom >&2; exit 3", 0, "exited 3"},
		{"empty output", "cat > /dev/null; exit 0", 0, "wrote no patches"},
		{"whitespace-only output", "cat > /dev/null; printf '   \\n\\n'", 0, "wrote no patches"},
		{"timeout", "cat > /dev/null; sleep 30", 200 * time.Millisecond, "timed out"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := RunHook(context.Background(), HookOptions{
				Command:  tt.command,
				Contract: Sample(),
				Timeout:  tt.timeout,
			})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestRunHookIncludesFormatterDiagnosticsInError(t *testing.T) {
	t.Parallel()
	requireShell(t)

	// A formatter's own explanation is the useful part of a failure; losing it
	// leaves an author with an exit code and nothing to act on.
	_, err := RunHook(context.Background(), HookOptions{
		Command:  "cat > /dev/null; echo 'template not found: PULL_REQUEST_TEMPLATE.md' >&2; exit 2",
		Contract: Sample(),
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "template not found") {
		t.Fatalf("error = %v, want the formatter's stderr", err)
	}
}

func TestRunHookSurfacesDiagnosticsOnSuccess(t *testing.T) {
	t.Parallel()
	requireShell(t)

	result, err := RunHook(context.Background(), HookOptions{
		Command:  `cat > /dev/null; echo 'linked ENG-4471' >&2; printf '%s\n' '{"version":1,"sections":[{"id":"summary","content":"body"}]}'`,
		Contract: Sample(),
	})
	if err != nil {
		t.Fatalf("RunHook: %v", err)
	}
	if !strings.Contains(result.Diagnostics, "linked ENG-4471") {
		t.Fatalf("diagnostics = %q", result.Diagnostics)
	}
}

func TestRunHookRejectsOversizedBody(t *testing.T) {
	t.Parallel()
	requireShell(t)

	_, err := RunHook(context.Background(), HookOptions{
		Command:  "cat > /dev/null; head -c 2000000 /dev/zero | tr '\\0' 'x'",
		Contract: Sample(),
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Fatalf("error = %v, want the size cap", err)
	}
}

// The cap has to be enforced as output is read, not after the whole stream has
// been buffered: this runs in the daemon, and a formatter that streams for the
// full timeout would otherwise grow its heap by gigabytes before being rejected.
// The marker file is the proof - the formatter is killed mid-stream, so the
// command after the stream never runs.
func TestRunHookStopsAStreamingFormatterAtTheCap(t *testing.T) {
	t.Parallel()
	requireShell(t)

	marker := filepath.Join(t.TempDir(), "drained")
	_, err := RunHook(context.Background(), HookOptions{
		Command:  "cat > /dev/null; head -c 8000000 /dev/zero | tr '\\0' 'x'; touch " + marker,
		Contract: Sample(),
		Timeout:  30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Fatalf("error = %v, want the size cap", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the formatter ran to completion; the cap was applied after buffering the whole stream")
	}
}

// An aborted run or a stopping daemon cancels the caller's context. Reporting
// that as a timeout blames the formatter for the operator's decision.
func TestRunHookReportsCancellationRatherThanTimeout(t *testing.T) {
	t.Parallel()
	requireShell(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err := RunHook(ctx, HookOptions{
		Command:  "cat > /dev/null; sleep 30",
		Contract: Sample(),
		Timeout:  30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want a cancellation rather than a timeout", err)
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("error = %v, want it to name the cancellation", err)
	}
}

func TestRunHookWithoutCommand(t *testing.T) {
	t.Parallel()
	_, err := RunHook(context.Background(), HookOptions{Command: "  ", Contract: Sample()})
	if !errors.Is(err, ErrNoHook) {
		t.Fatalf("error = %v, want ErrNoHook", err)
	}
}

func TestRunHookWithoutContract(t *testing.T) {
	t.Parallel()
	_, err := RunHook(context.Background(), HookOptions{Command: "cat"})
	if err == nil || !strings.Contains(err.Error(), "no contract") {
		t.Fatalf("error = %v, want a missing-contract error", err)
	}
}

func requireShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-command fixtures are POSIX")
	}
}
