// Layer: host-agent internal — WireGuard manager stub for non-Linux builds.
//go:build !linux

package wireguard

import "errors"

// TenantTunnel holds the WireGuard configuration for one tenant.
type TenantTunnel struct {
	TenantID      string
	Interface     string
	PrivateKey    string
	PeerPublicKey string
	Endpoint      string
	AllowedIPs    []string
	ListenPort    int
}

// Manager manages WireGuard interfaces per tenant (stub).
type Manager struct{}

// NewManager returns an error on non-Linux systems.
func NewManager() (*Manager, error) {
	return nil, errors.New("wireguard: only supported on Linux (requires WireGuard kernel module)")
}

// EnsureTunnel is not supported on non-Linux systems.
func (m *Manager) EnsureTunnel(_ *TenantTunnel) error {
	return errors.New("wireguard: only supported on Linux")
}

// RemoveTunnel is not supported on non-Linux systems.
func (m *Manager) RemoveTunnel(_ string) error {
	return errors.New("wireguard: only supported on Linux")
}

// Close is a no-op on non-Linux systems.
func (m *Manager) Close() error { return nil }
