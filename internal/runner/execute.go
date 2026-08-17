package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

var (
	ErrTimeout       = errors.New("runner timed out")
	ErrInvalidSyntax = errors.New("invalid runner command syntax")
)

// ExecuteOptions configures one local runner process. A zero timeout inherits
// only the caller's context deadline.
type ExecuteOptions struct {
	Dir                     string
	ExtraEnv                []string
	Timeout                 time.Duration
	ProcessTerminationGrace time.Duration
	// CaptureFullOutput preserves configured-pipeline behavior where the complete
	// combined output is retained for the authoritative step log.
	// Leave it false at bounded diagnostic surfaces such as preflight.
	CaptureFullOutput bool
	// OutputLimit bounds combined stdout/stderr retained in Result. Values at
	// or below zero use the safe default; values above the hard cap are clamped.
	OutputLimit int
}

// Result distinguishes an ordinary non-zero command exit from a launch or
// lifecycle error.
type Result struct {
	Output    string
	ExitCode  int
	Truncated bool
}

const maxCapturedOutputBytes = 64 * 1024

// Prepared is a resolved command whose syntax was checked by its exact shell.
// Its fields stay private so callers cannot accidentally execute an unvalidated
// Resolved value.
type Prepared struct {
	resolved  Resolved
	validated bool
}

// Prepare resolves and syntax-checks one command. No command body is executed.
func Prepare(ctx context.Context, command Command, defaultRunner Spec, options ExecuteOptions) (Prepared, error) {
	resolved, err := Resolve(ctx, command, defaultRunner)
	prepared := Prepared{resolved: resolved}
	if err != nil {
		return prepared, err
	}
	if err := resolved.ValidateSyntax(ctx, options); err != nil {
		return prepared, err
	}
	prepared.validated = true
	return prepared, nil
}

// Resolution returns an independent copy of the prepared command and its
// provenance for persistence or display.
func (p Prepared) Resolution() Resolved {
	resolved := p.resolved
	resolved.Argv = append([]string(nil), p.resolved.Argv...)
	resolved.Provenance.Args = append([]string(nil), p.resolved.Provenance.Args...)
	if p.resolved.Provenance.Version != nil {
		version := *p.resolved.Provenance.Version
		resolved.Provenance.Version = &version
	}
	return resolved
}

// Execute runs a prepared command in a managed process tree.
func (p Prepared) Execute(ctx context.Context, options ExecuteOptions) (Result, error) {
	if !p.validated {
		return Result{}, fmt.Errorf("execute runner: command was not prepared")
	}
	return executeArgv(ctx, p.resolved.Argv, options, nil, captureOutputLimit(options))
}

// ValidateSyntax parses the command with the resolved shell without running
// the command body.
func (r Resolved) ValidateSyntax(ctx context.Context, options ExecuteOptions) error {
	argv, err := r.syntaxArgv()
	if err != nil {
		return err
	}
	var stdin io.Reader
	if resolvedKind(r) == shellPowerShell {
		stdin = strings.NewReader(r.Script)
	}
	result, err := executeArgv(ctx, argv, options, stdin, 64*1024)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("%w: runner exited with code %d", ErrInvalidSyntax, result.ExitCode)
	}
	return nil
}

func executeArgv(ctx context.Context, argv []string, options ExecuteOptions, stdin io.Reader, outputLimit int) (Result, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return Result{}, fmt.Errorf("launch runner: argv is empty")
	}
	runCtx := ctx
	cancel := func() {}
	if options.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, options.Timeout)
	}
	defer cancel()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = options.Dir
	cmd.Stdin = stdin
	if len(options.ExtraEnv) > 0 {
		cmd.Env = mergeEnv(os.Environ(), options.ExtraEnv, runtime.GOOS)
	}
	shellenv.ConfigureShellCommand(cmd, options.ProcessTerminationGrace)
	output := newBoundedBuffer(outputLimit)
	cmd.Stdout = output
	cmd.Stderr = output
	err := shellenv.RunShellCommand(cmd)
	if ctx.Err() != nil {
		return capturedResult(output, -1), ctx.Err()
	}
	if runCtx.Err() != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return capturedResult(output, -1), fmt.Errorf("%w after %s", ErrTimeout, options.Timeout)
		}
		return capturedResult(output, -1), runCtx.Err()
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return capturedResult(output, exitErr.ExitCode()), nil
		}
		return capturedResult(output, -1), fmt.Errorf("launch runner: %w", err)
	}
	return capturedResult(output, 0), nil
}

