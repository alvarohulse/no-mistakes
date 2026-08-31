//go:build windows

package daemon

import (
	"golang.org/x/sys/windows"
)

func syncEffectiveConfigDirectory(string) error {
	return nil
}

func publishEffectiveConfigDirectory(source, destination string) error {
	sourcePath, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPath, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(sourcePath, destinationPath, windows.MOVEFILE_WRITE_THROUGH)
}
