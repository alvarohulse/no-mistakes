//go:build !windows

package artifact

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openRegularArtifactFile resolves every path component from a directory
// descriptor. O_NOFOLLOW on every open prevents a replacement symlink from
// redirecting the final read outside the supplied root.
func openRegularArtifactFile(root, relativePath string) (*os.File, fs.FileInfo, error) {
	if _, err := strictRelativePath(relativePath); err != nil {
		return nil, nil, err
	}

	directory, err := openNoFollowArtifactRoot(root)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = directory.Close() }()

	components := strings.Split(relativePath, "/")
	for index, component := range components {
		isFinal := index == len(components)-1
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if !isFinal {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Openat(int(directory.Fd()), component, flags, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("open artifact path component %q: %w", component, err)
		}
		next := os.NewFile(uintptr(fd), component)
		info, err := next.Stat()
		if err != nil {
			_ = next.Close()
			return nil, nil, fmt.Errorf("stat artifact path component %q: %w", component, err)
		}
		if isFinal {
			if !info.Mode().IsRegular() {
				_ = next.Close()
				return nil, nil, fmt.Errorf("artifact is not a regular file")
			}
			return next, info, nil
		}
		if !info.IsDir() {
			_ = next.Close()
			return nil, nil, fmt.Errorf("artifact path component %q is not a directory", component)
		}
		_ = directory.Close()
		directory = next
	}

	return nil, nil, fmt.Errorf("artifact path is empty")
}

func openNoFollowArtifactRoot(root string) (*os.File, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve supplied root: %w", err)
	}
	if !filepath.IsAbs(resolvedRoot) {
		return nil, fmt.Errorf("supplied root is not absolute")
	}

	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	current := os.NewFile(uintptr(fd), string(filepath.Separator))
	for _, component := range strings.Split(strings.TrimPrefix(filepath.Clean(resolvedRoot), string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		nextFD, err := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			_ = current.Close()
			return nil, fmt.Errorf("open supplied root component %q: %w", component, err)
		}
		next := os.NewFile(uintptr(nextFD), component)
		_ = current.Close()
		current = next
	}
	return current, nil
}
