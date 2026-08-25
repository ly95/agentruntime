package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

func configuredDirectory(raw, kind string) (configured string, root *descriptorRoot, err error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil, fmt.Errorf("%w: %s directory is required", ErrInvalidSource, kind)
	}
	if raw != strings.TrimSpace(raw) {
		return "", nil, fmt.Errorf("%w: %s directory must be normalized", ErrInvalidSource, kind)
	}
	if err := validateControlFreeText(kind+" directory", raw); err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrInvalidSource, err)
	}
	if !filepath.IsAbs(raw) {
		return "", nil, fmt.Errorf("%w: %s directory must be absolute", ErrInvalidSource, kind)
	}
	if hasParentComponent(raw) {
		return "", nil, fmt.Errorf("%w: %s directory must not contain .. parent components", ErrInvalidSource, kind)
	}
	configured = filepath.Clean(raw)
	if configured != raw {
		return "", nil, fmt.Errorf("%w: %s directory must be canonical", ErrInvalidSource, kind)
	}
	root, err = openDescriptorRoot(configured)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, fmt.Errorf("%w: %s directory does not exist", ErrInvalidSource, kind)
		}
		return "", nil, fmt.Errorf("%w: open %s directory: %v", ErrInvalidSource, kind, err)
	}
	return configured, root, nil
}

func hasParentComponent(value string) bool {
	normalized := strings.ReplaceAll(value, `\`, "/")
	for _, component := range strings.Split(normalized, "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

func hasWindowsVolumePrefix(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	first := value[0]
	return first >= 'A' && first <= 'Z' || first >= 'a' && first <= 'z'
}