func outputLimit(requested int) int {
	if requested <= 0 || requested > maxCapturedOutputBytes {
		return maxCapturedOutputBytes
	}
	return requested
}

func captureOutputLimit(options ExecuteOptions) int {
	if options.CaptureFullOutput {
		return int(^uint(0) >> 1)
	}
	return outputLimit(options.OutputLimit)
}

func capturedResult(output *boundedBuffer, exitCode int) Result {
	return Result{Output: output.String(), ExitCode: exitCode, Truncated: output.Truncated()}
}

func (r Resolved) syntaxArgv() ([]string, error) {
	if len(r.Argv) < 2 || strings.TrimSpace(r.Script) == "" {
		return nil, fmt.Errorf("validate runner syntax: resolved argv is incomplete")
	}
	kind, err := validateSpec(Spec{Executable: r.Provenance.Executable, Args: r.Provenance.Args})
	if err != nil {
		return nil, fmt.Errorf("validate runner syntax: %w", err)
	}
	if kind == shellPowerShell {
		parser := `$source = [Console]::In.ReadToEnd(); $tokens = $null; $parseErrors = $null; ` +
			`[System.Management.Automation.Language.Parser]::ParseInput($source, [ref]$tokens, [ref]$parseErrors) | Out-Null; ` +
			`if ($parseErrors.Count -gt 0) { exit 1 }`
		argv := make([]string, 0, len(r.Provenance.Args)+2)
		argv = append(argv, r.resolvedExecutable())
		argv = append(argv, r.Provenance.Args...)
		argv = append(argv, parser)
		return argv, nil
	}
	argv := make([]string, 0, len(r.Provenance.Args)+3)
	argv = append(argv, r.resolvedExecutable(), "-n")
	argv = append(argv, r.Provenance.Args...)
	argv = append(argv, r.Script)
	return argv, nil
}

func (r Resolved) resolvedExecutable() string {
	if r.executable != "" {
		return r.executable
	}
	if len(r.Argv) > 0 {
		return r.Argv[0]
	}
	return ""
}

func resolvedKind(resolved Resolved) shellKind {
	kind, _ := validateSpec(Spec{Executable: resolved.Provenance.Executable, Args: resolved.Provenance.Args})
	return kind
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{remaining: limit}
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	if b.remaining <= 0 {
		b.truncated = b.truncated || len(data) > 0
		return written, nil
	}
	if len(data) > b.remaining {
		data = data[:b.remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(data)
	b.remaining -= len(data)
	return written, nil
}

func (b *boundedBuffer) String() string { return b.buffer.String() }

func (b *boundedBuffer) Truncated() bool { return b.truncated }

func mergeEnv(base, extra []string, platform string) []string {
	merged := append([]string(nil), base...)
	positions := make(map[string]int, len(merged))
	for i, entry := range merged {
		positions[envKey(entry, platform)] = i
	}
	for _, entry := range extra {
		key := envKey(entry, platform)
		if position, ok := positions[key]; ok {
			merged[position] = entry
			continue
		}
		positions[key] = len(merged)
		merged = append(merged, entry)
	}
	return merged
}

func envKey(entry, platform string) string {
	key, _, _ := strings.Cut(entry, "=")
	if platform == "windows" {
		return strings.ToUpper(key)
	}
	return key
}
