// Layer: host networking — veth pair + network namespace setup.
// Each sandbox VM gets an isolated network namespace with a veth pair
// connecting it to the host bridge. eBPF TC hooks on the veth enforce egress rules.
//
// Topology:
//   [host bridge: sandock0] <--veth pair--> [per-sandbox veth inside netns]
//
// Build tags: Linux only — netlink calls require the Linux kernel.
//go:build linux

package network

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	// bridgeName is the host-side bridge all sandbox veths attach to.
	bridgeName = "sandock0"
	// netnsDir is where named network namespaces are pinned (ip-netns convention).
	netnsDir = "/var/run/netns"
	// subnetBase is the /30 subnet base; each sandbox gets 4 IPs.
	// Actual addressing is derived from the sandbox index.
	bridgeCIDR = "10.100.0.1/16"
)

// SandboxNetwork holds the veth pair names and namespace path for one sandbox.
type SandboxNetwork struct {
	SandboxID string
	// HostVeth is the veth end attached to the host bridge (e.g. "veth-sb123").
	HostVeth string
	// PeerVeth is the veth end inside the sandbox netns (always named "eth0" inside the ns).
	PeerVeth string
	// NetnsPath is the bind-mounted network namespace path.
	NetnsPath string
	// GuestIP is the IP assigned to the guest eth0 interface.
	GuestIP string
	// GatewayIP is the host-side IP (bridge) reachable from the guest.
	GatewayIP string
}

// Setup creates a network namespace, a veth pair, attaches the host side to
// the bridge, and configures the guest side with an IP address.
func Setup(sandboxID string) (*SandboxNetwork, error) {
	if err := ensureBridge(); err != nil {
		return nil, fmt.Errorf("network setup: ensure bridge: %w", err)
	}

	hostVeth := "veth-" + sandboxID[:8]
	netnsPath := filepath.Join(netnsDir, sandboxID)

	// Derive a deterministic /30 from the first 8 hex chars of the sandbox ID.
	guestIP, gatewayIP, err := allocateIPs(sandboxID)
	if err != nil {
		return nil, err
	}

	// 1. Create the named network namespace.
	if err := run("ip", "netns", "add", sandboxID); err != nil {
		return nil, fmt.Errorf("ip netns add: %w", err)
	}

	// 2. Create veth pair: hostVeth <-> eth0 (inside netns).
	if err := run("ip", "link", "add", hostVeth, "type", "veth", "peer", "name", "eth0",
		"netns", sandboxID); err != nil {
		_ = run("ip", "netns", "del", sandboxID)
		return nil, fmt.Errorf("ip link add veth: %w", err)
	}

	// 3. Bring up host veth and add to bridge.
	if err := run("ip", "link", "set", hostVeth, "master", bridgeName); err != nil {
		_ = cleanup(sandboxID, hostVeth)
		return nil, fmt.Errorf("attach to bridge: %w", err)
	}
	if err := run("ip", "link", "set", hostVeth, "up"); err != nil {
		_ = cleanup(sandboxID, hostVeth)
		return nil, fmt.Errorf("bring up host veth: %w", err)
	}

	// 4. Configure guest side inside the netns.
	if err := run("ip", "netns", "exec", sandboxID,
		"ip", "addr", "add", guestIP+"/30", "dev", "eth0"); err != nil {
		_ = cleanup(sandboxID, hostVeth)
		return nil, fmt.Errorf("assign guest IP: %w", err)
	}
	if err := run("ip", "netns", "exec", sandboxID, "ip", "link", "set", "eth0", "up"); err != nil {
		_ = cleanup(sandboxID, hostVeth)
		return nil, fmt.Errorf("bring up guest eth0: %w", err)
	}
	if err := run("ip", "netns", "exec", sandboxID,
		"ip", "route", "add", "default", "via", gatewayIP); err != nil {
		_ = cleanup(sandboxID, hostVeth)
		return nil, fmt.Errorf("set guest default route: %w", err)
	}

	return &SandboxNetwork{
		SandboxID: sandboxID,
		HostVeth:  hostVeth,
		PeerVeth:  "eth0",
		NetnsPath: netnsPath,
		GuestIP:   guestIP,
		GatewayIP: gatewayIP,
	}, nil
}

// Destroy removes the veth pair and the network namespace for a sandbox.
func Destroy(sandboxID string) error {
	hostVeth := "veth-" + sandboxID[:8]
	return cleanup(sandboxID, hostVeth)
}

// ensureBridge creates the sandock0 bridge if it doesn't exist.
func ensureBridge() error {
	// Check if the bridge already exists.
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if i.Name == bridgeName {
			return nil
		}
	}
	if err := run("ip", "link", "add", bridgeName, "type", "bridge"); err != nil {
		return err
	}
	if err := run("ip", "addr", "add", bridgeCIDR, "dev", bridgeName); err != nil {
		return err
	}
	return run("ip", "link", "set", bridgeName, "up")
}

// allocateIPs derives a deterministic /30 pair from sandboxID.
// It hashes the first 4 bytes of the ID to pick a slot in the 10.100.x.x range.
func allocateIPs(sandboxID string) (guestIP, gatewayIP string, err error) {
	// Extract 4 hex digits from the sandbox ID to form a deterministic slot.
	id := sandboxID
	if len(id) > 12 {
		id = id[len(id)-8:]
	}
	var slot uint32
	for _, c := range []byte(id) {
		if c >= '0' && c <= '9' {
			slot = slot*16 + uint32(c-'0')
		} else if c >= 'a' && c <= 'f' {
			slot = slot*16 + uint32(c-'a'+10)
		} else if c >= 'A' && c <= 'F' {
			slot = slot*16 + uint32(c-'A'+10)
		}
	}
	// Limit to 65000 slots to stay in 10.100.x.x.
	slot = slot % 65000
	// Each /30 uses 4 IPs starting at slot*4+4 (skip 0/1/2/3 reserved).
	base := slot*4 + 4
	third := (base >> 8) & 0xFF
	fourth := base & 0xFF
	gateway := fmt.Sprintf("10.100.%d.%d", third, fourth+1)
	guest := fmt.Sprintf("10.100.%d.%d", third, fourth+2)
	return guest, gateway, nil
}

// cleanup tears down the veth and removes the netns.
func cleanup(sandboxID, hostVeth string) error {
	var last error
	// Delete the veth (also deletes the peer inside the netns).
	if err := run("ip", "link", "del", hostVeth); err != nil {
		last = err
	}
	// Delete the network namespace.
	if err := run("ip", "netns", "del", sandboxID); err != nil {
		last = err
	}
	return last
}

// run executes a shell command, returning its combined output on failure.
func run(args ...string) error {
	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %w", args, err)
	}
	return nil
}
