package prbody

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const (
	// DefaultHookTimeout bounds a formatter. Formatting is a pure text
	// transform on data already in hand, so a formatter that has not finished
	// by now is stuck, not slow.
	DefaultHookTimeout = 60 * time.Second
	// MaxHookBodyBytes rejects a runaway formatter before its output reaches
	// a host. Well above any real PR body, far below anything that could wedge
	// the run.
	MaxHookBodyBytes = 1 << 20
	// maxHookDiagnosticRunes bounds the formatter stderr echoed into a
	// pipeline log line.
	maxHookDiagnosticRunes = 4 * 1024
)

// HookOptions configures one formatter invocation.
type HookOptions struct {
	// Command is the shell command to run. Empty means no hook is configured.
	Command string
	// Dir is the working directory, normally the run's worktree.
	Dir string
	// Contract is serialized to the command's stdin.
	Contract *Contract
	// Timeout defaults to DefaultHookTimeout.
	Timeout time.Duration
	// Grace is the process-group termination grace period.
	Grace time.Duration
	// Env is appended to the command's environment.
	Env []string
}

// HookResult is a successful formatter invocation.
type HookResult struct {
	Body string
	// Diagnostics is the formatter's stderr. A formatter is expected to report
	// what it linked, dropped, or left for the author here, so this is echoed
	// into the pipeline log even on success.
	Diagnostics string
	Duration    time.Duration
}

// ErrNoHook reports that no formatter is configured.
var ErrNoHook = errors.New("no pr_body hook configured")

// RunHook serializes the contract to the formatter's stdin and returns its
// stdout as the PR body.
//
// Every failure mode - missing command, non-zero exit, timeout, empty output,
// oversized output - returns an error. The caller's contract with the author
// is that a broken formatter never blocks shipping: fall back to the built-in
// body, and say loudly that you did. Silently shipping an untemplated body is
// the one outcome worse than either.
func RunHook(ctx context.Context, opts HookOptions) (*HookResult, error) {
	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return nil, ErrNoHook
	}
	if opts.Contract == nil {
		return nil, errors.New("pr_body hook: no contract to format")
	}

	payload, err := json.Marshal(opts.Contract)
	if err != nil {
		return nil, fmt.Errorf("pr_body hook: encode contract: %w", err)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultHookTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd.exe", "/c", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = opts.Dir
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if len(opts.Env) > 0 {
		cmd.Env = append(cmd.Environ(), opts.Env...)
	}
	shellenv.ConfigureShellCommand(cmd, opts.Grace)

	started := time.Now()
	runErr := shellenv.RunShellCommand(cmd)
	elapsed := time.Since(started)
	diagnostics := BoundedDiagnostics(stderr.Bytes())

	if runErr != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("pr_body hook timed out after %s%s", timeout, suffix(diagnostics))
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return nil, fmt.Errorf("pr_body hook exited %d%s", exitErr.ExitCode(), suffix(diagnostics))
		}
		return nil, fmt.Errorf("pr_body hook failed: %s%s", safeurl.RedactText(runErr.Error()), suffix(diagnostics))
	}

	if stdout.Len() > MaxHookBodyBytes {
		return nil, fmt.Errorf("pr_body hook returned %d bytes, over the %d byte cap%s",
			stdout.Len(), MaxHookBodyBytes, suffix(diagnostics))
	}
	body := strings.TrimSpace(stdout.String())
	if body == "" {
		return nil, fmt.Errorf("pr_body hook exited 0 but wrote no body%s", suffix(diagnostics))
	}

	return &HookResult{Body: body, Diagnostics: diagnostics, Duration: elapsed}, nil
}

// BoundedDiagnostics redacts and tail-truncates formatter stderr for logging.
// The tail is kept because a formatter's last lines are its conclusions.
func BoundedDiagnostics(output []byte) string {
	text := strings.TrimSpace(safeurl.RedactText(string(output)))
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > maxHookDiagnosticRunes {
		text = "…" + string(runes[len(runes)-maxHookDiagnosticRunes:])
	}
	return text
}

func suffix(diagnostics string) string {
	if diagnostics == "" {
		return ""
	}
	return ": " + diagnostics
}
