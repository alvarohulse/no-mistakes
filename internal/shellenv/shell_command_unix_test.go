//go:build unix

package shellenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTerminateShellCommandGroup_AllowsCooperativeDescendantCleanup(t *testing.T) {
	dir := t.TempDir()
	cmd := shellCommandTerminationHelper(t, context.Background(), "leader-clean", dir)
	ConfigureShellCommand(cmd)
	if err := RunShellCommand(cmd); err != nil {
		t.Fatalf("RunShellCommand() error = %v", err)
	}

	descendantPID := readPID(t, filepath.Join(dir, "descendant.pid"), 5*time.Second)
	t.Cleanup(func() {
		_ = syscall.Kill(descendantPID, syscall.SIGKILL)
	})
	if _, err := os.Stat(filepath.Join(dir, "cleanup-complete")); err != nil {
		t.Fatal("cooperative descendant did not finish cleanup after leader exit")
	}
	if !pidGoneWithin(descendantPID, 5*time.Second) {
		t.Fatalf("cooperative descendant %d survived process-group cleanup", descendantPID)
	}
}

func TestConfigureShellCommand_CancelAllowsCooperativeDescendantCleanup(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := shellCommandTerminationHelper(t, ctx, "leader-wait", dir)
	ConfigureShellCommand(cmd)

	done := make(chan error, 1)
	go func() {
		done <- RunShellCommand(cmd)
	}()
	if !waitForHelperReady(filepath.Join(dir, "descendant-ready"), 5*time.Second) {
		cancel()
		t.Fatal("timed out waiting for cooperative descendant")
	}
	descendantPID := readPID(t, filepath.Join(dir, "descendant.pid"), 5*time.Second)
	t.Cleanup(func() {
		_ = syscall.Kill(descendantPID, syscall.SIGKILL)
	})

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunShellCommand() did not return after cancellation")
	}
	if _, err := os.Stat(filepath.Join(dir, "cleanup-complete")); err != nil {
		t.Fatal("cooperative descendant did not finish cleanup after cancellation")
	}
	if !pidGoneWithin(descendantPID, 5*time.Second) {
		t.Fatalf("cooperative descendant %d survived cancellation", descendantPID)
	}
}

func TestTerminateShellCommandGroup_ForceKillsTermIgnoringDescendantAfterGrace(t *testing.T) {
	dir := t.TempDir()
	cmd := shellCommandTerminationHelper(t, context.Background(), "leader-clean-ignore", dir)
	ConfigureShellCommand(cmd)

	started := time.Now()
	if err := RunShellCommand(cmd); err != nil {
		t.Fatalf("RunShellCommand() error = %v", err)
	}
	elapsed := time.Since(started)

	descendantPID := readPID(t, filepath.Join(dir, "descendant.pid"), 5*time.Second)
	t.Cleanup(func() {
		_ = syscall.Kill(descendantPID, syscall.SIGKILL)
	})
	if elapsed < 200*time.Millisecond {
		t.Fatalf("process-group cleanup returned after %s; want a grace period before SIGKILL", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("process-group cleanup took %s; want bounded escalation", elapsed)
	}
	if !pidGoneWithin(descendantPID, 5*time.Second) {
		t.Fatalf("SIGTERM-ignoring descendant %d survived bounded cleanup", descendantPID)
	}
}

func TestShellCommandTerminationHelper(t *testing.T) {
	mode := os.Getenv("NM_SHELLENV_TERMINATION_HELPER")
	if mode == "" {
		return
	}
	dir := os.Getenv("NM_SHELLENV_TERMINATION_DIR")

	switch mode {
	case "leader-clean", "leader-wait", "leader-clean-ignore":
		descendantMode := "descendant-cooperative"
		if mode == "leader-clean-ignore" {
			descendantMode = "descendant-ignore"
		}
		child := exec.Command(os.Args[0], "-test.run=^TestShellCommandTerminationHelper$")
		child.Env = append(os.Environ(),
			"NM_SHELLENV_TERMINATION_HELPER="+descendantMode,
			"NM_SHELLENV_TERMINATION_DIR="+dir,
		)
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if !waitForHelperReady(filepath.Join(dir, "descendant-ready"), 5*time.Second) {
			os.Exit(3)
		}
		if mode == "leader-wait" {
			for {
				time.Sleep(time.Hour)
			}
		}
		os.Exit(0)
	case "descendant-cooperative":
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		if err := os.WriteFile(filepath.Join(dir, "descendant.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(4)
		}
		if err := os.WriteFile(filepath.Join(dir, "descendant-ready"), []byte("ready"), 0o644); err != nil {
			os.Exit(5)
		}
		<-term
		if err := os.WriteFile(filepath.Join(dir, "cleanup-complete"), []byte("done"), 0o644); err != nil {
			os.Exit(6)
		}
		os.Exit(0)
	case "descendant-ignore":
		signal.Ignore(syscall.SIGTERM)
		if err := os.WriteFile(filepath.Join(dir, "descendant.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(8)
		}
		if err := os.WriteFile(filepath.Join(dir, "descendant-ready"), []byte("ready"), 0o644); err != nil {
			os.Exit(9)
		}
		for {
			time.Sleep(time.Hour)
		}
	default:
		os.Exit(7)
	}
}

func shellCommandTerminationHelper(t *testing.T, ctx context.Context, mode, dir string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShellCommandTerminationHelper$")
	cmd.Env = append(os.Environ(),
		"NM_SHELLENV_TERMINATION_HELPER="+mode,
		"NM_SHELLENV_TERMINATION_DIR="+dir,
	)
	return cmd
}

// TestTerminateShellCommandGroup_ReapsGrandchildAfterCleanExit pins the
// success-path guarantee that keeps the daemon alive: when a leader configured
// with ConfigureShellCommand exits 0 but leaves a grandchild alive in its
// process group (a test runner's worker pool), TerminateShellCommandGroup
// gracefully terminates the whole group, force-killing only after a bounded
// grace period. cmd.Cancel only fires on cancellation, so without this the
// grandchild leaks and orphan pools pile up across runs until the host OOMs and
// the OS kills the daemon.
func TestTerminateShellCommandGroup_ReapsGrandchildAfterCleanExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	// The leader backgrounds a long-lived grandchild (stdio detached so it does
	// not hold the inherited pipes open), records its pid, and exits 0.
	script := "( sleep 120 >/dev/null 2>&1 ) & echo $! > " + pidFile + "; exit 0"
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", script)
	ConfigureShellCommand(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("leader Run: %v", err)
	}

	grandchild := readPID(t, pidFile, 5*time.Second)
	if syscall.Kill(grandchild, 0) != nil {
		t.Fatalf("precondition failed: grandchild %d should still be alive before reap", grandchild)
	}

	TerminateShellCommandGroup(cmd)

	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild %d still alive after TerminateShellCommandGroup; group leaked", grandchild)
	}
}

// TestTerminateShellCommandGroup_NoopOnNilOrUnstarted guards the cheap safety
// contract: a nil command, or one that was never started (no Process), must be
// a no-op rather than panic or signal an arbitrary pid.
func TestTerminateShellCommandGroup_NoopOnNilOrUnstarted(t *testing.T) {
	TerminateShellCommandGroup(nil)
	cmd := exec.Command("/bin/sh", "-c", "true") // never Start()ed: cmd.Process is nil
	TerminateShellCommandGroup(cmd)
}

func TestCombinedOutputShellCommand_ReturnsCleanExitWithInheritedPipeGrandchild(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "printf 'leader done\\n'; sleep 30 & exit 0")
	ConfigureShellCommand(cmd)
	cmd.WaitDelay = 100 * time.Millisecond

	out, err := CombinedOutputShellCommand(cmd)
	if err != nil {
		t.Fatalf("CombinedOutputShellCommand() error = %v; output %q", err, out)
	}
	if got, want := string(out), "leader done\n"; got != want {
		t.Fatalf("CombinedOutputShellCommand() output = %q, want %q", got, want)
	}
}

