// Layer: host-agent — gRPC server implementation.
// Build tag: linux only. The host-agent uses Firecracker/KVM which requires Linux.
// The cmd/host-agent binary will only build and run on Linux hosts.
// This file implements the HostAgent gRPC service defined in proto/sandock.proto.
// It owns the per-host sandbox lifecycle: Firecracker VMM management, overlayfs
// mount/teardown, cgroup resource limits, and vsock exec forwarding.
//
// One host-agent process runs per physical/virtual host machine.
// The orchestrator discovers and calls it via the configured gRPC address.
package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sandock/sandock/internal/cgroup"
	"github.com/sandock/sandock/internal/config"
	"github.com/sandock/sandock/internal/firecracker"
	"github.com/sandock/sandock/internal/id"
	"github.com/sandock/sandock/internal/metrics"
	"github.com/sandock/sandock/internal/overlay"
	"github.com/sandock/sandock/internal/snapshot"
	"github.com/sandock/sandock/internal/vsock"

	ebpfloader "github.com/sandock/sandock/ebpf/loader"
)

// sandboxEntry tracks a running microVM on this host.
type sandboxEntry struct {
	sandboxID     string
	tenantID      string
	pid           int               // OS PID of the firecracker process
	vsockCID      uint32            // guest vsock context ID (for API visibility)
	vsockUDS      string            // Firecracker vsock.sock path used for exec
	vmAgentSocket string            // Unix socket path for vm-agent (dev mode when vsock unavailable)
	cgroupDir     string            // absolute path to the cgroup directory
	cancel        context.CancelFunc
}

// hostAgentServer implements the HostAgent gRPC service.
type hostAgentServer struct {
	cfg    *config.HostAgentConfig
	log    *zap.Logger
	hostID string

	mu        sync.RWMutex
	sandboxes map[string]*sandboxEntry

	snapManager *snapshot.Manager
}

func newHostAgentServer(cfg *config.HostAgentConfig, log *zap.Logger) *hostAgentServer {
	return &hostAgentServer{
		cfg:         cfg,
		log:         log,
		hostID:      id.NewHost(),
		sandboxes:   make(map[string]*sandboxEntry),
		snapManager: snapshot.NewManager(cfg.SnapshotDir),
	}
}

