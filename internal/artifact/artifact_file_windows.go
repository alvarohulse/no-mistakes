//go:build windows

package artifact

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type commandOutputFile struct {
	file      *os.File
	handle    windows.Handle
	directory windows.Handle
}

func createCommandOutputFile(root, runID, name string, afterCommandDirectoryOpen func()) (*commandOutputFile, error) {
	rootDirectory, err := openOrCreateNoFollowArtifactRoot(root)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(rootDirectory)

	runDirectory, err := openOrCreatePrivateArtifactDirectory(rootDirectory, runID)
	if err != nil {
		return nil, fmt.Errorf("open run directory: %w", err)
	}
	defer windows.CloseHandle(runDirectory)

	commandDirectory, err := openOrCreatePrivateArtifactDirectory(runDirectory, commandOutputDirectory)
	if err != nil {
		return nil, fmt.Errorf("open command output directory: %w", err)
	}
	if afterCommandDirectoryOpen != nil {
		afterCommandDirectoryOpen()
	}
	handle, err := createNoFollowArtifactFile(commandDirectory, name)
	if err != nil {
		windows.CloseHandle(commandDirectory)
		return nil, fmt.Errorf("create output file: %w", err)
	}
	return &commandOutputFile{
		file:      os.NewFile(uintptr(handle), name),
		handle:    handle,
		directory: commandDirectory,
	}, nil
}

func (f *commandOutputFile) protect() error {
	return restrictArtifactACLHandle(f.handle)
}

func (f *commandOutputFile) closeAndSync() error {
	if f.file != nil {
		if err := f.file.Close(); err != nil {
			return err
		}
		f.file = nil
		f.handle = windows.InvalidHandle
	}
	if f.directory == windows.InvalidHandle {
		return nil
	}
	err := windows.CloseHandle(f.directory)
	f.directory = windows.InvalidHandle
	return err
}

func (f *commandOutputFile) discard() error {
	var errs []error
	if f.file != nil {
		deleteOnClose := byte(1)
		var status windows.IO_STATUS_BLOCK
		errs = append(errs, windows.NtSetInformationFile(
			f.handle,
			&status,
			&deleteOnClose,
			1,
			windows.FileDispositionInformation,
		))
		errs = append(errs, f.file.Close())
		f.file = nil
		f.handle = windows.InvalidHandle
	}
	if f.directory != windows.InvalidHandle {
		errs = append(errs, windows.CloseHandle(f.directory))
		f.directory = windows.InvalidHandle
	}
	return errors.Join(errs...)
}

// openRegularArtifactFile resolves every path component relative to an open
// directory handle. Reparse points are opened as themselves and rejected, so
// they cannot redirect the final read outside the supplied root.
func openRegularArtifactFile(root, relativePath string) (*os.File, fs.FileInfo, error) {
	if _, err := strictRelativePath(relativePath); err != nil {
		return nil, nil, err
	}

	directory, err := openNoFollowArtifactRoot(root)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = windows.CloseHandle(directory) }()

	components := strings.Split(relativePath, "/")
	for index, component := range components {
		isFinal := index == len(components)-1
		next, err := openNoFollowArtifactComponent(directory, component, !isFinal)
		if err != nil {
			return nil, nil, err
		}
		if isFinal {
			file := os.NewFile(uintptr(next), component)
			info, err := file.Stat()
			if err != nil {
				_ = file.Close()
				return nil, nil, fmt.Errorf("stat artifact path component %q: %w", component, err)
			}
			if !info.Mode().IsRegular() {
				_ = file.Close()
				return nil, nil, fmt.Errorf("artifact is not a regular file")
			}
			return file, info, nil
		}
		windows.CloseHandle(directory)
		directory = next
	}

	return nil, nil, fmt.Errorf("artifact path is empty")
}

func openNoFollowArtifactRoot(root string) (windows.Handle, error) {
	return openArtifactRoot(root, windows.FILE_OPEN)
}

func openOrCreateNoFollowArtifactRoot(root string) (windows.Handle, error) {
	return openArtifactRoot(root, windows.FILE_OPEN_IF)
}

func openArtifactRoot(root string, disposition uint32) (windows.Handle, error) {
	if !filepath.IsAbs(root) {
		return windows.InvalidHandle, fmt.Errorf("supplied root is not absolute")
	}

	volume := filepath.VolumeName(root)
	if volume == "" {
		return windows.InvalidHandle, fmt.Errorf("supplied root has no volume")
	}
	volumeRoot := volume + string(filepath.Separator)
	directory, err := windows.CreateFile(
		windows.StringToUTF16Ptr(volumeRoot),
		windows.FILE_GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("open filesystem root: %w", err)
	}

	relativeRoot := strings.TrimPrefix(root, volume)
	components := strings.FieldsFunc(relativeRoot, func(r rune) bool { return r == '/' || r == '\\' })
	for index, component := range components {
		access := uint32(windows.FILE_GENERIC_READ)
		if disposition == windows.FILE_OPEN_IF && index == len(components)-1 {
			access |= windows.WRITE_DAC
		}
		next, err := openArtifactComponent(directory, component, true, disposition, access)
		if err != nil {
			windows.CloseHandle(directory)
			return windows.InvalidHandle, fmt.Errorf("open supplied root component %q: %w", component, err)
		}
		windows.CloseHandle(directory)
		directory = next
	}
	if disposition == windows.FILE_OPEN_IF {
		if err := restrictArtifactACLHandle(directory); err != nil {
			windows.CloseHandle(directory)
			return windows.InvalidHandle, err
		}
	}
	return directory, nil
}

func openOrCreatePrivateArtifactDirectory(parent windows.Handle, component string) (windows.Handle, error) {
	directory, err := openArtifactComponent(
		parent,
		component,
		true,
		windows.FILE_OPEN_IF,
		windows.FILE_GENERIC_READ|windows.WRITE_DAC,
	)
	if err != nil {
		return windows.InvalidHandle, err
	}
	if err := restrictArtifactACLHandle(directory); err != nil {
		windows.CloseHandle(directory)
		return windows.InvalidHandle, err
	}
	return directory, nil
}

func openNoFollowArtifactComponent(parent windows.Handle, component string, directory bool) (windows.Handle, error) {
	return openArtifactComponent(parent, component, directory, windows.FILE_OPEN, windows.FILE_GENERIC_READ)
}

func createNoFollowArtifactFile(parent windows.Handle, component string) (windows.Handle, error) {
	return openArtifactComponent(
		parent,
		component,
		false,
		windows.FILE_CREATE,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.WRITE_DAC|windows.DELETE,
	)
}

func openArtifactComponent(parent windows.Handle, component string, directory bool, disposition, access uint32) (windows.Handle, error) {
	name, err := windows.NewNTUnicodeString(component)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    name,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
	}

	var status windows.IO_STATUS_BLOCK
	var handle windows.Handle
	if err := windows.NtCreateFile(
		&handle,
		access,
		&attributes,
		&status,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		disposition,
		options,
		0,
		0,
	); err != nil {
		return windows.InvalidHandle, fmt.Errorf("open artifact path component %q: %w", component, err)
	}

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		windows.CloseHandle(handle)
		return windows.InvalidHandle, fmt.Errorf("stat artifact path component %q: %w", component, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(handle)
		return windows.InvalidHandle, fmt.Errorf("artifact path contains a reparse point")
	}
	if directory != (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) {
		windows.CloseHandle(handle)
		if directory {
			return windows.InvalidHandle, fmt.Errorf("artifact path component %q is not a directory", component)
		}
		return windows.InvalidHandle, fmt.Errorf("artifact is not a regular file")
	}
	return handle, nil
}
