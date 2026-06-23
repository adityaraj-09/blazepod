// Layer: host-agent internal — Phase 2 Firecracker snapshot/restore pipeline.
// Snapshots allow warm VM starts in <30ms by resuming from a saved VM state
// instead of booting the kernel from scratch (~125ms).
//
// Full snapshot: saves all guest memory + device state to two files:
//   <snapshot_dir>/<key>.snap  — Firecracker state (device model, vcpu state)
//   <snapshot_dir>/<key>.mem   — guest memory
//
// Phase 1: stub — methods exist but are no-ops returning nil.
// Phase 2: wire to the Firecracker client PUT /snapshot/create and PUT /snapshot/load.
package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sandock/sandock/internal/firecracker"
)

// Manager handles the snapshot lifecycle for a host.
type Manager struct {
	snapshotDir string
}

// NewManager creates a Manager that stores snapshots under snapshotDir.
func NewManager(snapshotDir string) *Manager {
	return &Manager{snapshotDir: snapshotDir}
}

// Create creates a full snapshot of a running VM.
// key is a human-readable identifier (e.g. "python312-base").
// fc is a Firecracker API client connected to the running VM.
func (m *Manager) Create(ctx context.Context, key string, fc *firecracker.Client) error {
	snapPath := m.snapPath(key)
	memPath := m.memPath(key)
	if err := fc.SnapshotCreate(ctx, snapPath, memPath); err != nil {
		return fmt.Errorf("snapshot create %s: %w", key, err)
	}
	return nil
}

// Load restores a VM from a previously saved snapshot.
// The VM must be freshly started with the same machine-config as when the snapshot was taken.
func (m *Manager) Load(ctx context.Context, key string, fc *firecracker.Client) error {
	snapPath := m.snapPath(key)
	memPath := m.memPath(key)
	if err := fc.SnapshotLoad(ctx, snapPath, memPath); err != nil {
		return fmt.Errorf("snapshot load %s: %w", key, err)
	}
	return nil
}

// Exists returns true if both snapshot files for key exist on disk.
func (m *Manager) Exists(key string) bool {
	for _, path := range []string{m.snapPath(key), m.memPath(key)} {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	return true
}

func (m *Manager) snapPath(key string) string {
	return filepath.Join(m.snapshotDir, key+".snap")
}

func (m *Manager) memPath(key string) string {
	return filepath.Join(m.snapshotDir, key+".mem")
}
