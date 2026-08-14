//go:build unix

package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func ensureDescriptorRootSupported() error { return nil }

func platformValidateRootPath(directory string) error {
	if !filepath.IsAbs(filepath.Clean(directory)) {
		return fmt.Errorf("root path is not absolute")
	}
	return nil
}

func platformOpenRoot(directory string) (*os.File, error) {
	cleaned := filepath.Clean(directory)
	if !filepath.IsAbs(cleaned) {
		return nil, fmt.Errorf("root path is not absolute")
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_DIRECTORY
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: "/", Err: err}
	}
	current := os.NewFile(uintptr(fd), "/")
	if current == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open returned an invalid root descriptor")
	}
	relative := strings.TrimPrefix(cleaned, "/")
	if relative == "" {
		return current, nil
	}
	for _, component := range strings.Split(relative, "/") {
		next, err := platformOpenRootComponent(current, component, true)
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

func platformOpenRootComponent(parent *os.File, name string, requireDirectory bool) (*os.File, error) {
	var metadata unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &metadata, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, &fs.PathError{Op: "fstatat", Path: name, Err: err}
	}
	fileType := uint32(metadata.Mode) & uint32(unix.S_IFMT)
	if fileType == uint32(unix.S_IFLNK) {
		return nil, &fs.PathError{Op: "fstatat", Path: name, Err: unix.ELOOP}
	}
	if requireDirectory && fileType != uint32(unix.S_IFDIR) {
		return nil, &fs.PathError{Op: "fstatat", Path: name, Err: unix.ENOTDIR}
	}
	if fileType != uint32(unix.S_IFDIR) && fileType != uint32(unix.S_IFREG) {
		return nil, fmt.Errorf("refusing to open non-regular filesystem object %q", name)
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_NOCTTY
	if requireDirectory || fileType == uint32(unix.S_IFDIR) {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(parent.Fd()), name, flags, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "openat", Path: name, Err: err}
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("openat returned an invalid descriptor")
	}
	return file, nil
}