// PlaceSandbox creates and starts a new microVM sandbox.
// Flow: overlayfs setup → cgroup setup → start Firecracker process → configure VM → boot.
func (s *hostAgentServer) PlaceSandbox(ctx context.Context, req *PlaceRequest) (*PlaceResponse, error) {
	s.log.Info("placing sandbox",
		zap.String("sandbox_id", req.SandboxId),
		zap.String("tenant_id", req.TenantId),
		zap.Uint32("cpu_millis", req.CpuMillis),
		zap.Uint32("mem_mib", req.MemMib),
	)
	startTime := time.Now()

	// 1. Setup overlayfs: lowerdir=base rootfs, upperdir=per-sandbox writable layer.
	// base_rootfs may be an ext4 file; overlayfs lowerdir must be a directory.
	lowerDir, err := overlayLowerDir(s.cfg.BaseRootfs)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "overlay lowerdir: %v", err)
	}
	overlayCfg := overlay.Config{
		SandboxID: req.SandboxId,
		BaseDir:   s.cfg.SandboxDir,
		LowerDir:  lowerDir,
	}
	paths, err := overlay.Setup(overlayCfg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "overlay setup: %v", err)
	}
	s.log.Debug("overlayfs mounted", zap.String("merged", paths.Merged))

	// 2. Setup cgroup v2 resource limits for this sandbox.
	// memory.max must exceed guest RAM — the Firecracker process maps the full guest
	// region in its own address space. Add headroom so the VMM is not OOM-killed
	// before the API socket comes up.
	hostMemMiB := req.MemMib + 128
	cgroupDir, err := cgroup.Setup(req.SandboxId, cgroup.Limits{
		CPUMillis: req.CpuMillis,
		MemoryMiB: hostMemMiB,
	})
	if err != nil {
		// Clean up overlay on cgroup failure.
		_ = overlay.Teardown(overlayCfg)
		return nil, status.Errorf(codes.Internal, "cgroup setup: %v", err)
	}
	s.log.Debug("cgroup created", zap.String("cgroup_dir", cgroupDir))

	// 3. Prepare the Firecracker socket path and rootfs path.
	sandboxDir := filepath.Join(s.cfg.SandboxDir, req.SandboxId)
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		_ = overlay.Teardown(overlayCfg)
		_ = cgroup.Teardown(req.SandboxId)
		return nil, status.Errorf(codes.Internal, "mkdir sandbox dir: %v", err)
	}
	socketPath := filepath.Join(sandboxDir, "firecracker.sock")
	rootfsDisk := filepath.Join(sandboxDir, "rootfs.ext4")
	fcLogPath := filepath.Join(sandboxDir, "fc.log")

	// For Phase 1 we copy (or link) the base rootfs as the per-sandbox disk.
	// Phase 2 will replace this with QCOW2 copy-on-write backed by the base image.
	if err := copyFile(s.cfg.BaseRootfs, rootfsDisk); err != nil {
		_ = overlay.Teardown(overlayCfg)
		_ = cgroup.Teardown(req.SandboxId)
		return nil, status.Errorf(codes.Internal, "rootfs copy: %v", err)
	}

	// Firecracker opens --log-path without O_CREAT; the file must exist beforehand.
	if f, err := os.OpenFile(fcLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err != nil {
		_ = overlay.Teardown(overlayCfg)
		_ = cgroup.Teardown(req.SandboxId)
		return nil, status.Errorf(codes.Internal, "create firecracker log %s: %v", fcLogPath, err)
	} else {
		_ = f.Close()
	}

	// 4. Start the Firecracker process. It will create the socket at socketPath.
	vmCtx, cancel := context.WithTimeout(context.Background(), time.Duration(req.TimeoutMs)*time.Millisecond)
	fcCmd := exec.CommandContext(vmCtx, s.cfg.FirecrackerBin,
		"--api-sock", socketPath,
		"--log-path", fcLogPath,
	)
	fcCmd.Stdout = os.Stdout
	fcCmd.Stderr = os.Stderr

	if err := fcCmd.Start(); err != nil {
		cancel()
		_ = overlay.Teardown(overlayCfg)
		_ = cgroup.Teardown(req.SandboxId)
		return nil, status.Errorf(codes.Internal, "firecracker start: %v", err)
	}
	s.log.Info("firecracker process started", zap.Int("pid", fcCmd.Process.Pid))

	// 5. Wait for the Firecracker API socket before configuring the VM.
	// Do NOT add the process to the memory-limited cgroup yet — Firecracker needs
	// a small host footprint during API setup; cgroup limits apply at boot time.
	if err := waitForSocket(socketPath, fcLogPath, 15*time.Second); err != nil {
		cancel()
		_ = fcCmd.Process.Kill()
		_ = overlay.Teardown(overlayCfg)
		_ = cgroup.Teardown(req.SandboxId)
		return nil, status.Errorf(codes.DeadlineExceeded, "firecracker socket timeout: %v", err)
	}

	fc := firecracker.NewClient(socketPath)
	cfgCtx, cfgCancel := context.WithTimeout(ctx, 10*time.Second)
	defer cfgCancel()

	if err := fc.PutMachineConfig(cfgCtx, firecracker.MachineConfig{
		VcpuCount:  1,
		MemSizeMib: int(req.MemMib),
	}); err != nil {
		cancel()
		_ = fcCmd.Process.Kill()
		_ = overlay.Teardown(overlayCfg)
		_ = cgroup.Teardown(req.SandboxId)
		return nil, status.Errorf(codes.Internal, "firecracker machine-config: %v", err)
	}

	if err := fc.PutBootSource(cfgCtx, firecracker.BootSource{
		KernelImagePath: s.cfg.KernelImage,
		BootArgs:        "console=ttyS0 reboot=k panic=1 pci=off ip=169.254.0.2::169.254.0.1:255.255.255.0::eth0:off root=/dev/vda rw",
	}); err != nil {
		cancel()
		_ = fcCmd.Process.Kill()
		_ = overlay.Teardown(overlayCfg)
		_ = cgroup.Teardown(req.SandboxId)
		return nil, status.Errorf(codes.Internal, "firecracker boot-source: %v", err)
	}

	if err := fc.PutDrive(cfgCtx, firecracker.Drive{
		DriveID:      "rootfs",
		PathOnHost:   rootfsDisk,
		IsRootDevice: true,
		IsReadOnly:   false,
	}); err != nil {
		cancel()
		_ = fcCmd.Process.Kill()
		_ = overlay.Teardown(overlayCfg)
		_ = cgroup.Teardown(req.SandboxId)
		return nil, status.Errorf(codes.Internal, "firecracker drive: %v", err)
	}

	cid := guestCID(req.SandboxId)
	vsockUDS := filepath.Join(sandboxDir, "vsock.sock")
	_ = os.Remove(vsockUDS)
	if err := fc.PutVsock(cfgCtx, firecracker.Vsock{
		GuestCID: cid,
		UDSPath:  vsockUDS,
	}); err != nil {
		cancel()
		_ = fcCmd.Process.Kill()
		_ = overlay.Teardown(overlayCfg)
		_ = cgroup.Teardown(req.SandboxId)
		return nil, status.Errorf(codes.Internal, "firecracker vsock: %v", err)
	}

	// 6. Boot the VM — cold start or snapshot restore.
	// Apply cgroup limits to the Firecracker process right before guest RAM is mapped.
	if err := cgroup.AddPID(req.SandboxId, fcCmd.Process.Pid); err != nil {
		cancel()
		_ = fcCmd.Process.Kill()
		_ = overlay.Teardown(overlayCfg)
		_ = cgroup.Teardown(req.SandboxId)
		return nil, status.Errorf(codes.Internal, "cgroup addpid: %v", err)
	}

	if req.SnapshotKey != "" && s.snapManager.Exists(req.SnapshotKey) {
		// Warm start: restore from Firecracker snapshot instead of booting kernel.
		warmStart := time.Now()
		s.log.Info("warm start from snapshot", zap.String("key", req.SnapshotKey))
		if err := s.snapManager.Load(cfgCtx, req.SnapshotKey, fc); err != nil {
			cancel()
			_ = fcCmd.Process.Kill()
			_ = overlay.Teardown(overlayCfg)
			_ = cgroup.Teardown(req.SandboxId)
			return nil, status.Errorf(codes.Internal, "snapshot load: %v", err)
		}
		metrics.SandboxWarmStartDuration.WithLabelValues(req.TenantId, req.ImageRef, "local").
			Observe(time.Since(warmStart).Seconds())
		s.log.Info("VM restored from snapshot",
			zap.String("sandbox_id", req.SandboxId),
			zap.Duration("warm_start", time.Since(warmStart)),
		)
	} else {
		// Cold start: boot from kernel image.
		if err := fc.StartInstance(cfgCtx); err != nil {
			cancel()
			_ = fcCmd.Process.Kill()
			_ = overlay.Teardown(overlayCfg)
			_ = cgroup.Teardown(req.SandboxId)
			return nil, status.Errorf(codes.Internal, "firecracker start instance: %v", err)
		}
		coldStart := time.Since(startTime).Seconds()
		metrics.SandboxColdStartDuration.WithLabelValues(req.TenantId, req.ImageRef, "local").Observe(coldStart)
		s.log.Info("VM booted",
		zap.String("sandbox_id", req.SandboxId),
		zap.Float64("cold_start_s", coldStart),
		zap.Uint32("vsock_cid", cid),
		zap.String("vsock_uds", vsockUDS),
	)
	}

	// 7. Record the guest vsock CID configured via PUT /vsock.
	vsockCID := cid

	// Track the running sandbox.
	s.mu.Lock()
	s.sandboxes[req.SandboxId] = &sandboxEntry{
		sandboxID: req.SandboxId,
		tenantID:  req.TenantId,
		pid:       fcCmd.Process.Pid,
		vsockCID:  vsockCID,
		vsockUDS:  vsockUDS,
		cgroupDir: cgroupDir,
		cancel:    cancel,
	}
	active := len(s.sandboxes)
	s.mu.Unlock()

	metrics.VMPoolActiveCount.WithLabelValues(s.hostID).Set(float64(active))

	// 8. Attach eBPF TC egress filter for network policy enforcement.
	// The filter drops all traffic except to explicitly allowed CIDRs.
	ebpfObjPath := "/var/sandock/ebpf/egress_filter.o"
	hostVeth := "veth-" + req.SandboxId[:8]
	egressFilter := ebpfloader.NewEgressFilter(req.SandboxId, hostVeth)
	if err := egressFilter.Load(ebpfObjPath); err != nil {
		// Non-fatal: log and continue. The VM is running but egress is unfiltered.
		// Production deployments should treat this as fatal.
		s.log.Warn("eBPF egress filter load failed",
			zap.String("sandbox_id", req.SandboxId),
			zap.Error(err),
		)
	} else {
		for _, cidr := range req.EgressAllowlist {
			if err := egressFilter.AddCIDR(cidr); err != nil {
				s.log.Warn("eBPF AddCIDR failed", zap.String("cidr", cidr), zap.Error(err))
			}
		}
		s.log.Debug("eBPF egress filter attached", zap.String("sandbox_id", req.SandboxId))
	}

	// Background goroutine: reap the VM when it exits (timeout or kill).
	go s.watchVM(req.SandboxId, fcCmd, overlayCfg, req.TenantId)

	return &PlaceResponse{
		SandboxId: req.SandboxId,
		VsockCid:  vsockCID,
	}, nil
}

