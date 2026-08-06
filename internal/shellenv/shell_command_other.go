//go:build !unix && !windows

package shellenv

import (
	"os/exec"
	"time"
)

// ConfigureShellCommand is a no-op on platforms that lack process groups
// (and a process-tree kill primitive). Context cancellation falls back to the
// exec.CommandContext default of terminating the direct child only.
func ConfigureShellCommand(cmd *exec.Cmd, _ time.Duration) {}

// StartShellCommand starts cmd on platforms without extra process-tree setup.
// It exists so call sites can use the same lifecycle helpers on every platform.
func StartShellCommand(cmd *exec.Cmd) error {
	return cmd.Start()
}

// TerminateShellCommandGroup is a no-op on platforms without a process-tree kill
// primitive, mirroring ConfigureShellCommand. The reap-the-group-on-exit
// guarantee is best-effort and platform-gated.
func TerminateShellCommandGroup(cmd *exec.Cmd) {}

// ShellCommandDescendants is the inert stand-in on platforms with no
// process-group model to escape from in the first place.
type ShellCommandDescendants struct{}

// PrepareShellCommandDescendants is a no-op on platforms without process groups.
func PrepareShellCommandDescendants(cmd *exec.Cmd, _ time.Duration) *ShellCommandDescendants {
	return nil
}

// Watch is a no-op, including on the nil receiver call sites use.
func (d *ShellCommandDescendants) Watch() {}

// Terminate is a no-op, including on the nil receiver call sites use.
func (d *ShellCommandDescendants) Terminate() {}
