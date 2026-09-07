//go:build !windows

package artifact

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type commandOutputFile struct {
	file      *os.File
	directory *os.File
	name      string
}

func createCommandOutputFile(root, runID, name string, afterCommandDirectoryOpen func()) (*commandOutputFile, error) {
	return createArtifactFile(root, runID, commandOutputDirectory, name, afterCommandDirectoryOpen)
}

func createArtifactFile(root, runID, directoryName, name string, afterDirectoryOpen func()) (*commandOutputFile, error) {
	rootDirectory, err := openOrCreateNoFollowArtifactRoot(root)
	if err != nil {
		return nil, err
	}
	defer rootDirectory.Close()

	runDirectory, err := openOrCreatePrivateArtifactDirectory(rootDirectory, runID)
	if err != nil {
		return nil, fmt.Errorf("open run directory: %w", err)
	}
	defer runDirectory.Close()

	commandDirectory, err := openOrCreatePrivateArtifactDirectory(runDirectory, directoryName)
	if err != nil {
		return nil, fmt.Errorf("open artifact directory: %w", err)
	}
	if afterDirectoryOpen != nil {
		afterDirectoryOpen()
	}
	fd, err := unix.Openat(
		int(commandDirectory.Fd()),
		name,
		unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_CREAT|unix.O_EXCL,
		0o600,
	)
	if err != nil {
		_ = commandDirectory.Close()
		return nil, fmt.Errorf("create output file: %w", err)
	}
	return &commandOutputFile{
		file:      os.NewFile(uintptr(fd), name),
		directory: commandDirectory,
		name:      name,
	}, nil
}

func (f *commandOutputFile) protect() error {
	return f.file.Chmod(0o600)
}

func (f *commandOutputFile) closeAndSync() error {
	if f.file != nil {
		if err := f.file.Close(); err != nil {
			return err
		}
		f.file = nil
	}
	if f.directory == nil {
		return nil
	}
	if err := f.directory.Sync(); err != nil {
		return err
	}
	err := f.directory.Close()
	f.directory = nil
	return err
}

func (f *commandOutputFile) discard() error {
	var errs []error
	if f.file != nil {
		errs = append(errs, f.file.Close())
		f.file = nil
	}
	if f.directory != nil {
		if err := unix.Unlinkat(int(f.directory.Fd()), f.name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			errs = append(errs, err)
		}
		errs = append(errs, f.directory.Close())
		f.directory = nil
	}
	return errors.Join(errs...)
}

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
	return openArtifactRoot(root, false)
}

func openOrCreateNoFollowArtifactRoot(root string) (*os.File, error) {
	return openArtifactRoot(root, true)
}

func openArtifactRoot(root string, create bool) (*os.File, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("supplied root is not absolute")
	}
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	current := os.NewFile(uintptr(fd), string(filepath.Separator))
	components := strings.Split(strings.TrimPrefix(filepath.Clean(root), string(filepath.Separator)), string(filepath.Separator))
	for index, component := range components {
		if component == "" {
			continue
		}
		var next *os.File
		if create {
			next, err = openOrCreateArtifactDirectory(current, component, index == len(components)-1)
		} else {
			next, err = openNoFollowArtifactDirectory(current, component)
		}
		if err != nil {
			_ = current.Close()
			return nil, fmt.Errorf("open supplied root component %q: %w", component, err)
		}
		_ = current.Close()
		current = next
	}
	return current, nil
}

func openOrCreatePrivateArtifactDirectory(parent *os.File, name string) (*os.File, error) {
	return openOrCreateArtifactDirectory(parent, name, true)
}

func openOrCreateArtifactDirectory(parent *os.File, name string, private bool) (*os.File, error) {
	directory, err := openNoFollowArtifactDirectory(parent, name)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return nil, err
	}
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, err
		}
		directory, err = openNoFollowArtifactDirectory(parent, name)
		if err != nil {
			return nil, err
		}
	}
	if private {
		if err := directory.Chmod(0o700); err != nil {
			_ = directory.Close()
			return nil, err
		}
	}
	return directory, nil
}

func openNoFollowArtifactDirectory(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(
		int(parent.Fd()),
		name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