// TerminateSandbox kills the Firecracker process and cleans up overlayfs and cgroups.
func (s *hostAgentServer) TerminateSandbox(_ context.Context, req *TerminateRequest) (*TerminateResponse, error) {
	s.log.Info("terminating sandbox", zap.String("sandbox_id", req.SandboxId), zap.String("reason", req.Reason))

	s.mu.Lock()
	entry, ok := s.sandboxes[req.SandboxId]
	if !ok {
		s.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found on this host", req.SandboxId)
	}
	delete(s.sandboxes, req.SandboxId)
	s.mu.Unlock()

	entry.cancel()
	return &TerminateResponse{SandboxId: req.SandboxId}, nil
}

// ExecInSandbox forwards an exec request to the in-VM Rust agent via vsock.
func (s *hostAgentServer) ExecInSandbox(_ context.Context, req *ExecRequest) (*ExecResponse, error) {
	s.mu.RLock()
	entry, ok := s.sandboxes[req.SandboxId]
	s.mu.RUnlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not running on this host", req.SandboxId)
	}

	timeoutMs := req.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = 30_000
	}

	execStart := time.Now()
	resp, err := vsock.Exec(entry.vsockUDS, vsock.ExecRequest{
		Command:   req.Command,
		Stdin:     req.Stdin,
		TimeoutMs: timeoutMs,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "vsock exec: %v", err)
	}

	metrics.SandboxExecDuration.WithLabelValues(entry.tenantID, "base").Observe(time.Since(execStart).Seconds())

	return &ExecResponse{
		Stdout:     resp.Stdout,
		Stderr:     resp.Stderr,
		ExitCode:   int32(resp.ExitCode),
		DurationMs: resp.DurationMs,
	}, nil
}

