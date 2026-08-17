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

// Escaped-descendant helpers use a short, self-expiring marker so a crashed or
// killed test run cannot leave anything behind for long. The assertions all
// resolve in seconds; this is only the worst-case backstop.
const (
	escapedDescendantMarkerSeconds  = "90"
	escapedDescendantMarkerLifetime = 90 * time.Second
)

// TestShellCommandDescendants_ReapsSetsidEscapedDescendantsOnCleanExit is the
// regression test for the leak that survived process-group cleanup entirely.
//
// ConfigureShellCommand puts the leader in its own process group and
// TerminateShellCommandGroup signals that group on every exit path, but a
// descendant that calls setsid leaves the group and the session before it
// backgrounds anything. kill(-leaderPGID) then reaches nothing: the escapee has
// its own process-group id, and once the leader exits the escapee reparents
// away, so its ancestry is gone too. This is exactly what agent CLIs do - they
// spawn each tool-call shell detached - so work an agent backgrounds outlives
// the step that started it and keeps burning CPU for hours.
//
// Descendant discovery closes that gap by learning the escapee from kernel
// process events while the leader is still alive, so teardown can reach it
// afterwards.
func TestShellCommandDescendants_ReapsSetsidEscapedDescendantsOnCleanExit(t *testing.T) {
	dir := t.TempDir()
	cmd := shellCommandTerminationHelper(t, context.Background(), "leader-escaped", dir)
	ConfigureShellCommand(cmd, 5*time.Second)

	descendants := PrepareShellCommandDescendants(cmd, 5*time.Second)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatalf("StartShellCommand() error = %v", err)
	}
	descendants.Watch()
	release := func() { _ = os.WriteFile(filepath.Join(dir, "leader-release"), []byte("go"), 0o644) }
	t.Cleanup(release)

	escapedPID := readPID(t, filepath.Join(dir, "escaped.pid"), 10*time.Second)
	markerPID := readPID(t, filepath.Join(dir, "escaped-marker.pid"), 10*time.Second)
	// Never leave the markers behind, however this test exits.
	t.Cleanup(func() {
		_ = syscall.Kill(escapedPID, syscall.SIGKILL)
		_ = syscall.Kill(markerPID, syscall.SIGKILL)
	})

	// Precondition: the escapee really did leave the leader's process group, so
	// the group kill below cannot be what reaps it.
	escapedPGID, err := syscall.Getpgid(escapedPID)
	if err != nil {
		t.Fatalf("Getpgid(%d): %v", escapedPID, err)
	}
	if escapedPGID == cmd.Process.Pid {
		t.Fatalf("precondition failed: descendant %d stayed in the leader's process group %d", escapedPID, escapedPGID)
	}

	// Where discovery runs while the leader is alive, let the leader exit only
	// once the escapee has been seen, so the test pins the reaping behaviour
	// rather than racing event delivery. Platforms that enumerate at teardown
	// instead (Linux, via the child-subreaper adoption path) have nothing
	// recorded until the leader is gone, so releasing it IS the trigger.
	if descendantDiscoveryTracksLiveLeader && !waitForRecordedDescendant(descendants, escapedPID, 30*time.Second) {
		t.Fatalf("discovery never recorded escaped descendant %d", escapedPID)
	}
	release()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("leader Wait() error = %v", err)
	}

	TerminateShellCommandGroup(cmd)
	descendants.Terminate()

	if !pidGoneWithin(escapedPID, 10*time.Second) {
		t.Errorf("escaped session leader %d survived teardown; descendants that setsid out of the group leak", escapedPID)
	}
	if !pidGoneWithin(markerPID, 10*time.Second) {
		t.Errorf("marker %d backgrounded by the escaped descendant survived teardown", markerPID)
	}
}

