//go:build !windows

package runner

import (
	"os"
	"syscall"
)

func processSignal(state *os.ProcessState) *string {
	if state == nil {
		return nil
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return nil
	}
	signal := status.Signal().String()
	return &signal
}

// ProcessSignal reports the terminating signal represented by a process
// state. It is shared by runner-owned and controller-owned direct commands so
// both execution paths persist the same signal identity.
func ProcessSignal(state *os.ProcessState) *string {
	return processSignal(state)
}
