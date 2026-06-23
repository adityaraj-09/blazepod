// Layer: API gateway — HTTP handler implementations.
// Each handler validates the request, calls the orchestrator service,
// and writes a JSON response. This is the only layer users interact with.
//
// Routes:
//   POST   /v1/sandboxes           → create a sandbox
//   GET    /v1/sandboxes/:id       → inspect sandbox state
//   DELETE /v1/sandboxes/:id       → kill a sandbox
//   POST   /v1/sandboxes/:id/exec  → run a command inside a sandbox
//   GET    /v1/sandboxes           → list sandboxes for authenticated tenant
//
// Auth: Bearer token in Authorization header. Phase 1: static shared secret.
// Phase 2: JWT validation with HMAC-SHA256.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/sandock/sandock/internal/agentapi"
	"github.com/sandock/sandock/internal/id"
	"github.com/sandock/sandock/internal/quota"
	"github.com/sandock/sandock/internal/spec"
	"github.com/sandock/sandock/internal/state"
	"github.com/sandock/sandock/internal/tracing"
)

// apiServer holds dependencies for all HTTP handlers.
type apiServer struct {
	log        *zap.Logger
	store      *state.Store
	scheduler  OrchestratorClient
	quota      quota.Store
	// defaultQuotaLimit is the max concurrent sandboxes per tenant.
	// Phase 2: read per-tenant limit from a billing store.
	defaultQuotaLimit int
	jwtSecret  string
}

// OrchestratorClient is the interface the API uses to submit sandbox requests.
// Phase 1: in-process. Phase 2: gRPC client to cmd/orchestrator.
type OrchestratorClient interface {
	// Submit queues a sandbox placement and returns immediately with the sandbox ID.
	Submit(sp *spec.SandboxSpec) (sandboxID string, err error)
	// Terminate requests teardown of a running sandbox.
	Terminate(sandboxID, reason string) error
	// Exec runs a command in a sandbox and returns the result.
	Exec(req *spec.ExecRequest) (*spec.ExecResult, error)
}

// handleCreateSandbox handles POST /v1/sandboxes.
// Accepts a SandboxSpec JSON body and returns the new sandbox record.
func (a *apiServer) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracing.Start(r.Context(), "api.create_sandbox")
	defer span.End()
	r = r.WithContext(ctx)

	tenantID, ok := a.authenticate(w, r)
	if !ok {
		return
	}

	var sp spec.SandboxSpec
	if err := json.NewDecoder(r.Body).Decode(&sp); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	sp.TenantID = tenantID

	if err := sp.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Enforce tenant quota before admitting the request.
	limit := a.defaultQuotaLimit
	if limit <= 0 {
		limit = 20 // fallback default: 20 concurrent sandboxes per tenant
	}
	if err := a.quota.Acquire(tenantID, limit); err != nil {
		writeError(w, http.StatusTooManyRequests, "quota exceeded: "+err.Error())
		return
	}

	sandboxID, err := a.scheduler.Submit(&sp)
	if err != nil {
		a.log.Error("submit sandbox failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "could not create sandbox")
		return
	}

	rec, _ := a.store.Get(sandboxID)
	writeJSON(w, http.StatusCreated, rec)
}

// handleGetSandbox handles GET /v1/sandboxes/{id}.
func (a *apiServer) handleGetSandbox(w http.ResponseWriter, r *http.Request) {
	_, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	sandboxID := pathID(r)
	rec, err := a.store.Get(sandboxID)
	if err != nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handleDeleteSandbox handles DELETE /v1/sandboxes/{id}.
func (a *apiServer) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	sandboxID := pathID(r)

	rec, err := a.store.Get(sandboxID)
	if err != nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if rec.TenantID != tenantID {
		writeError(w, http.StatusForbidden, "not your sandbox")
		return
	}

	if err := a.scheduler.Terminate(sandboxID, "client requested kill"); err != nil {
		a.log.Error("terminate sandbox failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "could not kill sandbox")
		return
	}
	// Release the tenant quota slot now that the sandbox is being torn down.
	a.quota.Release(rec.TenantID)
	w.WriteHeader(http.StatusNoContent)
}

// handleExec handles POST /v1/sandboxes/{id}/exec.
// Body: { "command": "...", "stdin": "...", "timeout_ms": 30000 }
func (a *apiServer) handleExec(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracing.Start(r.Context(), "api.exec")
	defer span.End()
	r = r.WithContext(ctx)

	tenantID, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	sandboxID := pathID(r)

	rec, err := a.store.Get(sandboxID)
	if err != nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if rec.TenantID != tenantID {
		writeError(w, http.StatusForbidden, "not your sandbox")
		return
	}
	if rec.State != state.StateRunning {
		writeError(w, http.StatusConflict, "sandbox is not in running state: "+string(rec.State))
		return
	}

	var execReq spec.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&execReq); err != nil {
		writeError(w, http.StatusBadRequest, "invalid exec body: "+err.Error())
		return
	}
	execReq.SandboxID = sandboxID
	if execReq.TimeoutMs == 0 {
		execReq.TimeoutMs = 30_000
	}

	result, err := a.scheduler.Exec(&execReq)
	if err != nil {
		a.log.Error("exec failed", zap.String("sandbox_id", sandboxID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "exec error: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleListSandboxes handles GET /v1/sandboxes.
func (a *apiServer) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	records := a.store.List(tenantID)
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": records, "count": len(records)})
}

// handleHealth handles GET /healthz — used by load balancers and readiness probes.
func (a *apiServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "ts": time.Now().Unix()})
}

// ---------- Auth helpers ----------

// authenticate extracts and validates the Bearer token from the Authorization header.
// Phase 1: compares against a static shared secret stored in config.
// Phase 2: HMAC-SHA256 JWT validation with tenant claims.
// Returns (tenantID, true) on success or writes an error and returns ("", false).
func (a *apiServer) authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeError(w, http.StatusUnauthorized, "missing or invalid Authorization header")
		return "", false
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	if token != a.jwtSecret {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return "", false
	}
	// Phase 1: single static tenant derived from token. Phase 2: decode JWT claims.
	return "ten-" + token[:8], true
}

