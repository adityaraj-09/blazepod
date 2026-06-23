// Layer: host-agent internal — overlayfs stubs for non-Linux platforms.
// Allows the overlay package to compile on macOS for development and unit testing.
// Real overlayfs requires Linux CAP_SYS_ADMIN; these stubs return clear errors.
//go:build !linux

package overlay

import (
	"fmt"
	"os"
)

func mountOverlay(_ Paths, _ string) error {
	return fmt.Errorf("overlay: mount not supported on this OS (Linux required)")
}

func unmountOverlay(_ string) error {
	return fmt.Errorf("overlay: unmount not supported on this OS (Linux required)")
}

func removeAll(path string) error {
	return os.RemoveAll(path)
}
