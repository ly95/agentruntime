//go:build windows

package skills

import (
	"reflect"
	"strings"
	"testing"
)

func windowsNTPath(directory string) (string, error) {
	parsed, err := parseWindowsRootPath(directory)
	if err != nil {
		return "", err
	}
	if len(parsed.components) == 0 {
		return parsed.anchor, nil
	}
	separator := `\`
	if strings.HasSuffix(parsed.anchor, separator) {
		separator = ""
	}
	return parsed.anchor + separator + strings.Join(parsed.components, `\`), nil
}

func TestWindowsNTPathRejectsDeviceNamespaces(t *testing.T) {
	for _, unsafePath := range []string{
		`\\.\PIPE\agent-go`,
		`\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy1`,
		`\\?\PIPE\agent-go`,
		`\\?\Volume{00000000-0000-0000-0000-000000000000}\skill`,
		`\\server\pipe\agent-go`,
		`\\server\mailslot\agent-go`,
		`\\server\IPC$\agent-go`,
		`\\?\UNC\server\pipe\agent-go`,
	} {
		if translated, err := windowsNTPath(unsafePath); err == nil {
			t.Fatalf("windowsNTPath(%q)=%q, want rejection", unsafePath, translated)
		}
	}
}

func TestWindowsRootPathPreflightRejectsDeviceNamespaces(t *testing.T) {
	for _, unsafePath := range []string{
		`\\.\PIPE\agent-go`,
		`\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy1`,
		`\\?\PIPE\agent-go`,
		`\\server\pipe\agent-go`,
		`\\?\UNC\server\mailslot\agent-go`,
	} {
		if err := platformValidateRootPath(unsafePath); err == nil {
			t.Fatalf("platformValidateRootPath(%q) unexpectedly succeeded", unsafePath)
		}
	}
}

func TestWindowsNTPathAcceptsDriveAndUNCFilesystemPaths(t *testing.T) {
	tests := map[string]string{
		`C:\skills\review`:             `\??\C:\skills\review`,
		`\\?\C:\skills\review`:         `\??\C:\skills\review`,
		`\\server\share\skills\review`: `\??\UNC\server\share\skills\review`,
		`\\?\UNC\server\share\review`:  `\??\UNC\server\share\review`,
	}
	for input, expected := range tests {
		translated, err := windowsNTPath(input)
		if err != nil || translated != expected {
			t.Fatalf("windowsNTPath(%q)=%q, %v; want %q", input, translated, err, expected)
		}
	}
}

func TestWindowsRootPathSeparatesNamespaceAnchorFromNoFollowComponents(t *testing.T) {
	tests := []struct {
		input      string
		anchor     string
		components []string
	}{
		{input: `C:\skills\review`, anchor: `\??\C:\`, components: []string{"skills", "review"}},
		{input: `\\?\C:\skills\review`, anchor: `\??\C:\`, components: []string{"skills", "review"}},
		{input: `\\server\share\skills\review`, anchor: `\??\UNC\server\share`, components: []string{"skills", "review"}},
		{input: `\\?\UNC\server\share\review`, anchor: `\??\UNC\server\share`, components: []string{"review"}},
	}
	for _, test := range tests {
		parsed, err := parseWindowsRootPath(test.input)
		if err != nil || parsed.anchor != test.anchor || !reflect.DeepEqual(parsed.components, test.components) {
			t.Fatalf("parseWindowsRootPath(%q)=%+v, %v; want anchor=%q components=%v", test.input, parsed, err, test.anchor, test.components)
		}
	}
}
