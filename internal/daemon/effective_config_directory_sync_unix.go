//go:build !windows

package daemon

import "os"

func publishEffectiveConfigDirectory(source, destination string) error {
	return os.Rename(source, destination)
}

func syncEffectiveConfigDirectory(path string) error {
	dir, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}
