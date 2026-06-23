// Layer: orchestrator — main entry point.
// The orchestrator is the central nervous system: it owns the scheduler,
// sandbox state store, host registry, and heartbeat monitor.
// It exposes an HTTP server that cmd/api calls via the agentapi.Client.
//
// Phase 2: the orchestrator is now a standalone process.
// cmd/api connects to it over HTTP (same agentapi.Client used for host-agents).
//
// Usage:
//   orchestrator --config /etc/sandock/config.yaml
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/sandock/sandock/internal/agentapi"
	"github.com/sandock/sandock/internal/config"
	"github.com/sandock/sandock/internal/fencing"
	"github.com/sandock/sandock/internal/log"
	"github.com/sandock/sandock/internal/metrics"
	"github.com/sandock/sandock/internal/spec"
	"github.com/sandock/sandock/internal/state"
)

var configPath = flag.String("config", "/etc/sandock/config.yaml", "path to config YAML")

func main() {
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		os.Stderr.WriteString("orchestrator: load config: " + err.Error() + "\n")
		os.Exit(1)
	}

	log.MustInit(cfg.Log.Level, cfg.Log.Format)
	logger := log.L.With(zap.String("service", "orchestrator"))

	store := state.NewStore()
	sched := NewScheduler(logger, store)

	// Register all configured host agents at startup.
	// Phase 2: dynamic discovery via gRPC stream or service mesh.
	for _, ha := range cfg.HostAgents {
		sched.RegisterHost(&HostInfo{
			ID:       ha.ID,
			Addr:     ha.HTTPAddr,
			Healthy:  true,
			LastSeen: time.Now(),
		})
		logger.Info("registered host agent", zap.String("id", ha.ID), zap.String("addr", ha.HTTPAddr))
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Build the gRPC placer that makes real HTTP calls to host agents.
	placer := &httpPlacer{
		store:       store,
		agentSecret: cfg.HostAgent.AgentSecret,
		log:         logger,
	}

	go sched.RunDispatchLoop(ctx, placer)

	// Start the heartbeat monitor — periodically polls all hosts and updates health.
	go runHeartbeatMonitor(ctx, sched, cfg.HostAgent.AgentSecret, logger)

	// Emit scheduler queue depth to Prometheus every 5s.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sched.queueMu.Lock()
				depth := float64(sched.queue.Len())
				sched.queueMu.Unlock()
				metrics.SchedulerQueueDepth.Set(depth)
			}
		}
	}()

	logger.Info("orchestrator started",
		zap.String("state_backend", cfg.Orchestrator.StateBackend),
		zap.String("grpc_addr", cfg.Orchestrator.GRPCAddr),
	)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	logger.Info("orchestrator shutting down")
	cancel()
}

// httpPlacer calls the host-agent HTTP API to provision VMs.
type httpPlacer struct {
	store       *state.Store
	agentSecret string
	log         *zap.Logger
}

func (p *httpPlacer) PlaceSandbox(ctx context.Context, sandboxID string, host *HostInfo, sp *spec.SandboxSpec) (uint32, error) {
	start := time.Now()
	client := agentapi.NewClient("http://"+host.Addr, p.agentSecret)

	resp, err := client.PlaceSandbox(ctx, &agentapi.PlaceRequest{
		SandboxID:       sandboxID,
		TenantID:        sp.TenantID,
		ImageRef:        sp.Image,
		CPUMillis:       sp.CPUMillis,
		MemMiB:          sp.MemoryMiB,
		TimeoutMs:       sp.TimeoutMs,
		EgressAllowlist: sp.EgressAllowlist,
	})
	if err != nil {
		return 0, err
	}

	// Store the vsock CID / unix socket path in the state record so exec calls can reach the VM.
	if err := p.store.SetVsockCID(sandboxID, resp.VsockCID, resp.UnixSocket); err != nil {
		p.log.Warn("failed to store vsock CID", zap.String("sandbox_id", sandboxID), zap.Error(err))
	}

	p.log.Info("sandbox placed",
		zap.String("sandbox_id", sandboxID),
		zap.String("host_addr", host.Addr),
		zap.Duration("latency", time.Since(start)),
	)
	return resp.VsockCID, nil
}

// runHeartbeatMonitor polls all registered hosts every 10 seconds.
// On missed heartbeats it downgrades the host health score and triggers fencing
// after MissThreshold consecutive failures.
func runHeartbeatMonitor(ctx context.Context, sched *Scheduler, agentSecret string, log *zap.Logger) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	scorer := fencing.NewHealthScorer(3)
	// Phase 3: swap LogFencer for IPMIFencer/AWSFencer based on config.
	var fence fencing.HostFencer = &fencing.LogFencer{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sched.mu.RLock()
			hosts := make([]*HostInfo, 0, len(sched.hosts))
			for _, h := range sched.hosts {
				hosts = append(hosts, h)
			}
			sched.mu.RUnlock()

			for _, host := range hosts {
				client := agentapi.NewClient("http://"+host.Addr, agentSecret)
				hbCtx, hbCancel := context.WithTimeout(ctx, 5*time.Second)
				resp, err := client.Heartbeat(hbCtx, &agentapi.HeartbeatRequest{HostID: host.ID})
				hbCancel()

				if err != nil {
					log.Warn("heartbeat failed",
						zap.String("host_id", host.ID),
						zap.Int("consecutive_misses", scorer.Misses(host.ID)),
						zap.Error(err),
					)
					// Trigger fencing after MissThreshold consecutive failures.
					if scorer.RecordMiss(host.ID) {
						sched.UpdateHostHealth(host.ID, false)
						log.Error("host marked unhealthy — initiating STONITH fence",
							zap.String("host_id", host.ID),
						)
						fenceCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
						if fenceErr := fence.Fence(fenceCtx, host.ID); fenceErr != nil {
							log.Error("STONITH fence failed", zap.String("host_id", host.ID), zap.Error(fenceErr))
						} else {
							log.Warn("STONITH fence succeeded", zap.String("host_id", host.ID))
						}
						cancel()
					}
				} else {
					scorer.RecordSuccess(host.ID)
					sched.mu.Lock()
					if h, ok := sched.hosts[host.ID]; ok {
						h.ActiveVMs = resp.ActiveVMs
						h.IdlePool = resp.IdlePoolSize
						h.CPUUsagePct = resp.CPUUsagePct
						h.LastSeen = time.Now()
						h.Healthy = true
					}
					sched.mu.Unlock()
				}
			}
		}
	}
}
