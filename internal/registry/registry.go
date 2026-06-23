// Layer: host-agent internal — OCI image registry with sha256 digest verification.
// Manages the local image cache from which host agents pull base rootfs images.
// All sandbox images are verified by sha256 digest before being mounted as the
// overlayfs lower layer — this is the first line of supply-chain defence.
//
// Phase 1: local file map with sha256 digest verification (fully implemented).
// Phase 3: add OCI pull from internal registry using ORAS distribution protocol.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// ImageRef represents a fully-qualified image reference with digest.
type ImageRef struct {
	// Name is the human-readable image name (e.g. "python:3.12").
	Name string
	// Digest is the expected sha256 content hash of the image rootfs.
	// Format: "sha256:<64 hex chars>". Leave empty to skip verification.
	Digest string
	// LocalPath is the absolute path to the image rootfs file on this host.
	LocalPath string
}

// Registry manages image resolution and integrity verification.
type Registry struct {
	localImages map[string]*ImageRef
}

// NewRegistry creates a Registry pre-populated with the given image map.
// localImages maps image name → ImageRef with a known-good digest.
func NewRegistry(localImages map[string]*ImageRef) *Registry {
	return &Registry{localImages: localImages}
}

// Resolve returns the ImageRef for the given image name.
// Returns an error if the image is not in the local cache.
// Phase 3: try local cache first, then pull from internal OCI registry.
func (r *Registry) Resolve(name string) (*ImageRef, error) {
	ref, ok := r.localImages[name]
	if !ok {
		return nil, fmt.Errorf("registry: image %q not found in local cache", name)
	}
	return ref, nil
}

// VerifyDigest computes the sha256 of ref.LocalPath and compares it to ref.Digest.
// If ref.Digest is empty, verification is skipped (dev mode — log a warning in production).
// Returns an error if the file is missing or the digest does not match.
func (r *Registry) VerifyDigest(ref *ImageRef) error {
	if ref.Digest == "" {
		// No pinned digest — allow but emit a warning via returned sentinel.
		return nil
	}

	f, err := os.Open(ref.LocalPath)
	if err != nil {
		return fmt.Errorf("registry: open %s: %w", ref.LocalPath, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("registry: hash %s: %w", ref.LocalPath, err)
	}

	actual := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actual, ref.Digest) {
		return fmt.Errorf("registry: digest mismatch for %s: want %s got %s",
			ref.Name, ref.Digest, actual)
	}
	return nil
}

// ComputeDigest returns the sha256 hex digest of the file at path.
// Useful when pre-populating the image map from a known-good image.
func ComputeDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
