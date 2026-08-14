//go:build !unix && !windows

package skills

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestFilesystemSourcesFailExplicitlyOnUnsupportedTarget(t *testing.T) {
	directory := testTempDir(t)
	for _, source := range []Source{
		NewLocalSource(LocalSourceConfig{ID: "local", Directories: []string{directory}}),
		NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex", PluginDirectories: []string{directory}}),
	} {
		set, err := LoadSet(t.Context(), source)
		if err == nil || set != nil || !errors.Is(err, ErrInvalidSource) ||
			!strings.Contains(err.Error(), "unsupported on "+runtime.GOOS) {
			t.Fatalf("source=%T set=%v error=%v", source, set, err)
		}
	}
}