// TestShellCommandDescendants_SentinelDetectsAnUnsweptSurvivor pins the
// backstop. Discovery narrows the window but does not close it, so the sentinel
// exists to answer whether anything survived the sweep. Here nothing is ever
// discovered (no Watch, so no kill), and the sentinel must still see that a
// descendant is holding the inherited descriptor.
func TestShellCommandDescendants_SentinelDetectsAnUnsweptSurvivor(t *testing.T) {
	dir := t.TempDir()
	cmd := shellCommandTerminationHelper(t, context.Background(), "leader-escaped", dir)
	ConfigureShellCommand(cmd, time.Second)

	descendants := PrepareShellCommandDescendants(cmd, time.Second)
	if descendants == nil || descendants.sentinel == nil {
		t.Fatal("PrepareShellCommandDescendants did not install a sentinel")
	}
	if err := StartShellCommand(cmd); err != nil {
		t.Fatalf("StartShellCommand() error = %v", err)
	}
	// Deliberately no Watch: this models a descendant discovery never saw.
	descendants.sentinel.releaseParentEnd()

	escapedPID := readPID(t, filepath.Join(dir, "escaped.pid"), 10*time.Second)
	markerPID := readPID(t, filepath.Join(dir, "escaped-marker.pid"), 10*time.Second)
	t.Cleanup(func() {
		_ = syscall.Kill(escapedPID, syscall.SIGKILL)
		_ = syscall.Kill(markerPID, syscall.SIGKILL)
	})
	_ = os.WriteFile(filepath.Join(dir, "leader-release"), []byte("go"), 0o644)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("leader Wait() error = %v", err)
	}
	TerminateShellCommandGroup(cmd)

	if !descendants.sentinel.holdersRemain() {
		t.Fatal("sentinel reported a clean sweep while an escaped descendant was still running")
	}

	// And once the survivors are gone the sentinel stops crying wolf.
	_ = syscall.Kill(escapedPID, syscall.SIGKILL)
	_ = syscall.Kill(markerPID, syscall.SIGKILL)
	if !pidGoneWithin(escapedPID, 10*time.Second) || !pidGoneWithin(markerPID, 10*time.Second) {
		t.Fatal("could not clear the survivors before asserting the sentinel clears")
	}
	if descendants.sentinel.holdersRemain() {
		t.Error("sentinel still reports holders after every descendant exited")
	}
	descendants.sentinel.close()
}

