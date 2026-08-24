package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
)

type artifactFSCloser interface {
	closeArtifactFS() error
}

// descriptorRoot pins one directory handle. Paths are opened one component at
// a time relative to retained parent handles, and platformOpenRootComponent
// refuses reparse points and symbolic links instead of resolving them.
type descriptorRoot struct {
	base      *os.File
	closeOnce sync.Once
	closeErr  error
}

func openDescriptorRoot(directory string) (*descriptorRoot, error) {
	if err := ensureDescriptorRootSupported(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSource, err)
	}
	if err := platformValidateRootPath(directory); err != nil {
		return nil, fmt.Errorf("%w: unsafe root path: %v", ErrInvalidSource, err)
	}
	base, err := platformOpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("%w: securely open root directory: %w", ErrInvalidSource, err)
	}
	current, err := base.Stat()
	if err != nil || !current.IsDir() {
		_ = base.Close()
		return nil, fmt.Errorf("%w: configured root is not a directory", ErrInvalidSource)
	}
	return &descriptorRoot{base: base}, nil
}

func (root *descriptorRoot) Open(name string) (fs.File, error) {
	file, err := root.open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("%w: inspect opened path %q: %w", ErrInvalidArtifact, name, err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%w: non-regular path %q is not allowed", ErrInvalidArtifact, name)
	}
	if info.IsDir() {
		return &descriptorDirectory{file: file, info: info}, nil
	}
	return file, nil
}

func (root *descriptorRoot) Stat(name string) (fs.FileInfo, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return nil, statErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return info, nil
}

func (root *descriptorRoot) ReadDir(name string) ([]fs.DirEntry, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	directory, ok := file.(fs.ReadDirFile)
	if !ok {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %q is not a directory", ErrInvalidArtifact, name)
	}
	entries, readErr := directory.ReadDir(-1)
	if readErr != nil {
		_ = file.Close()
		return nil, readErr
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	return entries, nil
}

func (root *descriptorRoot) open(name string) (*os.File, error) {
	if err := validateDescriptorPath(name); err != nil {
		return nil, err
	}
	components := []string{"."}
	if name != "." {
		components = strings.Split(name, "/")
	}
	parent := root.base
	for index, component := range components {
		requireDirectory := index < len(components)-1 || name == "."
		next, err := platformOpenRootComponent(parent, component, requireDirectory)
		if parent != root.base {
			_ = parent.Close()
		}
		if err != nil {
			return nil, fmt.Errorf("%w: open %q without following symbolic links: %w", ErrInvalidArtifact, name, err)
		}
		parent = next
	}
	return parent, nil
}

func (root *descriptorRoot) sub(name string) (*rootArtifactFS, error) {
	file, err := root.open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.IsDir() {
		_ = file.Close()
		return nil, fmt.Errorf("%w: resolved artifact path is not a directory", ErrInvalidArtifact)
	}
	return &rootArtifactFS{root: &descriptorRoot{base: file}}, nil
}

func (root *descriptorRoot) close() error {
	root.closeOnce.Do(func() {
		root.closeErr = root.base.Close()
	})
	return root.closeErr
}

func validateDescriptorPath(name string) error {
	if name != "." && (!fs.ValidPath(name) || path.Clean(name) != name || strings.Contains(name, `\`) || hasWindowsVolumePrefix(name)) {
		return fmt.Errorf("%w: path %q is not normalized", ErrInvalidArtifact, name)
	}
	return nil
}

func validateDescriptorEntryName(name string) error {
	if name == "" || name == "." || name == ".." || !fs.ValidPath(name) || path.Clean(name) != name ||
		strings.ContainsAny(name, `/\`) || hasWindowsVolumePrefix(name) {
		return fmt.Errorf("%w: directory entry name %q is not a normalized path component", ErrInvalidArtifact, name)
	}
	return nil
}

type verifiedDirEntry struct {
	name string
	info fs.FileInfo
}

func (entry verifiedDirEntry) Name() string               { return entry.name }
func (entry verifiedDirEntry) IsDir() bool                { return entry.info.IsDir() }
func (entry verifiedDirEntry) Type() fs.FileMode          { return entry.info.Mode().Type() }
func (entry verifiedDirEntry) Info() (fs.FileInfo, error) { return entry.info, nil }

type descriptorDirectory struct {
	file *os.File
	info os.FileInfo
}

func (directory *descriptorDirectory) Read(buffer []byte) (int, error) {
	return directory.file.Read(buffer)
}

func (directory *descriptorDirectory) Close() error { return directory.file.Close() }

func (directory *descriptorDirectory) Stat() (fs.FileInfo, error) { return directory.info, nil }

func (directory *descriptorDirectory) ReadDir(count int) ([]fs.DirEntry, error) {
	entries, readErr := directory.file.ReadDir(count)
	if readErr != nil && len(entries) == 0 {
		return nil, readErr
	}
	snapshots := make([]fs.DirEntry, len(entries))
	for index, entry := range entries {
		if isNilValue(entry) {
			return nil, fmt.Errorf("%w: directory returned a nil entry", ErrInvalidArtifact)
		}
		entryName := entry.Name()
		if err := validateDescriptorEntryName(entryName); err != nil {
			return nil, err
		}
		opened, err := platformOpenRootComponent(directory.file, entryName, false)
		if err != nil {
			return nil, fmt.Errorf("%w: open directory entry %q without following symbolic links: %v", ErrInvalidArtifact, entryName, err)
		}
		info, statErr := opened.Stat()
		closeErr := opened.Close()
		if statErr != nil {
			return nil, statErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if isNilValue(info) {
			return nil, fmt.Errorf("%w: directory returned nil entry information", ErrInvalidArtifact)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: non-regular path %q is not allowed", ErrInvalidArtifact, entryName)
		}
		snapshots[index] = verifiedDirEntry{name: entryName, info: info}
	}
	return snapshots, readErr
}

type rootArtifactFS struct {
	root *descriptorRoot
}

func (filesystem *rootArtifactFS) Open(name string) (fs.File, error) {
	return filesystem.root.Open(name)
}

func (filesystem *rootArtifactFS) Stat(name string) (fs.FileInfo, error) {
	return filesystem.root.Stat(name)
}

func (filesystem *rootArtifactFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return filesystem.root.ReadDir(name)
}

func (filesystem *rootArtifactFS) closeArtifactFS() error {
	return filesystem.root.close()
}

type sharedRoot struct {
	root *descriptorRoot
}

func (root *sharedRoot) FS() fs.FS { return root.root }

func (root *sharedRoot) inspect(relative string) (os.FileInfo, error) {
	return root.root.Stat(relative)
}

func (root *sharedRoot) sub(relative string) (*rootArtifactFS, error) {
	return root.root.sub(relative)
}

func (root *sharedRoot) abort() error {
	return root.root.close()
}

func closeOwnedArtifacts(artifacts []Artifact) error {
	var result error
	for index, artifact := range artifacts {
		if err := artifact.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("skills: close artifact %d %q: %w", index, artifact.Locator, err))
		}
	}
	return result
}