// StreamLogs is a server-streaming RPC that tails logs from the Firecracker log file.
// Phase 1: reads the Firecracker log file and streams lines. Phase 2: tails from Kafka.
func (s *hostAgentServer) StreamLogs(req *LogRequest, stream HostAgent_StreamLogsServer) error {
	logPath := filepath.Join(s.cfg.SandboxDir, req.SandboxId, "fc.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		return status.Errorf(codes.NotFound, "log file for %s: %v", req.SandboxId, err)
	}
	return stream.Send(&LogChunk{
		SandboxId: req.SandboxId,
		Line:      string(data),
		Timestamp: time.Now().UnixNano(),
		Stream:    "stdout",
	})
}

// Heartbeat returns real-time host resource metrics to the orchestrator.
func (s *hostAgentServer) Heartbeat(_ context.Context, req *HeartbeatRequest) (*HeartbeatResponse, error) {
	s.mu.RLock()
	active := uint32(len(s.sandboxes))
	s.mu.RUnlock()

	return &HeartbeatResponse{
		HostId:    s.hostID,
		ActiveVms: active,
	}, nil
}

// runPoolReconciler runs continuously, maintaining the configured warm VM pool size.
// It checks the current idle pool against the PoolManager target every 5 seconds
// and boots or tears down VMs to hit the target. Phase 2 wiring: pool.PoolManager.
func (s *hostAgentServer) runPoolReconciler(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcilePool(ctx)
		}
	}
}

