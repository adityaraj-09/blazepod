// Layer: host-agent internal — vsock client for communicating with the in-VM Rust agent.
// vsock (AF_VSOCK) is a socket family for VM↔host communication that bypasses
// the virtual network stack, providing lower latency and stronger isolation than
// TCP over the veth pair.
//
// The Rust vm-agent listens on a well-known vsock port inside the guest.
// The host-agent connects via the vsock CID assigned to the Firecracker VM.
//
// On Linux: go.sum includes golang.org/x/sys which exposes syscall.SockaddrVM.
// On macOS this package compiles but vsock Dial will fail at runtime (expected —
// vsock is a Linux KVM feature).
package vsock

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

const (
	// ExecPort is the vsock port the Rust vm-agent listens on inside every VM.
	ExecPort = 8888
)

// ExecRequest is the wire format for a command execution request sent to vm-agent.
type ExecRequest struct {
	Command   string `json:"command"`
	Stdin     string `json:"stdin,omitempty"`
	TimeoutMs uint32 `json:"timeout_ms"`
}

// ExecResponse is the wire format returned by vm-agent after process exit.
type ExecResponse struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
}

// Exec dials the vm-agent inside the VM identified by cid (vsock context ID),
// sends an ExecRequest, waits for the process to finish, and returns ExecResponse.
//
// cid is assigned by Firecracker and stored in the SandboxRecord at provisioning time.
func Exec(cid uint32, req ExecRequest) (*ExecResponse, error) {
	conn, err := dial(cid, ExecPort)
	if err != nil {
		return nil, fmt.Errorf("vsock: dial cid %d port %d: %w", cid, ExecPort, err)
	}
	defer conn.Close()

	// Set a generous read deadline equal to the command timeout plus headroom.
	deadlineDur := time.Duration(req.TimeoutMs)*time.Millisecond + 5*time.Second
	_ = conn.SetDeadline(time.Now().Add(deadlineDur))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("vsock: encode request: %w", err)
	}

	var resp ExecResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("vsock: decode response: %w", err)
	}
	return &resp, nil
}

// dial opens an AF_VSOCK connection to the given CID and port.
// Implemented in vsock_linux.go (Linux) and vsock_stub.go (other OS).
func dial(cid uint32, port uint32) (net.Conn, error) {
	return dialVsock(cid, port)
}
