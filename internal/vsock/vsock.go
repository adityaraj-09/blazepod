// Layer: host-agent internal — vsock client for communicating with the in-VM Rust agent.
// Firecracker exposes guest vsock on a host Unix socket (uds_path). The host sends
// "CONNECT <port>\n" and receives "OK <port>\n" before application data flows.
package vsock

import (
	"encoding/json"
	"fmt"
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

// Exec dials vm-agent inside the VM through Firecracker's vsock UDS and runs a command.
// udsPath is the Firecracker vsock.sock path for this sandbox.
func Exec(udsPath string, req ExecRequest) (*ExecResponse, error) {
	conn, err := dialFirecracker(udsPath, ExecPort)
	if err != nil {
		return nil, fmt.Errorf("vsock: dial %s port %d: %w", udsPath, ExecPort, err)
	}
	defer conn.Close()

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
