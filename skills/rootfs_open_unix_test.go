//go:build unix

package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPlatformOpenRootRejectsSymlinkComponent(t *testing.T) {
	parent, err := filepath.EvalSymlinks(testTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(parent, "linked")
	if err := os.Symlink(target, linked); err != nil {
		t.Fatal(err)
	}
	if opened, err := platformOpenRoot(linked); err == nil {
		_ = opened.Close()
		t.Fatal("platformOpenRoot followed a symbolic-link component")
	}
}

func TestPlatformOpenRootRejectsFIFOWithoutBlocking(t *testing.T) {
	parent, err := filepath.EvalSymlinks(testTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(parent, "root.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		opened, err := platformOpenRoot(fifo)
		if opened != nil {
			_ = opened.Close()
		}
		if err == nil {
			err = fmt.Errorf("FIFO unexpectedly opened as a root directory")
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("platformOpenRoot accepted a FIFO")
		}
	case <-time.After(time.Second):
		fd, openErr := unix.Open(fifo, unix.O_RDWR|unix.O_NONBLOCK, 0)
		if openErr == nil {
			_ = unix.Close(fd)
		}
		select {
		case <-result:
		case <-time.After(time.Second):
		}
		t.Fatal("platformOpenRoot blocked while opening a FIFO")
	}
}

func TestDescriptorDirectoryRejectsFIFOEntryWithoutBlocking(t *testing.T) {
	rootPath := testTempDir(t)
	fifo := filepath.Join(rootPath, "entry.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := openDescriptorRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.close() })

	result := make(chan error, 1)
	go func() {
		opened, openErr := root.Open(".")
		if openErr != nil {
			result <- openErr
			return
		}
		defer opened.Close()
		_, readErr := opened.(interface {
			ReadDir(int) ([]fs.DirEntry, error)
		}).ReadDir(-1)
		result <- readErr
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("descriptor directory accepted a FIFO entry")
		}
	case <-time.After(time.Second):
		fd, openErr := unix.Open(fifo, unix.O_RDWR|unix.O_NONBLOCK, 0)
		if openErr == nil {
			_ = unix.Close(fd)
		}
		t.Fatal("descriptor directory blocked while inspecting a FIFO entry")
	}
}
