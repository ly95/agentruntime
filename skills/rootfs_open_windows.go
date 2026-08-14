//go:build windows

package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func ensureDescriptorRootSupported() error { return nil }

func platformValidateRootPath(directory string) error {
	_, err := parseWindowsRootPath(directory)
	return err
}

func platformOpenRoot(directory string) (*os.File, error) {
	parsed, err := parseWindowsRootPath(directory)
	if err != nil {
		return nil, err
	}
	current, err := openWindowsObject(0, parsed.anchor, directory, true, false)
	if err != nil {
		return nil, err
	}
	for _, component := range parsed.components {
		next, openErr := openWindowsObject(windows.Handle(current.Fd()), component, component, true, true)
		_ = current.Close()
		if openErr != nil {
			return nil, openErr
		}
		current = next
	}
	return current, nil
}

func platformOpenRootComponent(parent *os.File, name string, requireDirectory bool) (*os.File, error) {
	displayName := name
	if name == "." {
		name = ""
	}
	return openWindowsObject(windows.Handle(parent.Fd()), name, displayName, requireDirectory, true)
}

func openWindowsObject(root windows.Handle, name, displayName string, requireDirectory, rejectReparse bool) (*os.File, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: root,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	if rejectReparse {
		attributes.Attributes |= windows.OBJ_DONT_REPARSE
	}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	options := uint32(windows.FILE_OPEN_FOR_BACKUP_INTENT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if requireDirectory {
		options |= windows.FILE_DIRECTORY_FILE
	}
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_GENERIC_READ|windows.SYNCHRONIZE,
		attributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		options,
		0,
		0,
	)
	if err != nil {
		if status, ok := err.(windows.NTStatus); ok {
			err = status.Errno()
		}
		return nil, &fs.PathError{Op: "ntcreatefile", Path: displayName, Err: err}
	}
	file := os.NewFile(uintptr(handle), displayName)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("NtCreateFile returned an invalid handle")
	}
	return file, nil
}

type windowsRootPath struct {
	anchor     string
	components []string
}

func parseWindowsRootPath(directory string) (windowsRootPath, error) {
	cleaned := filepath.Clean(directory)
	if !filepath.IsAbs(cleaned) {
		return windowsRootPath{}, fmt.Errorf("root path is not absolute")
	}
	if strings.HasPrefix(cleaned, `\\.\`) {
		return windowsRootPath{}, fmt.Errorf("Windows device paths are not supported")
	}
	if strings.HasPrefix(cleaned, `\\?\`) {
		remainder := cleaned[len(`\\?\`):]
		if len(remainder) >= len(`UNC\`) && strings.EqualFold(remainder[:len(`UNC\`)], `UNC\`) {
			return parseWindowsUNCPath(remainder[len(`UNC\`):])
		}
		if isWindowsDriveAbsolute(remainder) {
			return parseWindowsDrivePath(remainder)
		}
		return windowsRootPath{}, fmt.Errorf("extended Windows path uses an unsupported namespace")
	}
	if strings.HasPrefix(cleaned, `\\`) {
		return parseWindowsUNCPath(strings.TrimPrefix(cleaned, `\\`))
	}
	if isWindowsDriveAbsolute(cleaned) {
		return parseWindowsDrivePath(cleaned)
	}
	return windowsRootPath{}, fmt.Errorf("Windows root path uses an unsupported volume form")
}

func parseWindowsDrivePath(path string) (windowsRootPath, error) {
	components, err := windowsRelativeComponents(path[3:])
	if err != nil {
		return windowsRootPath{}, err
	}
	return windowsRootPath{anchor: `\??\` + path[:3], components: components}, nil
}

func parseWindowsUNCPath(remainder string) (windowsRootPath, error) {
	components := strings.Split(remainder, `\`)
	if len(components) < 2 || components[0] == "" || components[1] == "" {
		return windowsRootPath{}, fmt.Errorf("UNC root path requires a server and share")
	}
	share := components[1]
	if strings.EqualFold(share, "pipe") || strings.EqualFold(share, "mailslot") || strings.EqualFold(share, "IPC$") {
		return windowsRootPath{}, fmt.Errorf("UNC pseudo-share %q is not a filesystem root", share)
	}
	relative, err := windowsRelativeComponents(strings.Join(components[2:], `\`))
	if err != nil {
		return windowsRootPath{}, err
	}
	return windowsRootPath{anchor: `\??\UNC\` + components[0] + `\` + share, components: relative}, nil
}

func windowsRelativeComponents(relative string) ([]string, error) {
	if relative == "" {
		return nil, nil
	}
	components := strings.Split(relative, `\`)
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, fmt.Errorf("Windows root path contains an invalid component")
		}
	}
	return components, nil
}

func isWindowsDriveAbsolute(value string) bool {
	if len(value) < 3 || value[1] != ':' || value[2] != '\\' {
		return false
	}
	letter := value[0]
	return letter >= 'A' && letter <= 'Z' || letter >= 'a' && letter <= 'z'
}
