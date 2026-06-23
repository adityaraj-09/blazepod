// Layer: shared internal — sandbox specification types.
// SandboxSpec is the single source of truth for what a sandbox looks like.
// It flows from the API layer → orchestrator → host-agent → Firecracker.
// Every layer validates only the fields it owns; it must not modify others.
package spec

import (
	"errors"
	"strings"
)

// SandboxSpec is the user-supplied description of a desired sandbox.
type SandboxSpec struct {
	// Image is the OCI image reference to run inside the VM.
	// Phase 1: only "base" is supported (maps to the configured base rootfs).
	Image string `json:"image"`
	// CPUMillis is the CPU allocation in thousandths of a CPU.
	// 500 = 0.5 CPU, 1000 = 1 full CPU.
	CPUMillis uint32 `json:"cpu_millis"`
	// MemoryMiB is the memory limit in mebibytes.
	MemoryMiB uint32 `json:"memory_mib"`
	// TimeoutMs is the maximum wall-clock lifetime of the sandbox in milliseconds.
	TimeoutMs uint32 `json:"timeout_ms"`
	// EgressAllowlist is an optional list of hostnames or CIDRs allowed for egress.
	// Empty list means all egress is blocked (Phase 2 enforcement via eBPF).
	EgressAllowlist []string `json:"egress_allowlist,omitempty"`
	// TenantID is the authenticated tenant this sandbox belongs to.
	// Set by the API layer after authentication — not user-supplied.
	TenantID string `json:"-"`
}

// Validate returns an error if the spec contains invalid or out-of-range values.
func (s *SandboxSpec) Validate() error {
	var errs []string

	if s.Image == "" {
		errs = append(errs, "image must not be empty")
	}
	if s.CPUMillis == 0 || s.CPUMillis > 32_000 {
		errs = append(errs, "cpu_millis must be between 1 and 32000")
	}
	if s.MemoryMiB < 64 || s.MemoryMiB > 32_768 {
		errs = append(errs, "memory_mib must be between 64 and 32768")
	}
	if s.TimeoutMs == 0 || s.TimeoutMs > 3_600_000 {
		errs = append(errs, "timeout_ms must be between 1 and 3600000 (1 hour)")
	}

	if len(errs) > 0 {
		return errors.New("spec validation: " + strings.Join(errs, "; "))
	}
	return nil
}

// ExecRequest is the payload for running a command inside a running sandbox.
type ExecRequest struct {
	// SandboxID identifies the target running sandbox.
	SandboxID string `json:"sandbox_id"`
	// Command is the shell command to run inside the VM.
	Command string `json:"command"`
	// Stdin is optional data to write to the process stdin.
	Stdin string `json:"stdin,omitempty"`
	// TimeoutMs is the maximum execution time. Defaults to 30000ms if zero.
	TimeoutMs uint32 `json:"timeout_ms,omitempty"`
}

// ExecResult is the response from a completed exec call.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	// DurationMs is wall-clock time from exec start to process exit.
	DurationMs int64 `json:"duration_ms"`
}
