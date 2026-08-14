//go:build !unix && !windows

package skills

import (
	"fmt"
	"os"
	"runtime"
)

func ensureDescriptorRootSupported() error {
	return fmt.Errorf("secure descriptor-relative Skill loading is unsupported on %s", runtime.GOOS)
}

func platformValidateRootPath(string) error { return ensureDescriptorRootSupported() }

func platformOpenRoot(string) (*os.File, error) {
	return nil, ensureDescriptorRootSupported()
}

func platformOpenRootComponent(*os.File, string, bool) (*os.File, error) {
	return nil, ensureDescriptorRootSupported()
}
