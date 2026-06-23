# Phase 1 — Flow Explanation

## What Is Implemented

Phase 1 is a **single-host proof of concept** where the API, orchestrator, and host-agent
run in one process and share in-memory state. No real gRPC network hop exists yet between
the API and host-agent; placement is simulated. All logic is fully wired for the sandbox
lifecycle state machine, admission, scheduling, and exec.

---

## Request Flow: Create Sandbox

```
Client (TypeScript SDK / curl)
  POST /v1/sandboxes  { image, cpu_millis, memory_mib, timeout_ms }
        │
        ▼
cmd/api/handlers.go  handleCreateSandbox()
  ├── authenticate() — validates Bearer token against cfg.API.JWTSecret
  ├── json.Decode() → spec.SandboxSpec
  ├── spec.Validate() — cpu/memory/timeout range checks
  └── scheduler.Submit(spec)
        │
        ▼
cmd/api/scheduler.go  inProcessOrchestrator.Submit()
  ├── id.NewSandbox() — generates "sb-<12 hex chars>"
  ├── state.Store.Create(record)  — state: queued
  └── Scheduler.Enqueue(sandboxID, spec, timeoutMs)
        │  (pushes to min-heap priority queue)
        ▼
cmd/api/scheduler.go  Scheduler.RunDispatchLoop()  [goroutine]
  ├── heap.Pop() → pendingItem
  ├── deadline check — fail if expired
  ├── Scheduler.bestHost()
  │     score = 0.4×cpu_fit + 0.35×warm_pool_hit + 0.25×host_health
  ├── state.Store.SetHostID(sandboxID, hostID)
  ├── state.Store.Transition(queued → provisioning)
  └── grpcPlacer.PlaceSandbox()  [Phase 1: stub, returns vsockCID=3]
        └── state.Store.Transition(provisioning → running)

Response: 201 Created  { id, state: "queued", ... }
```

**Files involved:**
| File | Responsibility |
|---|---|
| `cmd/api/handlers.go` | HTTP routing, auth, JSON encode/decode |
| `cmd/api/scheduler.go` | Priority queue, host scoring, dispatch loop |
| `internal/spec/spec.go` | `SandboxSpec` validation |
| `internal/state/state.go` | State machine + in-memory store |
| `internal/id/id.go` | Sandbox ID generation |
| `internal/config/config.go` | Runtime config loaded at startup |

---

## Request Flow: Exec Command

```
Client
  POST /v1/sandboxes/sb-abc123/exec  { command: "echo hello" }
        │
        ▼
cmd/api/handlers.go  handleExec()
  ├── authenticate()
  ├── state.Store.Get(sandboxID) — must be state=running
  ├── TenantID ownership check
  └── inProcessOrchestrator.Exec(execReq)
        │
        ▼
cmd/api/handlers.go  inProcessOrchestrator.Exec()
  Phase 1: returns stub response
  Phase 2: calls host-agent gRPC ExecInSandbox →
    cmd/host-agent/server.go  hostAgentServer.ExecInSandbox()
      └── vsock.Exec(entry.vsockCID, ExecRequest)
            │
            ▼  (AF_VSOCK connection to CID assigned by Firecracker)
          vm-agent/src/main.rs  handle_connection()
            ├── read_line() — JSON ExecRequest
            ├── Command::new("/bin/sh").arg("-c").arg(command).spawn()
            ├── wait_with_output()
            └── serde_json::to_string(ExecResponse) → write to conn

Response: 200 OK  { stdout, stderr, exit_code, duration_ms }
```

**Files involved:**
| File | Responsibility |
|---|---|
| `cmd/api/handlers.go` | Exec handler, tenant check, state guard |
| `cmd/host-agent/server.go` | gRPC ExecInSandbox, vsock forwarding |
| `internal/vsock/vsock.go` | vsock Exec() caller |
| `internal/vsock/vsock_linux.go` | AF_VSOCK dial (Linux only) |
| `internal/vsock/vsock_stub.go` | Error stub for macOS dev |
| `vm-agent/src/main.rs` | In-VM command execution |

---

## Request Flow: Kill Sandbox

```
Client
  DELETE /v1/sandboxes/sb-abc123
        │
        ▼
cmd/api/handlers.go  handleDeleteSandbox()
  ├── authenticate()
  ├── state.Store.Get() — tenant ownership check
  └── inProcessOrchestrator.Terminate(sandboxID, "client requested kill")
        │
        ▼
cmd/api/handlers.go  inProcessOrchestrator.Terminate()
  └── state.Store.Transition(running → draining, reason)

  Phase 2: sends TerminateSandbox gRPC to host-agent →
    cmd/host-agent/server.go  hostAgentServer.TerminateSandbox()
      ├── entry.cancel() — cancels the VM context
      └── watchVM goroutine runs:
            ├── cmd.Wait()
            ├── cgroup.Teardown(sandboxID)    internal/cgroup/cgroup.go
            └── overlay.Teardown(overlayCfg)  internal/overlay/overlay.go
                  └── verifyEmptyUpper() — security gate before slot reuse

Response: 204 No Content
```

---

## Phase 1 Startup Sequence

```
main() in cmd/api/main.go
  1. config.Load(*configPath)                    internal/config/config.go
  2. log.MustInit(level, format)                 internal/log/log.go
  3. state.NewStore()                            internal/state/state.go
  4. NewScheduler(logger, store)                 cmd/api/scheduler.go
  5. sched.RegisterHost(local host entry)
  6. go sched.RunDispatchLoop(ctx, placer)       — goroutine
  7. apiServer{store, scheduler, jwtSecret}      cmd/api/handlers.go
  8. httpSrv.ListenAndServe(cfg.API.ListenAddr)
```

---

## What Is Stubbed in Phase 1

| Component | Phase 1 Status | Phase 2 Wiring |
|---|---|---|
| Firecracker VM boot | Simulated (grpcPlacer returns vsockCID=3) | Real: `fc.StartInstance()` via Firecracker API |
| overlayfs mount | Real on Linux; stub on macOS | Already wired in host-agent |
| cgroup v2 | Real on Linux; runtime check | Already wired in host-agent |
| vsock exec | Returns stub response | Real: AF_VSOCK → vm-agent |
| Multi-host gRPC | Not started | Implement orchestrator gRPC server |
| VM pool | PoolManager math ready | Wire to host-agent boot loop |
| eBPF egress | C program exists; loader is stub | Wire cilium/ebpf loader |
| Tenant quotas | MemoryStore ready | Swap for Redis |
| Metrics | Registered; not emitted | Wire to lifecycle events |
| Snapshots | Snapshot package exists | Wire to Firecracker snapshot API |
