//go:build !windows

package daemon

import "os"

func protectEffectiveConfigDirectory(path string) error {
	return os.Chmod(path, 0o700)
}

func protectEffectiveConfigFile(path string) error {
	return os.Chmod(path, 0o600)
}
