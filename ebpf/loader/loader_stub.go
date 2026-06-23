// Layer: host-agent internal — eBPF TC egress filter loader stub for non-Linux.
// All operations return errors immediately; this file exists only so the package
// compiles on macOS/Windows for development.
//go:build !linux

package loader

import "errors"

// EgressFilter manages the lifecycle of a per-sandbox eBPF TC egress filter.
type EgressFilter struct {
	SandboxID string
	HostVeth  string
}

// NewEgressFilter creates an EgressFilter configuration (non-Linux stub).
func NewEgressFilter(sandboxID, hostVeth string) *EgressFilter {
	return &EgressFilter{SandboxID: sandboxID, HostVeth: hostVeth}
}

// Load returns an error on non-Linux systems.
func (f *EgressFilter) Load(_ string) error {
	return errors.New("ebpf loader: only supported on Linux (requires CAP_NET_ADMIN + CAP_BPF)")
}

// AddCIDR returns an error on non-Linux systems.
func (f *EgressFilter) AddCIDR(_ string) error {
	return errors.New("ebpf loader: only supported on Linux")
}

// Detach is a no-op on non-Linux systems.
func (f *EgressFilter) Detach() error { return nil }
