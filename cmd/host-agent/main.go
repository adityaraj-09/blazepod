// Layer: host-agent — main entry point.
// Build: Linux-only. Firecracker, KVM, overlayfs, and cgroup v2 require Linux.
// Starts three servers:
//   1. Internal HTTP API (:9091) — used by the orchestrator for PlaceSandbox, Exec, Heartbeat, etc.
//   2. Prometheus metrics HTTP (:9100) — scraped by the monitoring stack.
//   3. gRPC stub (:9090) — placeholder; replaced by real registration once protoc stubs are generated.
//
// Usage:
//   host-agent --config /etc/sandock/config.yaml
package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/sandock/sandock/internal/config"
	"github.com/sandock/sandock/internal/log"
)

var configPath = flag.String("config", "/etc/sandock/config.yaml", "path to config YAML")

func main() {
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		os.Stderr.WriteString("host-agent: load config: " + err.Error() + "\n")
		os.Exit(1)
	}

	log.MustInit(cfg.Log.Level, cfg.Log.Format)
	logger := log.L.With(zap.String("service", "host-agent"))

	agentServer := newHostAgentServer(&cfg.HostAgent, logger)

	// 1. Start the internal HTTP API server (primary RPC path with orchestrator).
	internalAPI := &internalAPIServer{
		agent:       agentServer,
		agentSecret: cfg.HostAgent.AgentSecret,
		log:         logger,
	}
	internalMux := http.NewServeMux()
	internalAPI.registerInternalRoutes(internalMux)

	go func() {
		logger.Info("internal API listening", zap.String("addr", cfg.HostAgent.GRPCAddr))
		if err := http.ListenAndServe(cfg.HostAgent.GRPCAddr, internalMux); err != nil {
			logger.Fatal("internal API server error", zap.Error(err))
		}
	}()

	// 2. Start Prometheus metrics endpoint.
	go func() {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())
		logger.Info("metrics listening", zap.String("addr", cfg.HostAgent.MetricsAddr))
		if err := http.ListenAndServe(cfg.HostAgent.MetricsAddr, metricsMux); err != nil {
			logger.Error("metrics server error", zap.Error(err))
		}
	}()

	// 3. gRPC stub server (no-op registration until protoc stubs are generated).
	grpcAddr := cfg.HostAgent.GRPCAddr
	if grpcAddr == cfg.HostAgent.GRPCAddr {
		// Use a different port for gRPC to avoid binding conflict with the HTTP API.
		grpcAddr = cfg.HostAgent.GRPCStubAddr
	}
	if grpcAddr != "" {
		lis, err := net.Listen("tcp", grpcAddr)
		if err == nil {
			grpcServer := grpc.NewServer()
			RegisterHostAgentServer(grpcServer, agentServer)
			go func() {
				logger.Info("gRPC stub listening", zap.String("addr", grpcAddr))
				_ = grpcServer.Serve(lis)
			}()
		}
	}

	// Start the pool manager reconcile loop.
	ctx, cancel := context.WithCancel(context.Background())
	go agentServer.runPoolReconciler(ctx)

	// Graceful shutdown on SIGTERM / SIGINT.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	logger.Info("host-agent shutting down")
	cancel()
}
