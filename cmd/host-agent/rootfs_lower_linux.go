//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// overlayLowerDir returns a directory path suitable as overlayfs lowerdir.
// base_rootfs may be either a directory or an ext4 image file; image files
// are loop-mounted read-only at <image-dir>/base-rootfs-lower.
// Loop mounts require root — run host-agent with sudo.
func overlayLowerDir(baseRootfs string) (string, error) {
	info, err := os.Stat(baseRootfs)
	if err != nil {
		return "", fmt.Errorf("stat base rootfs %s: %w", baseRootfs, err)
	}
	if info.IsDir() {
		return baseRootfs, nil
	}

	mountPoint := filepath.Join(filepath.Dir(baseRootfs), "base-rootfs-lower")
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return "", fmt.Errorf("mkdir overlay lower mount %s (need root?): %w", mountPoint, err)
	}

	if isMountedAt(mountPoint) {
		return mountPoint, nil
	}

	cmd := exec.Command("mount", "-o", "ro,loop", baseRootfs, mountPoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf(
			"mount %s at %s: %w: %s (run host-agent with sudo, or pre-mount: sudo mount -o ro,loop %s %s)",
			baseRootfs, mountPoint, err, out, baseRootfs, mountPoint,
		)
	}
	return mountPoint, nil
}

// isMountedAt reports whether path is a mount point by reading /proc/mounts.
func isMountedAt(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == abs {
			return true
		}
	}
	return false
}
