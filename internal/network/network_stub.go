// Layer: host networking — stub for non-Linux builds.
// Veth/netns operations require Linux kernel netlink; this stub
// returns an error on any other OS so the rest of the codebase compiles.
//go:build !linux

package network

import "errors"

// SandboxNetwork holds network info for a sandbox (stub: always empty).
type SandboxNetwork struct {
	SandboxID string
	HostVeth  string
	PeerVeth  string
	NetnsPath string
	GuestIP   string
	GatewayIP string
}

// Setup is not supported on non-Linux systems.
func Setup(sandboxID string) (*SandboxNetwork, error) {
	return nil, errors.New("network: veth/netns setup is Linux-only")
}

// Destroy is not supported on non-Linux systems.
func Destroy(sandboxID string) error {
	return errors.New("network: veth/netns teardown is Linux-only")
}
