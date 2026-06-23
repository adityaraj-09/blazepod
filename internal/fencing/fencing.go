// Layer: orchestrator — STONITH (Shoot The Other Node In The Head) host fencing.
// When a host-agent misses 3 consecutive heartbeats it is marked unhealthy and
// queued for fencing. Fencing kills any remaining power to the host to prevent
// split-brain VM escapes — a zombie VM with a stale network connection is a
// tenant isolation breach.
//
// Fencing backends:
//   IPMIFencer   — out-of-band power cut via IPMI ipmitool (physical/bare-metal).
//   AWSFencer    — force-stop EC2 instance via AWS API (cloud).
//   LogFencer    — logs the fence event only (testing/dev).
//
// Phase 1: LogFencer (no-op, logs to stderr).
// Phase 3: IPMIFencer / AWSFencer wired in via config.
package fencing

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// HostFencer defines the interface for STONITH fencing operations.
type HostFencer interface {
	// Fence forcibly powers off or isolates hostID.
	// Implementations must be idempotent — fencing an already-off host is not an error.
	Fence(ctx context.Context, hostID string) error
	// Unfence re-enables a host after it has been remediated and is safe to use.
	Unfence(ctx context.Context, hostID string) error
}

// ---------- Log Fencer (dev / test) ----------

// LogFencer logs fence/unfence events instead of taking real action.
// Use in development and CI where real out-of-band power control is unavailable.
type LogFencer struct{}

func (f *LogFencer) Fence(_ context.Context, hostID string) error {
	fmt.Fprintf(os.Stderr, "[fencing] FENCE host=%s time=%s\n", hostID, time.Now().UTC().Format(time.RFC3339))
	return nil
}

func (f *LogFencer) Unfence(_ context.Context, hostID string) error {
	fmt.Fprintf(os.Stderr, "[fencing] UNFENCE host=%s time=%s\n", hostID, time.Now().UTC().Format(time.RFC3339))
	return nil
}

// ---------- IPMI Fencer (physical hosts) ----------

// IPMIFencer uses ipmitool to perform out-of-band power control.
// Requires ipmitool in $PATH and IPMI credentials for each host.
type IPMIFencer struct {
	// BMCAddrs maps hostID → BMC (Baseboard Management Controller) IP address.
	BMCAddrs map[string]string
	// Username and Password are the IPMI credentials.
	Username string
	Password string
}

// Fence issues an IPMI "chassis power off" command to the host's BMC.
func (f *IPMIFencer) Fence(ctx context.Context, hostID string) error {
	bmc, ok := f.BMCAddrs[hostID]
	if !ok {
		return fmt.Errorf("fencing: no BMC address configured for host %s", hostID)
	}
	return f.ipmi(ctx, bmc, "chassis", "power", "off")
}

// Unfence powers the host back on via IPMI after remediation.
func (f *IPMIFencer) Unfence(ctx context.Context, hostID string) error {
	bmc, ok := f.BMCAddrs[hostID]
	if !ok {
		return fmt.Errorf("fencing: no BMC address configured for host %s", hostID)
	}
	return f.ipmi(ctx, bmc, "chassis", "power", "on")
}

func (f *IPMIFencer) ipmi(ctx context.Context, bmc string, args ...string) error {
	baseArgs := []string{
		"-H", bmc,
		"-U", f.Username,
		"-P", f.Password,
		"-I", "lanplus",
	}
	cmd := exec.CommandContext(ctx, "ipmitool", append(baseArgs, args...)...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("fencing: ipmitool %v: %w (output: %s)", args, err, out)
	}
	return nil
}

// ---------- Health Scorer ----------

// HealthScorer tracks per-host missed heartbeat counts and decides when to fence.
type HealthScorer struct {
	// MissThreshold is the number of consecutive missed heartbeats before fencing.
	MissThreshold int
	missed        map[string]int
}

// NewHealthScorer creates a HealthScorer with the given miss threshold.
func NewHealthScorer(threshold int) *HealthScorer {
	if threshold <= 0 {
		threshold = 3
	}
	return &HealthScorer{
		MissThreshold: threshold,
		missed:        make(map[string]int),
	}
}

// RecordSuccess resets the miss counter for hostID.
func (s *HealthScorer) RecordSuccess(hostID string) {
	s.missed[hostID] = 0
}

// RecordMiss increments the miss counter and returns true if the fence threshold is reached.
func (s *HealthScorer) RecordMiss(hostID string) bool {
	s.missed[hostID]++
	return s.missed[hostID] >= s.MissThreshold
}

// Misses returns the current miss count for hostID.
func (s *HealthScorer) Misses(hostID string) int {
	return s.missed[hostID]
}
