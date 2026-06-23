// Layer: host-agent internal — overlayfs mount lifecycle.
// Each sandbox gets an overlayfs mount combining:
//   - lowerdir: the shared read-only base rootfs image (pre-populated by deploy scripts).
//   - upperdir: a per-sandbox writable directory wiped on sandbox exit.
//   - workdir:  required by the kernel for overlayfs bookkeeping (must be same FS as upperdir).
//   - merged:   the combined view presented to the VM as its rootfs block device.
//
// After sandbox exit the upper/ dir is wiped with a sha256 integrity check before
// the slot is marked idle — this prevents cross-tenant data leaks.
//
// Platform-specific mount/unmount helpers live in overlay_linux.go (Linux)
// and overlay_stub.go (other OS) so the package compiles everywhere.
package overlay

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Config holds the paths needed to set up an overlayfs for one sandbox.
type Config struct {
	// SandboxID is used to name the per-sandbox directory tree.
	SandboxID string
	// BaseDir is the root under which per-sandbox dirs are created.
	BaseDir string
	// LowerDir is the shared read-only base image directory.
	LowerDir string
}

// Paths contains the resolved filesystem paths for one sandbox overlayfs.
type Paths struct {
	Upper  string
	Work   string
	Merged string
}

// Setup creates the upper/, work/, and merged/ directories and mounts overlayfs.
// Returns the Paths on success.
func Setup(cfg Config) (Paths, error) {
	base := filepath.Join(cfg.BaseDir, cfg.SandboxID)

	paths := Paths{
		Upper:  filepath.Join(base, "upper"),
		Work:   filepath.Join(base, "work"),
		Merged: filepath.Join(base, "merged"),
	}

	for _, dir := range []string{paths.Upper, paths.Work, paths.Merged} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return Paths{}, fmt.Errorf("overlay: mkdir %s: %w", dir, err)
		}
	}

	if err := mountOverlay(paths, cfg.LowerDir); err != nil {
		return Paths{}, err
	}
	return paths, nil
}

// Teardown unmounts the overlay and wipes the writable upper layer.
func Teardown(cfg Config) error {
	base := filepath.Join(cfg.BaseDir, cfg.SandboxID)
	merged := filepath.Join(base, "merged")
	upper := filepath.Join(base, "upper")

	if err := unmountOverlay(merged); err != nil {
		return err
	}

	if err := removeAll(upper); err != nil {
		return fmt.Errorf("overlay: remove upper %s: %w", upper, err)
	}

	if err := os.MkdirAll(upper, 0755); err != nil {
		return fmt.Errorf("overlay: recreate upper %s: %w", upper, err)
	}
	if err := verifyEmptyUpper(upper); err != nil {
		return fmt.Errorf("overlay: upper not empty after wipe for %s: %w", cfg.SandboxID, err)
	}
	return nil
}

// verifyEmptyUpper walks the upper directory and returns an error if any files remain.
func verifyEmptyUpper(upper string) error {
	h := sha256.New()
	var fileCount int

	err := filepath.WalkDir(upper, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == upper {
			return nil
		}
		fileCount++
		h.Write([]byte(path))
		return nil
	})
	if err != nil {
		return fmt.Errorf("walkdir: %w", err)
	}
	if fileCount > 0 {
		return fmt.Errorf("upper dir contains %d entries after wipe (hash %x)", fileCount, h.Sum(nil))
	}
	return nil
}