func TestTerminateShellCommandGroup_AllowsCooperativeDescendantCleanup(t *testing.T) {
	dir := t.TempDir()
	cmd := shellCommandTerminationHelper(t, context.Background(), "leader-clean", dir)
	ConfigureShellCommand(cmd, 5*time.Second)
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
	ConfigureShellCommand(cmd, 5*time.Second)

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

	started := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("RunShellCommand() did not return after cancellation")
	}
	if elapsed := time.Since(started); elapsed >= 4*time.Second {
		t.Fatalf("cooperative cancellation took %s; want prompt exit before the 5s ceiling", elapsed)
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
	ConfigureShellCommand(cmd, 250*time.Millisecond)

	started := time.Now()
	if err := RunShellCommand(cmd); err != nil {
		t.Fatalf("RunShellCommand() error = %v", err)
	}
	elapsed := time.Since(started)

	descendantPID := readPID(t, filepath.Join(dir, "descendant.pid"), 5*time.Second)
	t.Cleanup(func() {
		_ = syscall.Kill(descendantPID, syscall.SIGKILL)
	})
	if elapsed < 150*time.Millisecond {
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
	case "leader-escaped":
		// Mirrors an agent CLI launching a tool-call shell: the child is spawned
		// detached, so it leaves the leader's process group AND session before
		// backgrounding work of its own.
		child := exec.Command(os.Args[0], "-test.run=^TestShellCommandTerminationHelper$")
		child.Env = append(os.Environ(),
			"NM_SHELLENV_TERMINATION_HELPER=descendant-escaped",
			"NM_SHELLENV_TERMINATION_DIR="+dir,
		)
		// Detached stdio: the escapee must not hold the leader's pipes open, so
		// this exercises the process-tree leak and not the WaitDelay path.
		if err := child.Start(); err != nil {
			os.Exit(20)
		}
		if !waitForHelperReady(filepath.Join(dir, "escaped-ready"), 10*time.Second) {
			os.Exit(21)
		}
		// Keep running until released, the way a real agent keeps working after a
		// tool call backgrounds something. Exiting instantly instead would make the
		// test a race against the watcher's sampling interval.
		if !waitForHelperReady(filepath.Join(dir, "leader-release"), 60*time.Second) {
			os.Exit(25)
		}
		os.Exit(0)
	case "descendant-escaped":
		if _, err := syscall.Setsid(); err != nil {
			os.Exit(22)
		}
		// A cheap, bounded marker stands in for the runaway work the real leak
		// left behind. It self-expires so a crashed test cannot leak it for long.
		marker := exec.Command("sleep", escapedDescendantMarkerSeconds)
		if err := marker.Start(); err != nil {
			os.Exit(23)
		}
		_ = os.WriteFile(filepath.Join(dir, "escaped.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "escaped-marker.pid"), []byte(strconv.Itoa(marker.Process.Pid)), 0o644)
		if err := os.WriteFile(filepath.Join(dir, "escaped-ready"), []byte("ready"), 0o644); err != nil {
			os.Exit(24)
		}
		time.Sleep(escapedDescendantMarkerLifetime)
		os.Exit(0)
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
	ConfigureShellCommand(cmd, DefaultProcessTerminationGrace)
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

// TestTerminateShellCommandGroup_AsksBeforeKilling pins that a surviving
// group member is given the chance to shut down: it receives SIGTERM and its
// handler runs to completion. SIGKILL would deny a test runner or worker
// script the chance to flush output and clean up its own temporary state.
//
// A /bin/sh grandchild with `trap` and `sleep` is not this contract: macOS
// /bin/sh (bash 3.2) delivers process-group SIGTERM to both the shell and its
// sleep child, then exits without running the trap. The ready-file handshake
// only proved trap was registered, which is why this test still failed in
// 0.01s on CI run 31827318230 after that fix. The grandchild is a Go helper
// that uses signal.Notify, so SIGTERM is observed by this process, not a
// sleep(1) that dies with the default action.
func TestTerminateShellCommandGroup_AsksBeforeKilling(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	termFile := filepath.Join(dir, "grandchild.term")
	readyFile := filepath.Join(dir, "grandchild.ready")

	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestTerminateShellCommandGroupTermHelper$")
	cmd.Env = append(os.Environ(),
		"NM_SHELLENV_TERM_HELPER=leader",
		"NM_SHELLENV_TERM_PID="+pidFile,
		"NM_SHELLENV_TERM_READY="+readyFile,
		"NM_SHELLENV_TERM_FILE="+termFile,
	)
	ConfigureShellCommand(cmd, DefaultProcessTerminationGrace)
	if err := cmd.Run(); err != nil {
		t.Fatalf("leader Run: %v", err)
	}
	grandchild := readPID(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })

	TerminateShellCommandGroup(cmd)

	if !pidGoneWithin(grandchild, 5*time.Second) {
		t.Fatalf("grandchild %d still alive after TerminateShellCommandGroup", grandchild)
	}
	if _, err := os.Stat(termFile); err != nil {
		t.Fatalf("grandchild never ran its SIGTERM handler: %v", err)
	}
}

func TestTerminateShellCommandGroupTermHelper(t *testing.T) {
	switch os.Getenv("NM_SHELLENV_TERM_HELPER") {
	case "leader":
		child := exec.Command(os.Args[0], "-test.run=^TestTerminateShellCommandGroupTermHelper$")
		child.Env = append(os.Environ(),
			"NM_SHELLENV_TERM_HELPER=grandchild",
			"NM_SHELLENV_TERM_PID="+os.Getenv("NM_SHELLENV_TERM_PID"),
			"NM_SHELLENV_TERM_READY="+os.Getenv("NM_SHELLENV_TERM_READY"),
			"NM_SHELLENV_TERM_FILE="+os.Getenv("NM_SHELLENV_TERM_FILE"),
		)
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if !waitForHelperReady(os.Getenv("NM_SHELLENV_TERM_READY"), 5*time.Second) {
			_ = child.Process.Kill()
			os.Exit(3)
		}
		os.Exit(0)
	case "grandchild":
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		if err := os.WriteFile(os.Getenv("NM_SHELLENV_TERM_PID"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(4)
		}
		if err := os.WriteFile(os.Getenv("NM_SHELLENV_TERM_READY"), []byte("ready"), 0o644); err != nil {
			os.Exit(5)
		}
		<-term
		if err := os.WriteFile(os.Getenv("NM_SHELLENV_TERM_FILE"), []byte("terminated"), 0o644); err != nil {
			os.Exit(6)
		}
		os.Exit(0)
	default:
		t.Skip("helper invoked by TestTerminateShellCommandGroup_AsksBeforeKilling")
	}
}

// TestTerminateShellCommandGroup_EscalatesWhenSIGTERMIsIgnored is the other
// half of the contract: politeness must not become a new way to leak. A group
// member that ignores SIGTERM is SIGKILLed once the grace period is up.
func TestTerminateShellCommandGroup_EscalatesWhenSIGTERMIsIgnored(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	script := "( trap '' TERM; while :; do sleep 0.1; done ) >/dev/null 2>&1 & echo $! > " + pidFile + "; exit 0"
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", script)
	ConfigureShellCommand(cmd, DefaultProcessTerminationGrace)
	if err := cmd.Run(); err != nil {
		t.Fatalf("leader Run: %v", err)
	}
	grandchild := readPID(t, pidFile, 5*time.Second)

	TerminateShellCommandGroup(cmd)

	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild %d ignored SIGTERM and was never escalated to SIGKILL", grandchild)
	}
}

// TestConfigureShellCommand_CancelEscalatesWithoutBlockingWait pins the
// cancellation path: cmd.Cancel runs on the goroutine that owns cmd.Wait, so
// it must return promptly rather than sit through the grace period, and the
// SIGKILL escalation still has to land on a group member that ignores
// SIGTERM.
func TestConfigureShellCommand_CancelEscalatesWithoutBlockingWait(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	script := "( trap '' TERM; while :; do sleep 0.1; done ) >/dev/null 2>&1 & echo $! > " + pidFile + "; " +
		"trap '' TERM; while :; do sleep 0.1; done"
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	ConfigureShellCommand(cmd, DefaultProcessTerminationGrace)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatalf("StartShellCommand: %v", err)
	}
	grandchild := readPID(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })

	cancel()
	_ = cmd.Wait()

	if !pidGoneWithin(grandchild, 10*time.Second) {
		t.Fatalf("grandchild %d survived cancellation", grandchild)
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
	ConfigureShellCommand(cmd, DefaultProcessTerminationGrace)
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
	ConfigureShellCommand(cmd, DefaultProcessTerminationGrace)
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

// TestDefaultShellCommandOutput_WaitDelayBoundsEscapedPipeHolder pins the
// tighter WaitDelay the login-shell probe installs for itself: when the probed
// shell leaves behind a descendant that still holds the inherited stdout pipe
// open, the probe must return on that backstop instead of blocking for as long
// as the holder runs. Daemon startup waits on this probe, so a wedged read
// stalls every agent launch behind it (#143).
//
// The holder escapes the process group with setsid, which is what makes this
// deterministic. A shell-backgrounded child stays in the leader's group, so
// whether it is still holding the pipe after cancellation depends on whether
// the group SIGTERM raced the shell's fork: win that race and the child dies
// with the group and never exercises the backstop at all; lose it and the child
// misses a signal that is only sent once, keeps the group non-empty, and
// cmd.Cancel deliberately waits out the termination grace before escalating to
// SIGKILL. The predecessor of this test drove `sleep 1 & wait` through a 20ms
// deadline and asserted a 500ms ceiling, so it was usually vacuous and failed
// on macOS CI whenever the shell was slow enough to lose that race.
func TestDefaultShellCommandOutput_WaitDelayBoundsEscapedPipeHolder(t *testing.T) {
	readyFile := filepath.Join(t.TempDir(), "ready")
	// defaultShellCommandOutput builds the command itself, so the helper mode
	// travels through the inherited environment rather than cmd.Env.
	t.Setenv("NM_SHELLENV_PIPE_HELPER", "leader")
	t.Setenv("NM_SHELLENV_PIPE_READY", readyFile)

	started := time.Now()
	out, err := defaultShellCommandOutput(os.Args[0], "-test.run=^TestShellOutputPipeHelper$")
	elapsed := time.Since(started)

	escapedPID := parseEscapedPID(t, string(out))
	t.Cleanup(func() {
		_ = syscall.Kill(escapedPID, syscall.SIGKILL)
	})
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("defaultShellCommandOutput() error = %v, want %v; output %q", err, exec.ErrWaitDelay, out)
	}
	// The holder sleeps far longer than this ceiling, so returning under it can
	// only mean the probe stopped waiting on the pipe rather than outliving the
	// process holding it.
	if elapsed >= 15*time.Second {
		t.Fatalf("probe returned after %s; want the WaitDelay backstop to bound it", elapsed)
	}
	if syscall.Kill(escapedPID, 0) != nil {
		t.Fatalf("escaped pipe holder %d was gone on return; the probe was not bounded by the backstop", escapedPID)
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

func waitForRecordedDescendant(d *ShellCommandDescendants, pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		tracked, ok := d.tracked[pid]
		d.mu.Unlock()
		if ok && tracked.escaped {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
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

// pidGoneWithin reports whether pid stops existing within the window.
//
// It collects first because kill(pid, 0) succeeds for a zombie. Where this
// process is a child subreaper, a descendant a test kills by hand becomes its
// zombie rather than disappearing, and only a wait makes it actually gone.
func pidGoneWithin(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for {
		collectAdoptedDescendant(pid)
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
