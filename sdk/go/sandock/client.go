// Package sandock provides a Go client SDK for the Sandock sandbox execution platform.
// It mirrors the TypeScript SDK surface in sdk/typescript/src/client.ts.
//
// Usage:
//
//	client := sandock.NewClient("https://api.example.com", "sk-my-api-key")
//
//	// Create a sandbox
//	sb, err := client.Create(ctx, sandock.SandboxSpec{
//	    Image:     "python:3.12",
//	    CPUMillis: 500,
//	    MemoryMiB: 256,
//	    TimeoutMs: 30_000,
//	})
//
//	// Run a command
//	result, err := client.Exec(ctx, sb.ID, sandock.ExecRequest{Command: "python3 -c 'print(42)'"})
//	fmt.Println(result.Stdout)
//
//	// Clean up
//	err = client.Kill(ctx, sb.ID)
package sandock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ---------- Types ----------

// SandboxSpec defines the resource requirements for a new sandbox.
type SandboxSpec struct {
	// Image is the OCI image reference (e.g. "python:3.12").
	Image string `json:"image"`
	// CPUMillis is the CPU allocation in millicores (1000 = 1 full core).
	CPUMillis uint32 `json:"cpu_millis"`
	// MemoryMiB is the memory allocation in mebibytes.
	MemoryMiB uint32 `json:"memory_mib"`
	// TimeoutMs is the maximum lifetime of the sandbox in milliseconds.
	TimeoutMs uint32 `json:"timeout_ms"`
	// EgressAllowlist is a list of CIDRs the sandbox VM may send traffic to.
	// An empty list means the sandbox has no egress (fully isolated).
	EgressAllowlist []string `json:"egress_allowlist,omitempty"`
	// SnapshotKey resumes from a previously persisted snapshot.
	SnapshotKey string `json:"snapshot_key,omitempty"`
}

// Sandbox is the runtime record returned by Create and Get.
type Sandbox struct {
	// ID is the unique sandbox identifier (e.g. "sb-a1b2c3d4").
	ID string `json:"id"`
	// TenantID identifies the tenant that owns the sandbox.
	TenantID string `json:"tenant_id"`
	// State is the current lifecycle state: queued, provisioning, running, draining, terminated, failed.
	State string `json:"state"`
	// CreatedAt is when the sandbox was created.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the last time the state was updated.
	UpdatedAt time.Time `json:"updated_at"`
}

// ExecRequest asks the sandbox to run a command.
type ExecRequest struct {
	// Command is the shell command string to run inside the sandbox.
	Command string `json:"command"`
	// Stdin is optional data piped to the command's standard input.
	Stdin string `json:"stdin,omitempty"`
	// TimeoutMs overrides the command timeout (default: 30 000 ms).
	TimeoutMs uint32 `json:"timeout_ms,omitempty"`
}

// ExecResult holds the output of a completed exec.
type ExecResult struct {
	// Stdout is the combined standard output of the command.
	Stdout string `json:"stdout"`
	// Stderr is the combined standard error of the command.
	Stderr string `json:"stderr"`
	// ExitCode is the OS exit code (0 = success).
	ExitCode int `json:"exit_code"`
	// DurationMs is the wall-clock execution time in milliseconds.
	DurationMs int64 `json:"duration_ms"`
}

// APIError is returned when the server responds with an error HTTP status.
type APIError struct {
	// StatusCode is the HTTP status code.
	StatusCode int
	// Message is the server-provided error message.
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("sandock API error %d: %s", e.StatusCode, e.Message)
}

// ---------- Client ----------

// Client is a Sandock API client. Create one per process; it is safe for concurrent use.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a Client targeting the given base URL with the given API key.
// baseURL should not have a trailing slash (e.g. "https://api.example.com").
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// WithHTTPClient returns a copy of the client that uses the provided http.Client.
// Use this to set custom timeouts, TLS configuration, or a test transport.
func (c *Client) WithHTTPClient(hc *http.Client) *Client {
	cp := *c
	cp.httpClient = hc
	return &cp
}

// Create provisions a new sandbox and returns it in the "queued" or "provisioning" state.
func (c *Client) Create(ctx context.Context, spec SandboxSpec) (*Sandbox, error) {
	var sb Sandbox
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes", spec, &sb); err != nil {
		return nil, fmt.Errorf("sandock: Create: %w", err)
	}
	return &sb, nil
}

// Get retrieves the current state of a sandbox by ID.
func (c *Client) Get(ctx context.Context, sandboxID string) (*Sandbox, error) {
	var sb Sandbox
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+sandboxID, nil, &sb); err != nil {
		return nil, fmt.Errorf("sandock: Get: %w", err)
	}
	return &sb, nil
}

// List returns all sandboxes owned by the authenticated tenant.
func (c *Client) List(ctx context.Context) ([]*Sandbox, error) {
	var sandboxes []*Sandbox
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes", nil, &sandboxes); err != nil {
		return nil, fmt.Errorf("sandock: List: %w", err)
	}
	return sandboxes, nil
}

// Kill terminates a sandbox immediately.
func (c *Client) Kill(ctx context.Context, sandboxID string) error {
	if err := c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+sandboxID, nil, nil); err != nil {
		return fmt.Errorf("sandock: Kill: %w", err)
	}
	return nil
}

// Exec runs a command in a running sandbox and waits for the result.
// The sandbox must be in the "running" state.
func (c *Client) Exec(ctx context.Context, sandboxID string, req ExecRequest) (*ExecResult, error) {
	var result ExecResult
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/exec", req, &result); err != nil {
		return nil, fmt.Errorf("sandock: Exec: %w", err)
	}
	return &result, nil
}

// WaitRunning polls Get until the sandbox reaches the "running" state or a terminal state.
// Returns the sandbox when it is running, or an error if it reaches a terminal failure state.
// pollInterval controls how often the API is polled (default: 500 ms if zero).
func (c *Client) WaitRunning(ctx context.Context, sandboxID string, pollInterval time.Duration) (*Sandbox, error) {
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			sb, err := c.Get(ctx, sandboxID)
			if err != nil {
				return nil, err
			}
			switch sb.State {
			case "running":
				return sb, nil
			case "terminated", "failed":
				return nil, fmt.Errorf("sandock: WaitRunning: sandbox %s reached state %s", sandboxID, sb.State)
			}
		}
	}
}

// ---------- HTTP transport ----------

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(respBytes, &errBody)
		return &APIError{StatusCode: resp.StatusCode, Message: errBody.Error}
	}

	if out != nil && len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, out); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return nil
}
