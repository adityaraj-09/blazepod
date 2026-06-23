// Layer: API gateway — main entry point.
// Starts the HTTP REST/WebSocket API server.
// Phase 2: in-process scheduler + real host-agent HTTP calls for exec/terminate.
// Phase 3: replace in-process scheduler with external orchestrator gRPC client.
//
// Usage:
//   api --config /etc/sandock/config.yaml
package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/sandock/sandock/internal/config"
	"github.com/sandock/sandock/internal/log"
	"github.com/sandock/sandock/internal/quota"
	"github.com/sandock/sandock/internal/state"
	"github.com/sandock/sandock/internal/tracing"
)

var configPath = flag.String("config", "/etc/sandock/config.yaml", "path to config YAML")

func main() {
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		os.Stderr.WriteString("api: load config: " + err.Error() + "\n")
		os.Exit(1)
	}

	log.MustInit(cfg.Log.Level, cfg.Log.Format)
	logger := log.L.With(zap.String("service", "api"))

	// Phase 3: init OTel tracing.
	if cfg.Tracing.Endpoint != "" {
		if err := tracing.Init("sandock-api", cfg.Tracing.Endpoint); err != nil {
			logger.Warn("tracing init failed", zap.Error(err))
		}
	}

	store := state.NewStore()
	sched := NewScheduler(logger, store)

	// Register all configured host agents.
	for _, ha := range cfg.HostAgents {
		sched.RegisterHost(&HostInfo{
			ID:       ha.ID,
			Addr:     ha.HTTPAddr,
			Healthy:  true,
			LastSeen: time.Now(),
		})
	}
	// Fallback: register the single local host-agent from host_agent.grpc_addr.
	if len(cfg.HostAgents) == 0 {
		sched.RegisterHost(&HostInfo{
			ID:       "host-local",
			Addr:     cfg.HostAgent.GRPCAddr,
			Healthy:  true,
			LastSeen: time.Now(),
		})
	}

	orchCtx, orchCancel := context.WithCancel(context.Background())

	placer := &grpcPlacer{
		store:       store,
		hostAddr:    cfg.HostAgent.GRPCAddr,
		agentSecret: cfg.HostAgent.AgentSecret,
		log:         logger,
	}
	go sched.RunDispatchLoop(orchCtx, placer)

	quotaStore := quota.NewMemoryStore()

	apiSrv := &apiServer{
		log:               logger,
		store:             store,
		scheduler:         &inProcessOrchestrator{store: store, scheduler: sched, agentSecret: cfg.HostAgent.AgentSecret},
		quota:             quotaStore,
		defaultQuotaLimit: 20,
		jwtSecret:         cfg.API.JWTSecret,
	}

	mux := http.NewServeMux()
	apiSrv.registerRoutes(mux)

	httpSrv := &http.Server{
		Addr:         cfg.API.ListenAddr,
		Handler:      loggingMiddleware(logger, mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	logger.Info("API server starting", zap.String("addr", cfg.API.ListenAddr))

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("HTTP server error", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	logger.Info("API server shutting down")
	orchCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown error", zap.Error(err))
	}
}

// loggingMiddleware logs each request method, path, and duration.
func loggingMiddleware(logger *zap.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(lrw, r)
		logger.Info("request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", lrw.statusCode),
			zap.Duration("duration", time.Since(start)),
		)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}
