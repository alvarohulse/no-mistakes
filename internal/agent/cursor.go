package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// cursorAgent spawns Cursor CLI print mode for each invocation.
type cursorAgent struct {
	bin                     string
	extraArgs               []string
	model                   string
	processTerminationGrace time.Duration
}

func (a *cursorAgent) Name() string { return "cursor" }

func (a *cursorAgent) SupportsSessionResume() bool { return true }

func (a *cursorAgent) ReportsAgentAttempts() bool { return true }

func (a *cursorAgent) NeutralizesGateInstructions() bool { return true }

func (a *cursorAgent) buildArgs(workspace, repo, resumeID string) []string {
	extraArgs := withoutFlagValues(a.extraArgs, "--workspace", "--add-dir")
	if a.model != "" {
		extraArgs = withoutFlagValues(extraArgs, "--model")
	}
	args := make([]string, 0, len(extraArgs)+14)
	args = append(args, extraArgs...)
	if a.model != "" {
		args = append(args, "--model", a.model)
	}
	args = append(args,
		"-p",
		"--output-format", "stream-json",
		"--workspace", workspace,
		"--add-dir", repo,
	)
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if !cursorUserSetPermissionMode(a.extraArgs) {
		args = append(args, "--force")
	}
	args = append(args, "--trust")
	return args
}

func cursorUserSetPermissionMode(args []string) bool {
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "-f", "--force", "--yolo", "--auto-review":
			return true
		}
	}
	return false
}

func cursorContainedPrompt(prompt, repo string) string {
	return fmt.Sprintf(`## Repository target

Your primary working directory is an empty containment workspace. The repository for this task is the additional workspace root at the absolute path %q.

Perform every repository read, write, search, build, test, and version-control operation against that absolute path. Shell and git commands must set their working directory explicitly (for example, git -C %q status). Do not treat the empty primary workspace as the repository.

## Task

%s`, repo, repo, prompt)
}

func (a *cursorAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "cursor", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *cursorAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	repo, err := filepath.Abs(opts.CWD)
	if err != nil {
		return nil, fmt.Errorf("cursor repository path: %w", err)
	}
	workspace, err := os.MkdirTemp("", "no-mistakes-cursor-workspace-*")
	if err != nil {
		return nil, fmt.Errorf("cursor containment workspace: %w", err)
	}
	defer os.RemoveAll(workspace)

	prompt := cursorContainedPrompt(opts.Prompt, repo)
	if len(opts.JSONSchema) > 0 {
		prompt = buildACPStructuredPrompt(prompt, opts.JSONSchema)
	}
	resumeID := ""
	if opts.Session != nil {
		resumeID = opts.Session.ID
	}
	cmd := exec.CommandContext(ctx, a.bin, a.buildArgs(workspace, repo, resumeID)...)
	cmd.Dir = workspace
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = gitSafeEnv(workspace)
	shellenv.ConfigureShellCommand(cmd, a.processTerminationGrace)

	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("cursor start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "cursor", pid)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	metrics := newCursorMetricsAccumulator()
	parsed, parseErr := parseCursorEvents(ctx, started.stdout, opts.OnChunk, metrics.onEvent)
	if parseErr != nil {
		parseErr = started.waitAfterParseError(parseErr)
		stderrWG.Wait()
		// A cancellation can surface here or on the wait path below, depending
		// only on whether an unread event was still buffered when it landed: the
		// reader aborts mid-stream, otherwise it reaches EOF and the wait path
		// classifies it. Both are the same outcome, so classify it here too -
		// wrapping it reports a stream-format failure for an ordinary abort.
		if ctxErr := ctx.Err(); ctxErr != nil {
			emitAgentExited(opts, "cursor", pid, ctxErr)
			return nil, ctxErr
		}
		retErr := fmt.Errorf("cursor parse events: %w", parseErr)
		emitAgentExited(opts, "cursor", pid, retErr)
		return nil, retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		emitAgentExited(opts, "cursor", pid, ctxErr)
		return nil, ctxErr
	}
	if waitErr != nil && (parsed == nil || !parsed.Terminal) && cursorCancellationExit(waitErr) {
		emitAgentExited(opts, "cursor", pid, context.Canceled)
		return nil, context.Canceled
	}
	if waitErr != nil {
		retErr := fmt.Errorf("cursor exited: %w: %s", waitErr, cursorProcessErrorOutput(stderrBuf, parsed))
		emitAgentExited(opts, "cursor", pid, retErr)
		return nil, retErr
	}
	if parsed == nil || !parsed.Terminal {
		retErr := fmt.Errorf("cursor returned no result event")
		emitAgentExited(opts, "cursor", pid, retErr)
		return nil, retErr
	}
	if parsed.IsError || parsed.Subtype != "success" {
		detail := cursorProcessErrorOutput(nil, parsed)
		var retErr error
		if detail != "" {
			retErr = fmt.Errorf("cursor error: subtype=%s: %s", parsed.Subtype, detail)
		} else {
			retErr = fmt.Errorf("cursor error: subtype=%s", parsed.Subtype)
		}
		emitAgentExited(opts, "cursor", pid, retErr)
		return nil, retErr
	}

	res, err := finalizeTextResult("cursor", parsed.Text, opts.JSONSchema, parsed.Usage)
	if res != nil {
		res.SessionID = parsed.SessionID
		res.Resumed = resumeID != ""
		res.Model = a.model
		if res.Model == "" {
			res.Model = sanitizeModelToken(parsed.Model)
		}
		res.CacheCreationReported = parsed.Usage.CacheCreationReported
		m := metrics.metrics()
		res.Metrics = &m
	}
	emitAgentExited(opts, "cursor", pid, err)
	return res, err
}

func (a *cursorAgent) Close() error { return nil }

func cursorProcessErrorOutput(stderr []byte, parsed *cursorParsedResult) string {
	parts := make([]string, 0, 3)
	if text := strings.TrimSpace(string(stderr)); text != "" {
		parts = append(parts, text)
	}
	if parsed != nil {
		if text := outputSnippet(parsed.Text); text != "" {
			parts = append(parts, text)
		}
		if text := strings.TrimSpace(parsed.PlainText); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}
