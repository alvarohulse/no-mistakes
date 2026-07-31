//go:build windows

package agent

import (
	"errors"
	"os/exec"
)

func cursorCancellationExit(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && (exitErr.ExitCode() == 130 || exitErr.ExitCode() == 143)
}
