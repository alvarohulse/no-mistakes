//go:build unix

package agent

import (
	"errors"
	"os/exec"
	"syscall"
)

func cursorCancellationExit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() {
		return status.Signal() == syscall.SIGINT || status.Signal() == syscall.SIGTERM
	}
	return exitErr.ExitCode() == 130 || exitErr.ExitCode() == 143
}
