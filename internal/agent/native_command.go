package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

type nativeAgentCommand struct {
	cmd                      *exec.Cmd
	descendants              *shellenv.ShellCommandDescendants
	stdout                   *nativeAgentPipe
	stderr                   *nativeAgentPipe
	waitCh                   chan error
	terminateOnce            sync.Once
	terminateDescendantsOnce sync.Once
	closePipesOnce           sync.Once
	pipeMu                   sync.Mutex
	remainingPipes           int
	pipesDone                chan struct{}
}

type nativeAgentPipe struct {
	file     *os.File
	done     func()
	doneOnce sync.Once
}

func (p *nativeAgentPipe) Read(b []byte) (int, error) {
	n, err := p.file.Read(b)
	if err != nil {
		p.markDone()
	}
	return n, err
}

func (p *nativeAgentPipe) Close() error {
	err := p.file.Close()
	p.markDone()
	return err
}

func (p *nativeAgentPipe) markDone() {
	p.doneOnce.Do(p.done)
}

// startNativeAgentCommand starts an agent CLI that ConfigureShellCommand has
// already prepared, and owns its whole process-tree lifecycle.
//
// processTerminationGrace must be the same value passed to
// ConfigureShellCommand: it bounds cleanup of the descendants that escaped the
// agent's process group as well as the group itself. Agent CLIs spawn each
// tool-call shell detached (setsid), so anything the agent backgrounds sits
// outside the group the leader's pgid can reach - see
// shellenv.PrepareShellCommandDescendants.
func startNativeAgentCommand(cmd *exec.Cmd, processTerminationGrace time.Duration) (*nativeAgentCommand, error) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	// Prepared before the start so the agent inherits the sentinel descriptor,
	// then watched immediately after it so discovery begins with the first fork.
	descendants := shellenv.PrepareShellCommandDescendants(cmd, processTerminationGrace)
	if err := shellenv.StartShellCommand(cmd); err != nil {
		descendants.Terminate()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return nil, err
	}
	descendants.Watch()
	_ = stdoutW.Close()
	_ = stderrW.Close()

	started := &nativeAgentCommand{
		cmd:            cmd,
		descendants:    descendants,
		waitCh:         make(chan error, 1),
		remainingPipes: 2,
		pipesDone:      make(chan struct{}),
	}
	started.stdout = &nativeAgentPipe{file: stdoutR, done: started.markPipeDone}
	started.stderr = &nativeAgentPipe{file: stderrR, done: started.markPipeDone}
	go func() {
		err := cmd.Wait()
		started.terminate()
		waitErr := started.waitForPipes(err)
		// The escaped-descendant sweep runs after the pipes are settled, never
		// before. It waits for the processes it signalled to actually die, and
		// putting that wait ahead of waitForPipes would delay the parser by the
		// time it takes an escapee to exit. Ordering it here keeps the parser as
		// prompt as it was while still finishing the sweep before wait returns, so
		// callers still get "everything reaped by the time Run returns".
		started.terminateDescendants()
		started.waitCh <- waitErr
	}()
	return started, nil
}

func (c *nativeAgentCommand) markPipeDone() {
	c.pipeMu.Lock()
	defer c.pipeMu.Unlock()
	c.remainingPipes--
	if c.remainingPipes == 0 {
		close(c.pipesDone)
	}
}

func (c *nativeAgentCommand) pid() int {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *nativeAgentCommand) waitForPipes(waitErr error) error {
	if c.cmd.WaitDelay <= 0 {
		<-c.pipesDone
		return waitErr
	}
	timer := time.NewTimer(c.cmd.WaitDelay)
	defer timer.Stop()
	select {
	case <-c.pipesDone:
		return waitErr
	case <-timer.C:
		c.closePipes()
		if waitErr == nil {
			return exec.ErrWaitDelay
		}
		return waitErr
	}
}

func (c *nativeAgentCommand) terminate() {
	c.terminateOnce.Do(func() {
		shellenv.TerminateShellCommandGroup(c.cmd)
	})
}

// terminateDescendants reaps what the group kill structurally cannot reach:
// work the agent backgrounded from a tool-call shell that had setsid its way out
// of the group. Without it that work survives the step and burns CPU
// indefinitely. It is separate from terminate so it can be ordered after pipe
// handling; see the wait goroutine in startNativeAgentCommand.
func (c *nativeAgentCommand) terminateDescendants() {
	c.terminateDescendantsOnce.Do(func() {
		c.descendants.Terminate()
	})
}

func (c *nativeAgentCommand) waitAfterParseError(parseErr error) error {
	c.terminate()
	c.closePipes()
	waitErr := c.wait()
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		return waitErr
	}
	return parseErr
}

func (c *nativeAgentCommand) wait() error {
	return <-c.waitCh
}

func (c *nativeAgentCommand) closePipes() {
	c.closePipesOnce.Do(func() {
		_ = c.stdout.Close()
		_ = c.stderr.Close()
	})
}
