// Layer: host-agent — internal HTTP API server (Phase 2 multi-host wiring).
// Exposes the internal agent API endpoints consumed by the orchestrator.
// All routes are under /internal/v1/ and protected by the X-Agent-Secret header.
//
// This replaces the no-op gRPC registration from proto_types.go for the
// actual RPC path while keeping the gRPC server registration stub in place
// so protoc-generated stubs can be dropped in as a pure replacement later.
//
// Build: Linux only — same constraint as the rest of the host-agent.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"go.uber.org/zap"

	"github.com/sandock/sandock/internal/agentapi"
)

// internalAPIServer wraps the hostAgentServer to expose it over HTTP/JSON.
type internalAPIServer struct {
	agent       *hostAgentServer
	agentSecret string
	log         *zap.Logger
}

// registerInternalRoutes mounts all internal API routes onto mux.
func (s *internalAPIServer) registerInternalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/internal/v1/place",     s.handlePlace)
	mux.HandleFunc("/internal/v1/terminate", s.handleTerminate)
	mux.HandleFunc("/internal/v1/exec",      s.handleExec)
	mux.HandleFunc("/internal/v1/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/internal/healthz",      s.handleHealthz)
}

// authenticate checks the X-Agent-Secret header.
func (s *internalAPIServer) authenticate(w http.ResponseWriter, r *http.Request) bool {
	if s.agentSecret == "" {
		return true // secret not configured — allow all (dev mode)
	}
	if r.Header.Get("X-Agent-Secret") != s.agentSecret {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *internalAPIServer) handlePlace(w http.ResponseWriter, r *http.Request) {
	if !s.authenticate(w, r) { return }
	var req agentapi.PlaceRequest
	if err := decode(r.Body, &req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Convert agentapi.PlaceRequest → local PlaceRequest (same fields).
	localReq := &PlaceRequest{
		SandboxId:       req.SandboxID,
		TenantId:        req.TenantID,
		ImageRef:        req.ImageRef,
		CpuMillis:       req.CPUMillis,
		MemMib:          req.MemMiB,
		TimeoutMs:       req.TimeoutMs,
		EgressAllowlist: req.EgressAllowlist,
		SnapshotKey:     req.SnapshotKey,
	}

	resp, err := s.agent.PlaceSandbox(r.Context(), localReq)
	if err != nil {
		s.log.Error("PlaceSandbox failed", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Look up the vm-agent Unix socket path so callers in dev mode can exec.
	s.agent.mu.RLock()
	entry := s.agent.sandboxes[resp.SandboxId]
	s.agent.mu.RUnlock()

	apiResp := agentapi.PlaceResponse{
		SandboxID: resp.SandboxId,
		VsockCID:  resp.VsockCid,
	}
	if entry != nil && entry.vmAgentSocket != "" {
		apiResp.UnixSocket = entry.vmAgentSocket
	}

	jsonOK(w, apiResp)
}

func (s *internalAPIServer) handleTerminate(w http.ResponseWriter, r *http.Request) {
	if !s.authenticate(w, r) { return }
	var req agentapi.TerminateRequest
	if err := decode(r.Body, &req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := s.agent.TerminateSandbox(r.Context(), &TerminateRequest{
		SandboxId: req.SandboxID,
		Reason:    req.Reason,
	})
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, agentapi.TerminateResponse{SandboxID: resp.SandboxId})
}

func (s *internalAPIServer) handleExec(w http.ResponseWriter, r *http.Request) {
	if !s.authenticate(w, r) { return }
	var req agentapi.ExecRequest
	if err := decode(r.Body, &req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := s.agent.ExecInSandbox(r.Context(), &ExecRequest{
		SandboxId: req.SandboxID,
		Command:   req.Command,
		Stdin:     req.Stdin,
		TimeoutMs: req.TimeoutMs,
	})
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, agentapi.ExecResponse{
		Stdout:     resp.Stdout,
		Stderr:     resp.Stderr,
		ExitCode:   resp.ExitCode,
		DurationMs: resp.DurationMs,
	})
}

func (s *internalAPIServer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !s.authenticate(w, r) { return }
	var req agentapi.HeartbeatRequest
	if err := decode(r.Body, &req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := s.agent.Heartbeat(r.Context(), &HeartbeatRequest{HostId: req.HostID})
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, agentapi.HeartbeatResponse{
		HostID:       resp.HostId,
		ActiveVMs:    resp.ActiveVms,
		IdlePoolSize: resp.IdlePoolSize,
		CPUUsagePct:  resp.CpuUsagePct,
		MemUsedMiB:   resp.MemUsedMib,
		MemTotalMiB:  resp.MemTotalMib,
		Healthy:      true,
	})
}

func (s *internalAPIServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintln(w, `{"status":"ok"}`)
}

// ---------- helpers ----------

func decode(body io.Reader, dst any) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
