//go:build windows

package runner

import "os"

func processSignal(*os.ProcessState) *string { return nil }
