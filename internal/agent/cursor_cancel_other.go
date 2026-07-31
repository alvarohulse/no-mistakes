//go:build !unix && !windows

package agent

func cursorCancellationExit(error) bool { return false }
