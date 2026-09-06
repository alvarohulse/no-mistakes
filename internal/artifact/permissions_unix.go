//go:build !windows

package artifact

import "os"

func protectArtifactDirectory(path string) error {
	return os.Chmod(path, 0o700)
}

func protectArtifactFile(path string) error {
	return os.Chmod(path, 0o600)
}
