// Layer: host-agent internal — Linux overlayfs mount (Linux-only implementation).
// Extracted into a build-tagged file so the package compiles on macOS for unit testing,
// while the real mount/unmount calls are gated to Linux.
//go:build linux

package overlay

import (
	"fmt"
	"os"
	"syscall"
)

// mountOverlay mounts an overlayfs at paths.Merged using the Linux mount syscall.
func mountOverlay(paths Paths, lowerDir string) error {
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		lowerDir, paths.Upper, paths.Work)
	if err := syscall.Mount("overlay", paths.Merged, "overlay", 0, opts); err != nil {
		return fmt.Errorf("overlay: mount: %w", err)
	}
	return nil
}

// unmountOverlay unmounts the overlay at merged using the Linux mount syscall.
func unmountOverlay(merged string) error {
	if err := syscall.Unmount(merged, 0); err != nil {
		return fmt.Errorf("overlay: unmount %s: %w", merged, err)
	}
	return nil
}

// removeAll removes a directory tree.
func removeAll(path string) error {
	return os.RemoveAll(path)
}
