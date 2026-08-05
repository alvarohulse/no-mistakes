package prbody

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunHookReturnsFormatterStdout(t *testing.T) {
	t.Parallel()
	requireShell(t)

	result, err := RunHook(context.Background(), HookOptions{
		Command:  "cat > /dev/null; printf '## Custom\\n\\nbody\\n'",
		Contract: Sample(),
	})
	if err != nil {
		t.Fatalf("RunHook: %v", err)
	}
	if result.Body != "## Custom\n\nbody" {
		t.Fatalf("body = %q", result.Body)
	}
}

func TestRunHookReceivesContractOnStdin(t *testing.T) {
	t.Parallel()
	requireShell(t)

	// The formatter's only input is stdin; if the contract is not piped, the
	// grep finds nothing and the command exits non-zero.
	result, err := RunHook(context.Background(), HookOptions{
		Command:  `grep -o '"version":2' && echo ok`,
		Contract: Sample(),
	})
	if err != nil {
		t.Fatalf("RunHook: %v", err)
	}
	if !strings.Contains(result.Body, "ok") {
		t.Fatalf("body = %q, want the formatter to have seen the contract", result.Body)
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
		{"empty output", "cat > /dev/null; exit 0", 0, "wrote no body"},
		{"whitespace-only output", "cat > /dev/null; printf '   \\n\\n'", 0, "wrote no body"},
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
		Command:  "cat > /dev/null; echo 'linked ENG-4471' >&2; echo body",
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
