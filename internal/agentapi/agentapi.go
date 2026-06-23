// Layer: shared internal — agent API types and HTTP client/server for the
// orchestrator ↔ host-agent internal communication channel.
//
// Why HTTP/JSON instead of raw gRPC at this stage:
//   The gRPC+protobuf path requires `protoc` to be available at build time.
//   Running `make proto` on a Linux CI box with protoc installed will replace
//   this package with the generated stubs; the types below are wire-compatible
//   with the proto definitions in proto/sandock.proto.
//
// Transport: HTTP/1.1 POST requests to the host-agent HTTP API server.
// All requests carry an X-Agent-Secret header for intra-cluster authentication.
// All message bodies are newline-free JSON.
package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ---------- Wire types (mirror proto/sandock.proto) ----------

// PlaceRequest asks the host-agent to create and boot a new sandbox VM.
type PlaceRequest struct {
	SandboxID       string   `json:"sandbox_id"`
	TenantID        string   `json:"tenant_id"`
	ImageRef        string   `json:"image_ref"`
	CPUMillis       uint32   `json:"cpu_millis"`
	MemMiB          uint32   `json:"mem_mib"`
	TimeoutMs       uint32   `json:"timeout_ms"`
	EgressAllowlist []string `json:"egress_allowlist,omitempty"`
	SnapshotKey     string   `json:"snapshot_key,omitempty"`
}

// PlaceResponse returns the running VM's handle.
type PlaceResponse struct {
	SandboxID string `json:"sandbox_id"`
	// VsockCID is the KVM vsock context ID for this VM.
	// Connect to (VsockCID, ExecPort) to reach the Rust vm-agent.
	VsockCID  uint32 `json:"vsock_cid"`
	// UnixSocket is populated when vsock is unavailable (Phase 1 dev mode).
	// It holds the path to the vm-agent Unix socket used for exec calls.
	UnixSocket string `json:"unix_socket,omitempty"`
}

// TerminateRequest asks the host-agent to stop a sandbox.
type TerminateRequest struct {
	SandboxID string `json:"sandbox_id"`
	Reason    string `json:"reason"`
}

// TerminateResponse confirms termination.
type TerminateResponse struct {
	SandboxID string `json:"sandbox_id"`
}

// ExecRequest asks the host-agent to run a command inside a sandbox.
type ExecRequest struct {
	SandboxID string `json:"sandbox_id"`
	Command   string `json:"command"`
	Stdin     string `json:"stdin,omitempty"`
	TimeoutMs uint32 `json:"timeout_ms"`
}

// ExecResponse returns the result of a completed exec.
type ExecResponse struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int32  `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
}

// HeartbeatRequest is sent by the orchestrator to check host health.
type HeartbeatRequest struct {
	HostID string `json:"host_id"`
}

// HeartbeatResponse returns real-time host metrics.
type HeartbeatResponse struct {
	HostID        string  `json:"host_id"`
	ActiveVMs     uint32  `json:"active_vms"`
	IdlePoolSize  uint32  `json:"idle_pool_size"`
	CPUUsagePct   float32 `json:"cpu_usage_pct"`
	MemUsedMiB    uint64  `json:"mem_used_mib"`
	MemTotalMiB   uint64  `json:"mem_total_mib"`
	Healthy       bool    `json:"healthy"`
}

// apiError is the JSON error body returned by the agent HTTP API.
type apiError struct {
	Error string `json:"error"`
}

// ---------- HTTP Client ----------

// Client is an HTTP client that calls the host-agent internal API.
type Client struct {
	baseURL    string
	secret     string
	httpClient *http.Client
}

// NewClient creates a Client targeting a host-agent at baseURL.
// secret must match the X-Agent-Secret header configured on the server.
func NewClient(baseURL, secret string) *Client {
	return &Client{
		baseURL: baseURL,
		secret:  secret,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// PlaceSandbox calls POST /internal/v1/place on the host-agent.
func (c *Client) PlaceSandbox(ctx context.Context, req *PlaceRequest) (*PlaceResponse, error) {
	var resp PlaceResponse
	if err := c.post(ctx, "/internal/v1/place", req, &resp); err != nil {
		return nil, fmt.Errorf("agentapi: PlaceSandbox: %w", err)
	}
	return &resp, nil
}

// TerminateSandbox calls POST /internal/v1/terminate on the host-agent.
func (c *Client) TerminateSandbox(ctx context.Context, req *TerminateRequest) (*TerminateResponse, error) {
	var resp TerminateResponse
	if err := c.post(ctx, "/internal/v1/terminate", req, &resp); err != nil {
		return nil, fmt.Errorf("agentapi: TerminateSandbox: %w", err)
	}
	return &resp, nil
}

// ExecInSandbox calls POST /internal/v1/exec on the host-agent.
func (c *Client) ExecInSandbox(ctx context.Context, req *ExecRequest) (*ExecResponse, error) {
	var resp ExecResponse
	if err := c.post(ctx, "/internal/v1/exec", req, &resp); err != nil {
		return nil, fmt.Errorf("agentapi: ExecInSandbox: %w", err)
	}
	return &resp, nil
}

// Heartbeat calls POST /internal/v1/heartbeat on the host-agent.
func (c *Client) Heartbeat(ctx context.Context, req *HeartbeatRequest) (*HeartbeatResponse, error) {
	var resp HeartbeatResponse
	if err := c.post(ctx, "/internal/v1/heartbeat", req, &resp); err != nil {
		return nil, fmt.Errorf("agentapi: Heartbeat: %w", err)
	}
	return &resp, nil
}

// post serialises req to JSON, sends a POST to path, and deserialises the response into out.
func (c *Client) post(ctx context.Context, path string, req, out any) error {
	buf, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Agent-Secret", c.secret)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var ae apiError
		_ = json.Unmarshal(body, &ae)
		if ae.Error != "" {
			return fmt.Errorf("status %d: %s", resp.StatusCode, ae.Error)
		}
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return nil
}
