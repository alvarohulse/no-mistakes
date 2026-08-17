package prbody

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

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
	// the run. It is enforced as output is read, not after: this runs inside
	// the daemon, so a formatter that streams for the whole timeout must not be
	// able to grow the daemon's heap first and be rejected afterwards.
	MaxHookBodyBytes = 1 << 20
	// maxHookDiagnosticBytes bounds what is READ from a formatter's stderr, for
	// the same reason as MaxHookBodyBytes. It is far above any real formatter's
	// stderr, so in practice BoundedDiagnostics still sees the true tail.
	maxHookDiagnosticBytes = 64 << 10
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
	Patches PatchSet
	// Diagnostics is the formatter's stderr. A formatter is expected to report
	// what it linked, dropped, or left for the author here, so this is echoed
	// into the pipeline log even on success.
	Diagnostics string
	Duration    time.Duration
}

// ErrNoHook reports that no formatter is configured.
var ErrNoHook = errors.New("no pr_body hook configured")

// RunHook serializes the contract to the formatter's stdin and decodes its
// stdout as owned-section patches. A formatter can never return a replacement
// full body; unknown output fields are rejected.
//
// Every failure mode - missing command, non-zero exit, timeout, empty output,
// oversized output - returns an error. Callers report the failure and may use
// built-in section content, but publication still fails closed when an existing
// body lacks the matching verified marker set.
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
	stdout := &cappedBuffer{limit: MaxHookBodyBytes}
	stderr := &headBuffer{limit: maxHookDiagnosticBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if len(opts.Env) > 0 {
		cmd.Env = append(cmd.Environ(), opts.Env...)
	}
	shellenv.ConfigureShellCommand(cmd, opts.Grace)

	started := time.Now()
	// RunShellCommand joins its output-copy goroutines before returning, so the
	// buffers below are this goroutine's again by the time they are read.
	runErr := shellenv.RunShellCommand(cmd)
	elapsed := time.Since(started)
	diagnostics := BoundedDiagnostics(stderr.Bytes())

	// The cap fires while the formatter is still running and kills it, so it is
	// checked before runErr: the resulting "killed" wait error describes the
	// symptom, not the reason.
	if stdout.overflowed() {
		return nil, fmt.Errorf("pr_body hook returned more than the %d byte cap%s",
			MaxHookBodyBytes, suffix(diagnostics))
	}

	if runErr != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return nil, fmt.Errorf("pr_body hook timed out after %s%s", timeout, suffix(diagnostics))
		case errors.Is(ctx.Err(), context.Canceled):
			// An aborted run or a stopping daemon cancels the caller's context.
			// Reporting that as a timeout blames the formatter for the
			// operator's decision.
			return nil, fmt.Errorf("pr_body hook cancelled after %s%s",
				elapsed.Round(time.Millisecond), suffix(diagnostics))
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return nil, fmt.Errorf("pr_body hook exited %d%s", exitErr.ExitCode(), suffix(diagnostics))
		}
		return nil, fmt.Errorf("pr_body hook failed: %s%s", safeurl.RedactText(runErr.Error()), suffix(diagnostics))
	}

	output := stdout.Bytes()
	if len(bytes.TrimSpace(output)) == 0 {
		return nil, fmt.Errorf("pr_body hook exited 0 but wrote no patches%s", suffix(diagnostics))
	}
	if !utf8.Valid(output) {
		return nil, fmt.Errorf("pr_body hook output: %w%s", ErrInvalidUTF8, suffix(diagnostics))
	}
	var patches PatchSet
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patches); err != nil {
		return nil, fmt.Errorf("pr_body hook decode patches: %w%s", err, suffix(diagnostics))
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("pr_body hook decode patches: %w%s", err, suffix(diagnostics))
	}
	if err := ValidatePatchSet(patches); err != nil {
		return nil, fmt.Errorf("pr_body hook validate patches: %w%s", err, suffix(diagnostics))
	}

	return &HookResult{Patches: patches, Diagnostics: diagnostics, Duration: elapsed}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

// errHookOutputTooLarge stops the output copy the moment a formatter goes over
// the cap. The copier terminates the command group on a write error, so the
// formatter is killed instead of being allowed to keep streaming.
var errHookOutputTooLarge = errors.New("pr_body hook output over the size cap")

// cappedBuffer accumulates up to limit bytes and then fails the write. The body
// has to be exact, so an over-cap body is rejected rather than truncated.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.over {
		return 0, errHookOutputTooLarge
	}
	if room := c.limit - c.buf.Len(); len(p) > room {
		if room > 0 {
			_, _ = c.buf.Write(p[:room])
		}
		c.over = true
		return 0, errHookOutputTooLarge
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string { return c.buf.String() }

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

// overflowed reports whether the formatter wrote past the cap.
func (c *cappedBuffer) overflowed() bool { return c.over }

// headBuffer keeps the first limit bytes and silently discards the rest.
// Diagnostics are advisory, so a runaway stderr is dropped rather than being
// allowed to fail the formatter that otherwise produced a usable body.
type headBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := h.limit - h.buf.Len(); room > 0 {
		kept := p
		if len(kept) > room {
			kept = kept[:room]
		}
		if _, err := h.buf.Write(kept); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (h *headBuffer) Bytes() []byte { return h.buf.Bytes() }

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
