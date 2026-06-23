//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// overlayLowerDir returns a directory path suitable as overlayfs lowerdir.
// base_rootfs may be either a directory or an ext4 image file; image files
// are loop-mounted read-only at <image-dir>/base-rootfs-lower.
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
		return "", fmt.Errorf("mkdir overlay lower mount %s: %w", mountPoint, err)
	}

	mounted, err := isMountPoint(mountPoint)
	if err != nil {
		return "", err
	}
	if mounted {
		return mountPoint, nil
	}

	cmd := exec.Command("mount", "-o", "ro,loop", baseRootfs, mountPoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("mount %s at %s: %w: %s", baseRootfs, mountPoint, err, out)
	}
	return mountPoint, nil
}

func isMountPoint(path string) (bool, error) {
	cmd := exec.Command("mountpoint", "-q", path)
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("mountpoint %s: %w", path, err)
	}
	return true, nil
}
