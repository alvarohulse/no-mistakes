//go:build windows

package runner

import "os"

func processSignal(*os.ProcessState) *string { return nil }

// ProcessSignal reports the terminating signal represented by a process
// state. Windows does not expose Unix-style process signals.
func ProcessSignal(state *os.ProcessState) *string {
	return processSignal(state)
}