// ---------- Routing ----------

// registerRoutes wires all handler functions onto the provided ServeMux.
// Uses stdlib routing for simplicity. Phase 2 can swap in chi or gorilla/mux.
func (a *apiServer) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.HandleFunc("GET /v1/sandboxes", a.handleListSandboxes)
	mux.HandleFunc("POST /v1/sandboxes", a.handleCreateSandbox)
	// Routes with path parameters use a wrapper that parses the ID.
	mux.HandleFunc("GET /v1/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exec") && r.Method == http.MethodPost {
			a.handleExec(w, r)
			return
		}
		a.handleGetSandbox(w, r)
	})
	mux.HandleFunc("DELETE /v1/sandboxes/", a.handleDeleteSandbox)
	mux.HandleFunc("POST /v1/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exec") {
			a.handleExec(w, r)
			return
		}
		http.NotFound(w, r)
	})
}

// ---------- Small utilities ----------

// pathID extracts the sandbox ID from a path like /v1/sandboxes/sb-abc123 or /v1/sandboxes/sb-abc123/exec.
func pathID(r *http.Request) string {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), "/")
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// inProcessOrchestrator connects the API directly to the in-process scheduler.
// Phase 2: the Exec method is now wired to the real host-agent via agentapi.Client.
type inProcessOrchestrator struct {
	store       *state.Store
	scheduler   *Scheduler
	agentSecret string
}

func (o *inProcessOrchestrator) Submit(sp *spec.SandboxSpec) (string, error) {
	sandboxID := id.NewSandbox()
	rec := &state.SandboxRecord{
		ID:       sandboxID,
		TenantID: sp.TenantID,
	}
	if err := o.store.Create(rec); err != nil {
		return "", err
	}
	o.scheduler.Enqueue(sandboxID, sp, sp.TimeoutMs)
	return sandboxID, nil
}

func (o *inProcessOrchestrator) Terminate(sandboxID, reason string) error {
	rec, err := o.store.Get(sandboxID)
	if err != nil {
		return err
	}
	if state.IsTerminal(rec.State) {
		return nil // Already done.
	}
	// Best-effort tell the host-agent to kill the VM.
	if rec.HostID != "" {
		o.scheduler.mu.RLock()
		host, hostOK := o.scheduler.hosts[rec.HostID]
		o.scheduler.mu.RUnlock()
		if hostOK {
			client := agentapiClient(host.Addr, o.agentSecret)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = client.TerminateSandbox(ctx, &agentapi.TerminateRequest{
				SandboxID: sandboxID,
				Reason:    reason,
			})
			cancel()
		}
	}
	return o.store.Transition(sandboxID, rec.State, state.StateDraining, reason)
}

func (o *inProcessOrchestrator) Exec(req *spec.ExecRequest) (*spec.ExecResult, error) {
	rec, err := o.store.Get(req.SandboxID)
	if err != nil {
		return nil, err
	}
	if rec.State != state.StateRunning {
		return nil, fmt.Errorf("sandbox %s is not running (state=%s)", req.SandboxID, rec.State)
	}
	if rec.HostID == "" {
		return nil, fmt.Errorf("sandbox %s has no assigned host", req.SandboxID)
	}

	o.scheduler.mu.RLock()
	host, hostOK := o.scheduler.hosts[rec.HostID]
	o.scheduler.mu.RUnlock()
	if !hostOK {
		return nil, fmt.Errorf("host %s not found in registry", rec.HostID)
	}

	timeoutMs := req.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = 30_000
	}

	client := agentapiClient(host.Addr, o.agentSecret)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond+5*time.Second)
	defer cancel()

	resp, err := client.ExecInSandbox(ctx, &agentapi.ExecRequest{
		SandboxID: req.SandboxID,
		Command:   req.Command,
		Stdin:     req.Stdin,
		TimeoutMs: timeoutMs,
	})
	if err != nil {
		return nil, fmt.Errorf("exec in sandbox: %w", err)
	}

	return &spec.ExecResult{
		Stdout:     resp.Stdout,
		Stderr:     resp.Stderr,
		ExitCode:   int(resp.ExitCode),
		DurationMs: resp.DurationMs,
	}, nil
}

// agentapiClient is a helper that constructs an agentapi.Client with the right base URL.
func agentapiClient(hostAddr, secret string) *agentapi.Client {
	return agentapi.NewClient("http://"+hostAddr, secret)
}
