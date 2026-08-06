//go:build unix

package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const nativeAgentEscapedPipeHelperEnv = "NM_AGENT_NATIVE_PIPE_HELPER"

func TestNativeAgentCommand_WaitDelayClosesEscapedPipeHolder(t *testing.T) {
	dir := t.TempDir()
	readyFile := filepath.Join(dir, "ready")
	pidFile := filepath.Join(dir, "escaped.pid")
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestNativeAgentEscapedPipeHelper$")
	cmd.Env = append(os.Environ(),
		nativeAgentEscapedPipeHelperEnv+"=leader",
		"NM_AGENT_NATIVE_PIPE_READY="+readyFile,
		"NM_AGENT_NATIVE_PIPE_PID="+pidFile,
	)
	shellenv.ConfigureShellCommand(cmd, shellenv.DefaultProcessTerminationGrace)
	cmd.WaitDelay = 100 * time.Millisecond

	started, err := startNativeAgentCommand(cmd, shellenv.DefaultProcessTerminationGrace)
	if err != nil {
		t.Fatalf("startNativeAgentCommand: %v", err)
	}
	defer started.closePipes()

	type readResult struct {
		output string
		err    error
	}
	readCh := make(chan readResult, 1)
	go func() {
		out, err := io.ReadAll(started.stdout)
		readCh <- readResult{output: string(out), err: err}
	}()

	var rr readResult
	select {
	case rr = <-readCh:
	case <-time.After(2 * time.Second):
		started.closePipes()
		started.terminate()
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		t.Fatal("stdout reader stayed blocked after the leader exited with an escaped pipe holder")
	}

	escapedPID := waitForPidFile(t, pidFile, 5*time.Second)
	t.Cleanup(func() {
		_ = syscall.Kill(escapedPID, syscall.SIGKILL)
	})
	if !strings.Contains(rr.output, "leader done\n") {
		t.Fatalf("stdout output = %q, want leader output", rr.output)
	}
	if rr.err == nil {
		t.Fatalf("stdout read error = nil, want closed-pipe error")
	}
	if err := started.wait(); !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("wait error = %v, want %v", err, exec.ErrWaitDelay)
	}
}