func TestCombinedOutputShellCommand_WaitDelayBoundsEscapedPipeHolder(t *testing.T) {
	readyFile := filepath.Join(t.TempDir(), "ready")
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestShellOutputPipeHelper$")
	cmd.Env = append(os.Environ(),
		"NM_SHELLENV_PIPE_HELPER=leader",
		"NM_SHELLENV_PIPE_READY="+readyFile,
	)
	ConfigureShellCommand(cmd)
	cmd.WaitDelay = 100 * time.Millisecond

	out, err := CombinedOutputShellCommand(cmd)
	escapedPID := parseEscapedPID(t, string(out))
	t.Cleanup(func() {
		_ = syscall.Kill(escapedPID, syscall.SIGKILL)
	})
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("CombinedOutputShellCommand() error = %v, want %v; output %q", err, exec.ErrWaitDelay, out)
	}
	if !strings.Contains(string(out), "leader done\n") {
		t.Fatalf("CombinedOutputShellCommand() output = %q, want leader output", out)
	}
}

func TestShellOutputPipeHelper(t *testing.T) {
	switch os.Getenv("NM_SHELLENV_PIPE_HELPER") {
	case "leader":
		child := exec.Command(os.Args[0], "-test.run=^TestShellOutputPipeHelper$")
		child.Env = append(os.Environ(),
			"NM_SHELLENV_PIPE_HELPER=escaped",
			"NM_SHELLENV_PIPE_READY="+os.Getenv("NM_SHELLENV_PIPE_READY"),
		)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if !waitForHelperReady(os.Getenv("NM_SHELLENV_PIPE_READY"), 5*time.Second) {
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString("leader done\nescaped pid " + strconv.Itoa(child.Process.Pid) + "\n")
		os.Exit(0)
	case "escaped":
		_, _ = syscall.Setsid()
		_ = os.WriteFile(os.Getenv("NM_SHELLENV_PIPE_READY"), []byte("ready"), 0o644)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

func readPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if v, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && v > 0 {
				return v
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a pid in %s", path)
	return 0
}

func parseEscapedPID(t *testing.T, output string) int {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "escaped pid ") {
			pid, err := strconv.Atoi(strings.TrimPrefix(line, "escaped pid "))
			if err == nil && pid > 0 {
				return pid
			}
		}
	}
	t.Fatalf("output %q did not contain escaped pid", output)
	return 0
}

func waitForHelperReady(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func pidGoneWithin(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) == syscall.ESRCH
}