// reconcilePool compares idlePool size to cfg.WarmPoolSize and adjusts.
func (s *hostAgentServer) reconcilePool(ctx context.Context) {
	s.mu.RLock()
	active := len(s.sandboxes)
	s.mu.RUnlock()

	target := s.cfg.WarmPoolSize
	if target <= 0 {
		return // pool management disabled
	}

	s.log.Debug("pool reconcile", zap.Int("active", active), zap.Int("target_warm", target))
	// Phase 2: boot warm VMs from snapshot when pool is below target.
	// For now, emit pool metrics so dashboards are populated.
	metrics.VMPoolActiveCount.WithLabelValues(s.hostID).Set(float64(active))
	metrics.VMPoolIdleCount.WithLabelValues(s.hostID).Set(float64(target - active))
}

// watchVM waits for the Firecracker process to exit, then cleans up resources.
func (s *hostAgentServer) watchVM(sandboxID string, cmd *exec.Cmd, overlayCfg overlay.Config, tenantID string) {
	_ = cmd.Wait()

	s.log.Info("VM exited, cleaning up", zap.String("sandbox_id", sandboxID))

	// Check for OOM kills before teardown and record the counter.
	if oomCount, err := cgroup.OOMCount(sandboxID); err == nil && oomCount > 0 {
		metrics.SandboxOOMKillsTotal.WithLabelValues(tenantID, "base").Add(float64(oomCount))
	}

	// Remove from active map if not already removed by TerminateSandbox.
	s.mu.Lock()
	delete(s.sandboxes, sandboxID)
	active := len(s.sandboxes)
	s.mu.Unlock()

	metrics.VMPoolActiveCount.WithLabelValues(s.hostID).Set(float64(active))

	if err := cgroup.Teardown(sandboxID); err != nil {
		s.log.Warn("cgroup teardown failed", zap.String("sandbox_id", sandboxID), zap.Error(err))
	}
	if err := overlay.Teardown(overlayCfg); err != nil {
		s.log.Warn("overlay teardown failed", zap.String("sandbox_id", sandboxID), zap.Error(err))
	}
}

// waitForSocket polls for the existence of a Unix socket file up to timeout.
// If fcLogPath is set, includes a tail of the Firecracker log in the error.
func waitForSocket(path, fcLogPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	err := fmt.Errorf("socket %s did not appear within %s", path, timeout)
	if fcLogPath != "" {
		if data, readErr := os.ReadFile(fcLogPath); readErr == nil && len(data) > 0 {
			const maxTail = 2048
			tail := data
			if len(tail) > maxTail {
				tail = tail[len(tail)-maxTail:]
			}
			err = fmt.Errorf("%w; firecracker log tail: %s", err, tail)
		}
	}
	return err
}

// guestCID derives a stable vsock context ID (>= 3) from a sandbox ID.
func guestCID(sandboxID string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(sandboxID))
	c := (h.Sum32() % 65532) + 3
	if c < 3 {
		c = 3
	}
	return c
}

// copyFile copies src to dst for per-sandbox rootfs setup.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src %s: %w", src, err)
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create dst %s: %w", dst, err)
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