func TestNativeAgentEscapedPipeHelper(t *testing.T) {
	switch os.Getenv(nativeAgentEscapedPipeHelperEnv) {
	case "leader":
		child := exec.Command(os.Args[0], "-test.run=^TestNativeAgentEscapedPipeHelper$")
		child.Env = append(os.Environ(), nativeAgentEscapedPipeHelperEnv+"=escaped",
			"NM_AGENT_NATIVE_PIPE_READY="+os.Getenv("NM_AGENT_NATIVE_PIPE_READY"))
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(os.Getenv("NM_AGENT_NATIVE_PIPE_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		if !waitForNativeAgentPipeHelperReady(os.Getenv("NM_AGENT_NATIVE_PIPE_READY"), 5*time.Second) {
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString("leader done\nescaped pid " + strconv.Itoa(child.Process.Pid) + "\n")
		os.Exit(0)
	case "escaped":
		_, _ = syscall.Setsid()
		_ = os.WriteFile(os.Getenv("NM_AGENT_NATIVE_PIPE_READY"), []byte("ready"), 0o644)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

// TestCodexAgent_Run_ReapsLeakedGrandchildOnCleanExit is the regression test for
// the daemon-crash bug behind the agent-spawning test step.
//
// When a repo has no configured test command, the test step asks the agent to
// run the tests itself. That agent (codex here) spawns a test runner whose
// worker pool can outlive it. ConfigureShellCommand isolates the agent in its
// own process group and installs whole-group cancellation - but cmd.Cancel only
// fires on context cancellation. On a clean exit (exit 0) nothing reaped the
// group, so the worker grandchildren leaked. Across runs those orphans
// accumulate (each a multi-hundred-MB worker pool) until the host is out of
// memory and the OS OOM-killer SIGKILLs the daemon, which the next daemon start
// reports as "daemon crashed during execution".
//
// The fake codex backgrounds a grandchild whose stdio is detached (so it does
// not hold the agent's stdout pipe open, which would wedge the parser instead
// of exercising the clean-exit leak path), records its pid, prints a valid
// result, and exits 0. After the fix the deferred TerminateShellCommandGroup
// reaps the group on this success path, so the grandchild is gone once Run
// returns. Before the fix it survived.
func TestCodexAgent_Run_ReapsLeakedGrandchildOnCleanExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	bin := writeFakeCodex(t, dir, `#!/bin/sh
# Background a long-lived grandchild that outlives this leader, mirroring a test
# runner's worker pool. Detach its stdio so it does not keep the agent's
# stdout/stderr pipe open.
( sleep 120 >/dev/null 2>&1 ) &
echo $! > "`+pidFile+`"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
exit 0
`, "")

	ca := &codexAgent{bin: bin}
	result, err := ca.Run(context.Background(), RunOpts{Prompt: "run the tests", CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("Run returned error (the daemon would fail the step, not crash): %v", err)
	}
	if result.Text != `{"ok":true}` {
		t.Fatalf("unexpected agent text: %q", result.Text)
	}

	grandchild := waitForPidFile(t, pidFile, 5*time.Second)
	// Once Run has returned, the deferred TerminateShellCommandGroup must have
	// reaped the whole group. Poll briefly to absorb signal-delivery jitter.
	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL) // do not orphan a real process
		t.Fatalf("grandchild pid %d still alive after clean agent exit; the process group leaked "+
			"(this is the leak that OOM-kills the daemon)", grandchild)
	}
}

func TestCursorAgent_Run_ReapsLeakedGrandchildOnCleanExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "cursor-grandchild.pid")
	bin := filepath.Join(dir, "cursor-agent")
	script := `#!/bin/sh
( sleep 120 >/dev/null 2>&1 ) &
echo $! > "` + pidFile + `"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"cursor-session"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"cursor-session"}'
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := (&cursorAgent{bin: bin}).Run(context.Background(), RunOpts{Prompt: "run the tests", CWD: dir})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != "done" {
		t.Fatalf("result text = %q, want done", result.Text)
	}
	grandchild := waitForPidFile(t, pidFile, 5*time.Second)
	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("Cursor grandchild pid %d survived clean leader exit", grandchild)
	}
}

func TestClaudeAgent_LargeStdinReapsGrandchildHoldingPipesOnLeaderExit(t *testing.T) {
	dir := t.TempDir()
	readyFile := filepath.Join(dir, "ready")
	pidFile := filepath.Join(dir, "grandchild.pid")
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "spawn-grandchild")
	t.Setenv("NM_CLAUDE_STDIN_READY", readyFile)
	t.Setenv("NM_CLAUDE_STDIN_PID", pidFile)

	a := newClaudeStdinHelperAgent(t)
	result, err := a.runOnce(context.Background(), RunOpts{
		Prompt: strings.Repeat("p", 2*1024*1024),
		CWD:    dir,
	})
	if err != nil {
		t.Fatalf("Claude run with inherited-pipe holder: %v", err)
	}
	if result.Text != "ok" {
		t.Fatalf("Claude result text = %q, want ok", result.Text)
	}

	grandchild := waitForPidFile(t, pidFile, 5*time.Second)
	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("Claude grandchild pid %d survived clean leader exit", grandchild)
	}
}

func TestCodexAgent_Run_ReapsGrandchildHoldingStdoutPipeOnLeaderExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	bin := writeFakeCodex(t, dir, `#!/bin/sh
	( sleep 120 ) &
	echo $! > "`+pidFile+`"
	printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
	exit 0
	`, "")

	ca := &codexAgent{bin: bin}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type runResult struct {
		result *Result
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		result, err := ca.Run(ctx, RunOpts{Prompt: "run the tests", CWD: t.TempDir()})
		done <- runResult{result: result, err: err}
	}()

	var rr runResult
	select {
	case rr = <-done:
	case <-time.After(1500 * time.Millisecond):
		cancel()
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		t.Fatal("agent run did not return after its leader exited while a grandchild held stdout open")
	}

	if rr.err != nil {
		t.Fatalf("Run returned error: %v", rr.err)
	}
	if rr.result.Text != `{"ok":true}` {
		t.Fatalf("unexpected agent text: %q", rr.result.Text)
	}

	grandchild := waitForPidFile(t, pidFile, 5*time.Second)
	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d still alive after leader exit with inherited stdout", grandchild)
	}
}

// escapedToolShellHold keeps the fake agent alive briefly after its tool shell
// escapes, which is what a real agent does - it keeps working after a tool call
// backgrounds something.
//
// Discovery is driven by kernel fork events, so the descendant is normally known
// within a fraction of a millisecond; this is slack for event delivery, not a
// sampling interval being waited out. Some slack is still needed because an
// agent that vanished in the very same instant as its fork would be testing the
// residual discovery window rather than the reaping behaviour.
const escapedToolShellHold = 500 * time.Millisecond

// TestClaudeAgent_Run_ReapsToolShellThatEscapedTheProcessGroup is the
// end-to-end regression test for the leak observed in a real gate run: a Test
// step whose agent backgrounded work left 88 processes running for hours at
// 574% CPU, long after the step had finished and the run had moved on.
//
// The mechanism, reproduced here: agent CLIs spawn each tool-call shell
// detached, so the shell calls setsid and leads its own session and process
// group. ConfigureShellCommand's kill(-agentPGID) then reaches nothing the agent
// backgrounded, and once the agent exits the escapee reparents away, erasing the
// ancestry that could have identified it afterwards. The group cleanup was
// working exactly as designed and still could not see these processes.
//
// This exercises the real spawn path end to end - PrepareShellCommandDescendants
// before the start, kernel-driven discovery after it, and the sweep in
// terminate() - rather than the shellenv primitives in isolation.
//
// The fake agent mirrors that shape: an escaped session leader plus a cheap,
// self-expiring marker standing in for the runaway work. Both must be gone once
// Run returns.
func TestClaudeAgent_Run_ReapsToolShellThatEscapedTheProcessGroup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NM_AGENT_ESCAPED_HELPER", "tool-shell")
	t.Setenv("NM_AGENT_ESCAPED_DIR", dir)

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("current test executable: %v", err)
	}
	a := &claudeAgent{
		bin:                     exe,
		extraArgs:               []string{"-test.run=^TestAgentEscapedSessionHelper$", "--"},
		disableProjectSettings:  true,
		processTerminationGrace: 5 * time.Second,
	}

	result, err := a.runOnce(context.Background(), RunOpts{Prompt: "run the tests", CWD: dir})
	if err != nil {
		t.Fatalf("runOnce() error = %v", err)
	}
	if result.Text != "ok" {
		t.Fatalf("result text = %q, want ok", result.Text)
	}

	escapedPID := waitForPidFile(t, filepath.Join(dir, "escaped.pid"), 5*time.Second)
	markerPID := waitForPidFile(t, filepath.Join(dir, "marker.pid"), 5*time.Second)
	t.Cleanup(func() {
		_ = syscall.Kill(escapedPID, syscall.SIGKILL)
		_ = syscall.Kill(markerPID, syscall.SIGKILL)
	})

	if !pidGoneWithin(escapedPID, 10*time.Second) {
		t.Errorf("escaped tool shell %d survived the agent run; this is the gate leak", escapedPID)
	}
	if !pidGoneWithin(markerPID, 10*time.Second) {
		t.Errorf("work %d backgrounded by the escaped tool shell survived the agent run", markerPID)
	}
}

// TestAgentEscapedSessionHelper is the fake agent CLI and its escaping tool
// shell. It is inert unless NM_AGENT_ESCAPED_HELPER selects a role.
func TestAgentEscapedSessionHelper(t *testing.T) {
	dir := os.Getenv("NM_AGENT_ESCAPED_DIR")
	switch os.Getenv("NM_AGENT_ESCAPED_HELPER") {
	case "tool-shell":
		child := exec.Command(os.Args[0], "-test.run=^TestAgentEscapedSessionHelper$")
		child.Env = append(os.Environ(),
			"NM_AGENT_ESCAPED_HELPER=escaped-session",
			"NM_AGENT_ESCAPED_DIR="+dir,
		)
		if err := child.Start(); err != nil {
			os.Exit(30)
		}
		if !waitForNativeAgentPipeHelperReady(filepath.Join(dir, "escaped-ready"), 10*time.Second) {
			os.Exit(31)
		}
		time.Sleep(escapedToolShellHold)
		_, _ = io.WriteString(os.Stdout, `{"type":"assistant","session_id":"escape-session","message":{"model":"helper-model","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":"ok"}]}}`+"\n")
		_, _ = io.WriteString(os.Stdout, `{"type":"result","subtype":"success","session_id":"escape-session"}`+"\n")
		os.Exit(0)
	case "escaped-session":
		if _, err := syscall.Setsid(); err != nil {
			os.Exit(32)
		}
		// Self-expiring marker: cheap, unambiguous, and incapable of outliving a
		// crashed test run for long.
		marker := exec.Command("sleep", "90")
		if err := marker.Start(); err != nil {
			os.Exit(33)
		}
		_ = os.WriteFile(filepath.Join(dir, "escaped.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "marker.pid"), []byte(strconv.Itoa(marker.Process.Pid)), 0o644)
		if err := os.WriteFile(filepath.Join(dir, "escaped-ready"), []byte("ready"), 0o644); err != nil {
			os.Exit(34)
		}
		time.Sleep(90 * time.Second)
		os.Exit(0)
	}
}

func waitForPidFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			if v, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && v > 0 {
				return v
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a pid in %s", path)
	return 0
}

// pidGoneWithin reports whether pid stops existing within the window. kill(pid,
// 0) returns ESRCH once the process is gone, but it also succeeds for a zombie,
// and where this process is a child subreaper an orphaned grandchild reparents
// onto it rather than init - so a collection attempt has to come first for
// "gone" to mean gone. The wait targets an exact pid that is never one os/exec
// owns (these are always grandchildren), so it cannot steal a Cmd.Wait status.
func pidGoneWithin(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for {
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitForNativeAgentPipeHelperReady(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
