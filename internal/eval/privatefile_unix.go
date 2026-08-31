//go:build !windows

package eval

import "os"

func replacePrivateFile(source, target string) error {
	return os.Rename(source, target)
}
