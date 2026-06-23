// Layer: host-agent internal — filesystem persistence via tar.zst archives.
// Saves and restores a sandbox overlay upper directory to/from a compressed archive.
// Archives are written atomically (temp file + rename) and can be uploaded to
// an object store (S3/R2) for cross-host restore.
//
// Archive format: tar stream compressed with zstd (default level 3).
// File layout inside the archive mirrors the directory structure at srcDir.
//
// Phase 1: local file save/restore only.
// Phase 3: add S3Store that streams the archive to S3 using multipart upload.
package persistence

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Store persists and restores sandbox filesystem state.
type Store interface {
	// Save compresses srcDir into a tar.zst archive and stores it at key.
	Save(key, srcDir string) error
	// Restore extracts the archive at key into dstDir.
	Restore(key, dstDir string) error
	// Exists returns true if an archive for key exists.
	Exists(key string) (bool, error)
	// Delete removes the archive for key.
	Delete(key string) error
}

// LocalStore saves archives on the local filesystem under baseDir.
// Files are named <key>.tar.zst.
type LocalStore struct {
	baseDir string
}

// NewLocalStore creates a LocalStore that writes to baseDir (created if missing).
func NewLocalStore(baseDir string) (*LocalStore, error) {
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return nil, fmt.Errorf("persistence: create base dir: %w", err)
	}
	return &LocalStore{baseDir: baseDir}, nil
}

// archivePath returns the file path for the given key.
func (s *LocalStore) archivePath(key string) string {
	// Sanitise key: replace slashes with dashes to avoid path traversal.
	safe := strings.ReplaceAll(key, "/", "-")
	return filepath.Join(s.baseDir, safe+".tar.zst")
}

// Save compresses srcDir into <key>.tar.zst.
// Write is atomic: the archive is first written to a temp file and then renamed.
func (s *LocalStore) Save(key, srcDir string) error {
	dest := s.archivePath(key)
	tmp := dest + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("persistence: create tmp: %w", err)
	}

	enc, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		f.Close()
		return fmt.Errorf("persistence: zstd writer: %w", err)
	}

	tw := tar.NewWriter(enc)
	if err := tarDir(tw, srcDir, srcDir); err != nil {
		tw.Close()
		enc.Close()
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("persistence: tar %s: %w", srcDir, err)
	}
	if err := tw.Close(); err != nil {
		enc.Close()
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := enc.Close(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// Restore decompresses the archive at key into dstDir.
// dstDir must exist; files are created with their original permissions.
func (s *LocalStore) Restore(key, dstDir string) error {
	src := s.archivePath(key)
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("persistence: open archive %s: %w", src, err)
	}
	defer f.Close()

	dec, err := zstd.NewReader(f)
	if err != nil {
		return fmt.Errorf("persistence: zstd reader: %w", err)
	}
	defer dec.Close()

	tr := tar.NewReader(dec)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("persistence: tar next: %w", err)
		}

		// Security: prevent path traversal.
		target := filepath.Join(dstDir, filepath.Clean("/"+hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(dstDir)+string(os.PathSeparator)) {
			return fmt.Errorf("persistence: path traversal detected: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return fmt.Errorf("persistence: mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return fmt.Errorf("persistence: create file %s: %w", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil { //nolint:gosec
				out.Close()
				return fmt.Errorf("persistence: write %s: %w", target, err)
			}
			out.Close()
		case tar.TypeSymlink:
			if err := os.Symlink(hdr.Linkname, target); err != nil && !os.IsExist(err) {
				return fmt.Errorf("persistence: symlink %s: %w", target, err)
			}
		}
	}
	return nil
}

// Exists returns true if an archive for key exists and is readable.
func (s *LocalStore) Exists(key string) (bool, error) {
	_, err := os.Stat(s.archivePath(key))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// Delete removes the archive for key.
func (s *LocalStore) Delete(key string) error {
	return os.Remove(s.archivePath(key))
}

// tarDir recursively adds all files in dir to tw.
// baseDir is stripped from the header name so archives are relative.
func tarDir(tw *tar.Writer, dir, baseDir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Build a relative archive path.
		rel, err := filepath.Rel(baseDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel

		// Resolve symlinks for the link target.
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			hdr.Linkname = link
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	})
}
