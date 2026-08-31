//go:build windows

package eval

import "golang.org/x/sys/windows"

func replacePrivateFile(source, target string) error {
	return windows.MoveFileEx(
		windows.StringToUTF16Ptr(source),
		windows.StringToUTF16Ptr(target),
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}
