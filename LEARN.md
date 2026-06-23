# Sandock — Complete Beginner's Guide to the Codebase

This document teaches you **the whole Sandock codebase** from scratch: what it does, how requests move through the system, and what every important function means.

Read this top-to-bottom once, then use it as a reference while you read the actual source files.

---

## Table of Contents

1. [What is Sandock?](#1-what-is-sandock)
2. [Mental model (beginner analogy)](#2-mental-model-beginner-analogy)
3. [Architecture — the 7 layers](#3-architecture--the-7-layers)
4. [Key concepts you must understand first](#4-key-concepts-you-must-understand-first)
5. [How the repo is organized](#5-how-the-repo-is-organized)
6. [Sandbox lifecycle (state machine)](#6-sandbox-lifecycle-state-machine)
7. [What happens when the system starts](#7-what-happens-when-the-system-starts)
8. [Flow A — Create a sandbox (step by step)](#8-flow-a--create-a-sandbox-step-by-step)
9. [Flow B — Run a command inside a sandbox (exec)](#9-flow-b--run-a-command-inside-a-sandbox-exec)
10. [Flow C — Kill a sandbox](#10-flow-c--kill-a-sandbox)
11. [Flow D — Get / List sandboxes](#11-flow-d--get--list-sandboxes)
12. [Flow E — Host heartbeat and health monitoring](#12-flow-e--host-heartbeat-and-health-monitoring)
13. [Package-by-package function reference](#13-package-by-package-function-reference)
14. [Security — defense in depth](#14-security--defense-in-depth)
15. [How to trace a request yourself](#15-how-to-trace-a-request-yourself)
16. [Glossary](#16-glossary)

---

## 1. What is Sandock?

**Sandock** is a **sandbox provider**. It lets users run untrusted code (Python scripts, shell commands, etc.) inside **tiny virtual machines** called **microVMs**, instead of running that code directly on your server.

Why microVMs?

- A normal Docker container shares the host kernel. A bug in isolation can leak data between tenants.
- A **Firecracker microVM** uses **hardware virtualization (KVM)**. Each sandbox gets its own mini-computer with its own kernel. That is much stronger isolation.

Sandock's job:

1. Accept a request: *"Run this image with 256 MB RAM for 30 seconds."*
2. Boot a microVM on a Linux host.
3. Let the user run commands inside it.
4. Tear everything down safely when done.

---

## 2. Mental model (beginner analogy)

Think of Sandock like a **hotel for code**:

| Real world | Sandock |
|---|---|
| Guest (you) | Tenant (API user with an API key) |
| Hotel front desk | `cmd/api` — takes your request, checks ID |
| Hotel manager deciding which building | `cmd/orchestrator` / Scheduler — picks a host machine |
| Building superintendent | `cmd/host-agent` — boots VMs, manages rooms |
| Individual hotel room | One Firecracker microVM (one sandbox) |
| Room phone to call the front desk | `vsock` — host talks to code inside the VM |
| Person inside the room who runs errands | `vm-agent` (Rust) — runs your shell commands |
| Room lock + power limit | `cgroup` — CPU/memory limits |
| Room's writable notepad (wiped after checkout) | `overlayfs` upper layer |
| Security guard at the exit door | `eBPF egress filter` — blocks network unless allowed |

When you `POST /v1/sandboxes`, you are checking into a room.  
When you `POST /v1/sandboxes/:id/exec`, you are asking the in-room assistant to run a command.  
When you `DELETE /v1/sandboxes/:id`, you are checking out and the room gets cleaned.

---

## 3. Architecture — the 7 layers

```
┌─────────────────────────────────────────────────────────────┐
│  SDK (TypeScript / Go)                                      │
│  sdk/typescript, sdk/go/sandock                             │
│  User-facing client libraries                               │
└──────────────────────────┬──────────────────────────────────┘
                           │ HTTPS REST
                           ▼
┌─────────────────────────────────────────────────────────────┐
│  API Gateway — cmd/api                                      │
│  Auth, validation, quota, HTTP handlers                     │
└──────────────────────────┬──────────────────────────────────┘
                           │ in-process (today) or remote later
                           ▼
┌─────────────────────────────────────────────────────────────┐
│  Orchestrator / Scheduler — cmd/api/scheduler.go            │
│             or cmd/orchestrator                             │
│  Priority queue, host scoring, placement decisions          │
└──────────────────────────┬──────────────────────────────────┘
                           │ HTTP/JSON (internal/agentapi)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│  Host Agent — cmd/host-agent                                │
│  Firecracker, overlayfs, cgroups, network, eBPF             │
└──────────────────────────┬──────────────────────────────────┘
                           │ Unix socket HTTP API
                           ▼
┌─────────────────────────────────────────────────────────────┐
│  Firecracker VMM (external binary)                          │
│  internal/firecracker — Go client for its API               │
└──────────────────────────┬──────────────────────────────────┘
                           │ AF_VSOCK (virtual socket)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│  VM Agent — vm-agent (Rust)                                 │
│  Runs inside the guest VM, executes shell commands          │
└─────────────────────────────────────────────────────────────┘
```

**Data always flows downward** for provisioning (create) and **sideways/back up** for exec results.

---

## 4. Key concepts you must understand first

### Sandbox
One running (or queued) microVM instance. Has an ID like `sb-a1b2c3d4e5f6`.

### Tenant
The customer who owns sandboxes. Identified by API token. Two tenants must never see each other's data.

### Firecracker
AWS's microVM hypervisor. Sandock starts one `firecracker` OS process per sandbox. It exposes a **Unix socket HTTP API** to configure and boot the VM.

### KVM
Linux kernel feature for hardware virtualization. Firecracker needs `/dev/kvm`.

### overlayfs
A Linux filesystem that stacks a **read-only base image** (lower) under a **per-sandbox writable layer** (upper). When the sandbox dies, the upper layer is wiped — so the next tenant gets a clean disk.

### cgroup v2
Linux kernel feature to limit CPU and memory per process group. Sandock puts each Firecracker process in its own cgroup so one sandbox cannot eat all host RAM.

### vsock (AF_VSOCK)
A socket type for **host ↔ VM** communication without going through the virtual network card. Lower latency and simpler than TCP for exec.

### eBPF
Programs that run **inside the Linux kernel**. Sandock attaches an eBPF program to each sandbox's virtual network interface to **drop outbound traffic** unless the destination IP is in an allowlist.

### Snapshot
A saved copy of a running VM's memory and device state. Restoring a snapshot is a **warm start** (~30 ms) vs booting the kernel from scratch (~125 ms).

### Quota
Per-tenant limit on how many sandboxes can run at the same time (e.g. max 20).

---

## 5. How the repo is organized

```
sandock/
├── cmd/                    # Runnable programs (main packages)
│   ├── api/                # Public REST API server
│   ├── orchestrator/       # Standalone scheduler (multi-host mode)
│   └── host-agent/         # Per-machine VM manager (Linux only)
│
├── internal/               # Shared Go libraries (not importable externally)
│   ├── agentapi/           # HTTP client/types for orchestrator ↔ host-agent
│   ├── config/             # YAML config loading
│   ├── state/              # Sandbox state machine + in-memory DB
│   ├── spec/               # Request types (SandboxSpec, ExecRequest)
│   ├── id/                 # ID generators
│   ├── log/                # Structured logging (zap)
│   ├── firecracker/        # Firecracker Unix-socket HTTP client
│   ├── cgroup/             # cgroup v2 setup
│   ├── overlay/            # overlayfs mount/teardown
│   ├── vsock/              # Talk to vm-agent inside VM
│   ├── snapshot/           # Firecracker snapshot save/load
│   ├── quota/              # Per-tenant concurrency limits
│   ├── metrics/            # Prometheus metric definitions
│   ├── network/            # veth + network namespace (Linux)
│   ├── pool/               # Warm VM pool math (EWMA)
│   ├── registry/           # OCI image digest verification
│   ├── persistence/        # tar.zst filesystem archives
│   ├── fencing/            # STONITH host power-off
│   ├── tracing/            # OpenTelemetry spans
│   └── wireguard/          # Per-tenant VPN tunnels
│
├── vm-agent/               # Rust binary that runs INSIDE each VM
├── ebpf/
│   ├── programs/           # C source for kernel egress filter
│   └── loader/             # Go code to load eBPF onto veth
├── sdk/
│   ├── typescript/         # npm package @sandock/sdk
│   └── go/sandock/         # Go client library
├── proto/                  # gRPC/protobuf definitions (future)
└── deploy/                 # Config example + Linux setup script
```

**Rule of thumb:**
- `cmd/` = programs you **run**
- `internal/` = libraries those programs **use**
- `vm-agent/` and `ebpf/` = special languages for hot paths

---

## 6. Sandbox lifecycle (state machine)

Every sandbox moves through **states**. Illegal jumps are rejected.

```
  queued ──► provisioning ──► running ──► draining ──► terminated
     │              │            │
     └──────────────┴────────────┴──► failed (terminal)
```

| State | Meaning |
|---|---|
| `queued` | Request accepted, waiting for a free host |
| `provisioning` | Host picked, VM is being created |
| `running` | VM is live, exec works |
| `draining` | Kill requested, teardown in progress |
| `terminated` | Done, resources freed |
| `failed` | Something went wrong (timeout, boot error, etc.) |

**File:** `internal/state/state.go`

| Function | What it does |
|---|---|
| `Transition(from, to)` | Checks if a state change is legal (pure validation) |
| `IsTerminal(s)` | Returns true for `terminated` or `failed` |
| `NewStore()` | Creates the in-memory database of all sandboxes |
| `Store.Create(r)` | Inserts a new record in `queued` state |
| `Store.Transition(id, from, to, reason)` | Atomically moves a sandbox to a new state |
| `Store.Get(id)` | Returns a copy of one sandbox record |
| `Store.List(tenantID)` | Returns all sandboxes (optionally filtered by tenant) |
| `Store.SetHostID(id, hostID)` | Records which physical host is running this VM |
| `Store.SetVsockCID(id, cid, socket)` | Stores how to reach the vm-agent for exec |

---

## 7. What happens when the system starts

### Starting the API server (`cmd/api/main.go`)

When you run `./bin/api --config /etc/sandock/config.yaml`:

| Step | Function | What happens |
|---|---|---|
| 1 | `config.Load(path)` | Reads YAML: listen address, secrets, host-agent address |
| 2 | `log.MustInit(level, format)` | Sets up structured JSON logging |
| 3 | `tracing.Init(...)` | Optionally connects to OpenTelemetry collector |
| 4 | `state.NewStore()` | Creates empty in-memory sandbox database |
| 5 | `NewScheduler(logger, store)` | Creates scheduler with empty host list and priority queue |
| 6 | `sched.RegisterHost(&HostInfo{...})` | Registers one or more host-agent machines |
| 7 | `go sched.RunDispatchLoop(ctx, placer)` | **Background goroutine** that continuously places queued sandboxes |
| 8 | `quota.NewMemoryStore()` | Creates per-tenant concurrency counter |
| 9 | `apiServer{...}` + `registerRoutes(mux)` | Wires HTTP routes to handler functions |
| 10 | `httpSrv.ListenAndServe(addr)` | Starts accepting HTTP on port 8080 |

### Starting the host-agent (`cmd/host-agent/main.go`)

When you run `sudo ./bin/host-agent` on a Linux KVM machine:

| Step | Function | What happens |
|---|---|---|
| 1 | `config.Load(path)` | Reads Firecracker paths, sandbox dir, secrets |
| 2 | `newHostAgentServer(cfg, log)` | Creates server with empty `sandboxes` map |
| 3 | `internalAPIServer.registerInternalRoutes(mux)` | Exposes `/internal/v1/place`, `/exec`, etc. |
| 4 | `http.ListenAndServe(grpc_addr, mux)` | **Goroutine** — internal API for orchestrator |
| 5 | `promhttp.Handler()` on `/metrics` | **Goroutine** — Prometheus scrape endpoint |
| 6 | `go agentServer.runPoolReconciler(ctx)` | **Goroutine** — tries to keep warm idle VMs ready |

---

## 8. Flow A — Create a sandbox (step by step)

**User action:** `POST /v1/sandboxes` with body:
```json
{
  "image": "base",
  "cpu_millis": 500,
  "memory_mib": 256,
  "timeout_ms": 30000
}
```

### Phase 1 — API receives the request

**File:** `cmd/api/handlers.go`

| # | Function | Significance |
|---|---|---|
| 1 | `handleCreateSandbox(w, r)` | Entry point for create. Owns the whole HTTP handler. |
| 2 | `tracing.Start(ctx, "api.create_sandbox")` | Opens a distributed tracing span (for debugging latency in production) |
| 3 | `authenticate(w, r)` | Reads `Authorization: Bearer <token>`. Compares to config secret. Returns `tenantID` like `ten-change-me`. **Security gate #1.** |
| 4 | `json.NewDecoder(r.Body).Decode(&sp)` | Parses JSON into `spec.SandboxSpec` struct |
| 5 | `sp.TenantID = tenantID` | **Forces** tenant from auth — user cannot fake another tenant |
| 6 | `sp.Validate()` | Checks CPU 1–32000, memory 64–32768 MiB, timeout 1–3600000 ms. **Validation gate.** |
| 7 | `quota.Acquire(tenantID, limit)` | Increments tenant's running-sandbox counter. Returns 429 if at limit. **Fairness gate.** |
| 8 | `scheduler.Submit(&sp)` | Hands off to orchestrator logic |
| 9 | `store.Get(sandboxID)` | Fetches the record just created |
| 10 | `writeJSON(w, 201, rec)` | Sends `{"id":"sb-...","state":"queued",...}` to client |

**Why return `queued` immediately?** Booting a VM takes time. The API does **not** block until the VM is running. The client polls `GET /v1/sandboxes/:id` until `state == "running"`.

---

### Phase 2 — Orchestrator admits the sandbox

**File:** `cmd/api/handlers.go` → `inProcessOrchestrator.Submit`

| # | Function | Significance |
|---|---|---|
| 1 | `id.NewSandbox()` | Generates ID: `"sb-"` + 12 random hex chars. **File:** `internal/id/id.go` |
| 2 | `store.Create(rec)` | Saves record with `state = queued` |
| 3 | `scheduler.Enqueue(sandboxID, sp, timeoutMs)` | Pushes into priority queue |

**File:** `cmd/api/scheduler.go` → `Scheduler.Enqueue`

| # | Function | Significance |
|---|---|---|
| 1 | Creates `pendingItem{sandboxID, spec, deadline, priority}` | `deadline` = now + timeout — if host is busy too long, sandbox fails |
| 2 | `heap.Push(&s.queue, item)` | Min-heap: earliest deadline gets dispatched first |
| 3 | `placeCh <- struct{}{}` | Wakes the dispatch loop immediately |

---

### Phase 3 — Dispatch loop places the sandbox on a host

**File:** `cmd/api/scheduler.go` → `RunDispatchLoop` (runs forever in background)

| # | Function | Significance |
|---|---|---|
| 1 | `RunDispatchLoop(ctx, placer)` | Loops every 50ms OR when `placeCh` fires |
| 2 | `dispatchPending(ctx, placer)` | Tries to place one queued item |

Inside `dispatchPending`:

| # | Function | Significance |
|---|---|---|
| 1 | `heap.Pop(&s.queue)` | Takes highest-priority pending sandbox |
| 2 | Check `time.Now().After(item.deadline)` | If too late → `Transition(queued → failed, "deadline exceeded")` |
| 3 | `bestHost()` | Scores all registered hosts, picks best |
| 4 | `scoreHost(h)` | Formula: `0.4×cpu_fit + 0.35×warm_pool + 0.25×health`. Higher = better host |
| 5 | `store.SetHostID(sandboxID, host.ID)` | Records which machine will run this VM |
| 6 | `store.Transition(queued → provisioning)` | State update so client sees progress |
| 7 | `go placer.PlaceSandbox(...)` | **Async goroutine** — actual VM boot (slow) |

---

### Phase 4 — Placer calls the host-agent over HTTP

**File:** `cmd/api/scheduler.go` → `grpcPlacer.PlaceSandbox`

| # | Function | Significance |
|---|---|---|
| 1 | `agentapi.NewClient("http://"+host.Addr, secret)` | HTTP client with `X-Agent-Secret` header |
| 2 | `client.PlaceSandbox(ctx, &PlaceRequest{...})` | POST `/internal/v1/place` with sandbox spec |

**File:** `internal/agentapi/agentapi.go` → `Client.PlaceSandbox`

| # | Function | Significance |
|---|---|---|
| 1 | `post(ctx, "/internal/v1/place", req, &resp)` | Marshals JSON, sends HTTP POST, unmarshals response |

---

### Phase 5 — Host-agent HTTP handler receives the call

**File:** `cmd/host-agent/internal_api.go`

| # | Function | Significance |
|---|---|---|
| 1 | `handlePlace(w, r)` | HTTP handler for place |
| 2 | `authenticate(w, r)` | Checks `X-Agent-Secret` — only orchestrator may call this |
| 3 | `decode(r.Body, &req)` | Parses `agentapi.PlaceRequest` JSON |
| 4 | Maps to local `PlaceRequest` (protobuf-style field names) |
| 5 | `agent.PlaceSandbox(ctx, localReq)` | **The big one** — boots the VM |
| 6 | `writeJSON(w, PlaceResponse{VsockCID, ...})` | Returns handles needed for exec |

---

### Phase 6 — Host-agent boots the microVM (the core)

**File:** `cmd/host-agent/server.go` → `PlaceSandbox`

This is the longest and most important function. Here is **every step in order**:

| Step | Code / Function | What happens & why |
|---|---|---|
| **1** | `overlay.Setup(overlayCfg)` | Creates `upper/`, `work/`, `merged/` dirs and mounts overlayfs. Guest sees a writable root filesystem built on top of the shared base image. **Prevents cross-tenant disk leaks** when upper is wiped later. |
| **2** | `cgroup.Setup(sandboxID, Limits{...})` | Creates `/sys/fs/cgroup/sandboxes/<id>/` and writes `cpu.max`, `memory.max`. **Prevents one sandbox from starving the host.** |
| **3** | `copyFile(baseRootfs, rootfsDisk)` | Copies base ext4 image to per-sandbox path (future: QCOW2 copy-on-write). |
| **4** | `exec.CommandContext(vmCtx, firecrackerBin, "--api-sock", ...)` | Starts the Firecracker OS process. `vmCtx` auto-kills it after `timeout_ms`. |
| **5** | `cgroup.AddPID(sandboxID, fcCmd.Process.Pid)` | Puts Firecracker process inside the sandbox cgroup so limits apply. |
| **6** | `waitForSocket(socketPath, 5s)` | Polls until Firecracker creates its Unix control socket. |
| **7** | `firecracker.NewClient(socketPath)` | Go HTTP client that dials the Unix socket. |
| **8** | `fc.PutMachineConfig(...)` | Tells Firecracker: 1 vCPU, N MiB RAM. |
| **9** | `fc.PutBootSource(...)` | Points VM at `vmlinux` kernel + boot args (`console=ttyS0 ...`). |
| **10** | `fc.PutDrive(...)` | Attaches the per-sandbox `rootfs.ext4` as the boot disk. |
| **11a** | **Cold path:** `fc.StartInstance()` | Boots kernel from scratch. Records `SandboxColdStartDuration` metric. |
| **11b** | **Warm path:** `snapManager.Load(key, fc)` | If `SnapshotKey` provided and snapshot exists, restores saved VM state instead of booting. Records `SandboxWarmStartDuration` metric. |
| **12** | `cid(fcCmd.Process.Pid)` | Assigns a vsock Context ID (placeholder: derived from PID). Used to dial vm-agent. |
| **13** | Store in `sandboxes` map | `sandboxEntry` tracks PID, vsock CID, cgroup path, cancel func. |
| **14** | `egressFilter.Load("/var/sandock/ebpf/egress_filter.o")` | Loads eBPF program onto sandbox veth. **Network security gate.** |
| **15** | `egressFilter.AddCIDR(cidr)` for each allowlist entry | Inserts allowed destination IPs into kernel LPM trie map. |
| **16** | `go watchVM(...)` | Background goroutine: waits for VM exit, then cleans up cgroup + overlay. |
| **17** | Return `PlaceResponse{VsockCid}` | Tells orchestrator the VM is live. |

---

### Phase 7 — State updated to `running`

Back in `dispatchPending`'s goroutine:

| # | Function | Significance |
|---|---|---|
| 1 | `placer.PlaceSandbox` returns vsock CID | Host-agent succeeded |
| 2 | `store.SetVsockCID(sandboxID, cid, unixSocket)` | Saves exec routing info |
| 3 | `store.Transition(provisioning → running)` | Client can now exec |

**Client polls:** `GET /v1/sandboxes/sb-xxx` until `"state": "running"`.

---

## 9. Flow B — Run a command inside a sandbox (exec)

**User action:** `POST /v1/sandboxes/sb-abc123/exec`
```json
{ "command": "python3 -c 'print(42)'", "timeout_ms": 30000 }
```

### Step-by-step

| # | Layer | Function | Significance |
|---|---|---|---|
| 1 | API | `handleExec(w, r)` | Entry point |
| 2 | API | `tracing.Start(ctx, "api.exec")` | Tracing span |
| 3 | API | `authenticate(w, r)` | Auth check |
| 4 | API | `store.Get(sandboxID)` | Load sandbox record |
| 5 | API | `rec.TenantID != tenantID` check | **Ownership check** — you can only exec your own sandbox |
| 6 | API | `rec.State != running` check | Cannot exec a sandbox that is not ready |
| 7 | API | `scheduler.Exec(&execReq)` | Delegates to orchestrator |
| 8 | Orchestrator | `inProcessOrchestrator.Exec(req)` | Looks up which host runs this sandbox |
| 9 | Orchestrator | `scheduler.hosts[rec.HostID]` | Finds host-agent address |
| 10 | Orchestrator | `agentapi.Client.ExecInSandbox(...)` | HTTP POST `/internal/v1/exec` to host-agent |
| 11 | Host-agent HTTP | `internal_api.handleExec` | Authenticates, decodes JSON |
| 12 | Host-agent | `hostAgentServer.ExecInSandbox(req)` | Looks up `sandboxEntry` in local map |
| 13 | Host-agent | `vsock.Exec(entry.vsockCID, ExecRequest{...})` | Dials vm-agent inside VM |

**File:** `internal/vsock/vsock.go` → `Exec`

| # | Function | Significance |
|---|---|---|
| 1 | `dial(cid, ExecPort)` | Opens AF_VSOCK connection to VM (Linux) or errors on macOS |
| 2 | `json.NewEncoder(conn).Encode(req)` | Sends `{"command":"...", "timeout_ms":30000}` |
| 3 | `json.NewDecoder(conn).Decode(&resp)` | Reads `{"stdout":"...", "exit_code":0, ...}` |

### Inside the VM (Rust)

**File:** `vm-agent/src/main.rs`

| # | Function | Significance |
|---|---|---|
| 1 | `main()` | Binds Unix socket (dev) or vsock port 8888 (production) |
| 2 | `listener.incoming()` | Accepts one connection per exec request |
| 3 | `handle_connection(conn)` | Reads one JSON line, runs command, writes response |
| 4 | `read_line(&mut conn)` | Reads bytes until `\n` — the ExecRequest JSON |
| 5 | `serde_json::from_str::<ExecRequest>(&line)` | Parses command + timeout |
| 6 | `Command::new("/bin/sh").arg("-c").arg(&req.command)` | Runs shell command — supports pipes, redirects, etc. |
| 7 | `child.wait_with_output()` | Blocks until process exits, captures stdout/stderr |
| 8 | `serde_json::to_string(&ExecResponse)` | Sends result back to host-agent |

Response travels back up: vm-agent → vsock → host-agent → agentapi HTTP → API → client JSON.

---

## 10. Flow C — Kill a sandbox

**User action:** `DELETE /v1/sandboxes/sb-abc123`

| # | Layer | Function | Significance |
|---|---|---|---|
| 1 | API | `handleDeleteSandbox(w, r)` | Entry point |
| 2 | API | `authenticate` + tenant ownership check | Security |
| 3 | API | `scheduler.Terminate(sandboxID, reason)` | Start teardown |
| 4 | Orchestrator | `inProcessOrchestrator.Terminate` | |
| 5 | Orchestrator | `agentapi.Client.TerminateSandbox(...)` | HTTP POST `/internal/v1/terminate` (best-effort) |
| 6 | Host-agent | `hostAgentServer.TerminateSandbox` | Finds `sandboxEntry`, calls `entry.cancel()` |
| 7 | Host-agent | `entry.cancel()` | Cancels the VM context → Firecracker process gets killed |
| 8 | Orchestrator | `store.Transition(running → draining)` | State reflects teardown |
| 9 | API | `quota.Release(rec.TenantID)` | Frees one quota slot for the tenant |
| 10 | API | `w.WriteHeader(204)` | No content — success |

### Background cleanup (even without explicit kill)

**File:** `cmd/host-agent/server.go` → `watchVM` (goroutine started at place time)

| # | Function | Significance |
|---|---|---|
| 1 | `cmd.Wait()` | Blocks until Firecracker process exits (timeout or kill) |
| 2 | `cgroup.OOMCount(sandboxID)` | Checks if kernel OOM-killed the sandbox; records metric |
| 3 | `delete(s.sandboxes, sandboxID)` | Removes from active map |
| 4 | `cgroup.Teardown(sandboxID)` | Deletes cgroup directory |
| 5 | `overlay.Teardown(overlayCfg)` | Unmounts overlayfs, wipes upper layer (security!) |

---

## 11. Flow D — Get / List sandboxes

### GET `/v1/sandboxes/:id`

| Function | Significance |
|---|---|
| `handleGetSandbox` | Auth → `store.Get(id)` → return JSON record |

### GET `/v1/sandboxes`

| Function | Significance |
|---|---|
| `handleListSandboxes` | Auth → `store.List(tenantID)` → return array filtered to your tenant |

---

## 12. Flow E — Host heartbeat and health monitoring

When running standalone orchestrator (`cmd/orchestrator/main.go`):

| # | Function | Significance |
|---|---|---|
| 1 | `runHeartbeatMonitor(ctx, sched, secret, log)` | Goroutine, ticks every 10 seconds |
| 2 | For each registered host: `agentapi.Client.Heartbeat(...)` | POST `/internal/v1/heartbeat` |
| 3 | Host-agent `Heartbeat()` | Returns `ActiveVMs` count from local map |
| 4 | On success: `scorer.RecordSuccess(hostID)` | Resets miss counter |
| 5 | On failure: `scorer.RecordMiss(hostID)` | Increments miss counter |
| 6 | After 3 misses: `sched.UpdateHostHealth(id, false)` | Host marked unhealthy, won't receive new sandboxes |
| 7 | After 3 misses: `fence.Fence(hostID)` | STONITH — logs (dev) or IPMI power-off (production) |

**File:** `internal/fencing/fencing.go`

| Function | Significance |
|---|---|
| `HealthScorer.RecordMiss` | Tracks consecutive heartbeat failures |
| `LogFencer.Fence` | Dev mode: logs "FENCE host=..." to stderr |
| `IPMIFencer.Fence` | Production: runs `ipmitool chassis power off` |

---

## 13. Package-by-package function reference

### `cmd/api/` — Public API

| File | Function | Purpose |
|---|---|---|
| `main.go` | `main()` | Wires everything, starts HTTP server |
| `main.go` | `loggingMiddleware` | Logs method, path, status, duration per request |
| `handlers.go` | `handleCreateSandbox` | POST create flow |
| `handlers.go` | `handleGetSandbox` | GET one sandbox |
| `handlers.go` | `handleDeleteSandbox` | DELETE kill flow |
| `handlers.go` | `handleExec` | POST exec flow |
| `handlers.go` | `handleListSandboxes` | GET list |
| `handlers.go` | `handleHealth` | GET /healthz for load balancers |
| `handlers.go` | `authenticate` | Bearer token validation |
| `handlers.go` | `registerRoutes` | Maps URL patterns to handlers |
| `handlers.go` | `inProcessOrchestrator.Submit` | Create sandbox in DB + enqueue |
| `handlers.go` | `inProcessOrchestrator.Terminate` | Kill + state transition |
| `handlers.go` | `inProcessOrchestrator.Exec` | Forward exec to host-agent |
| `scheduler.go` | `NewScheduler` | Creates scheduler + empty heap |
| `scheduler.go` | `RegisterHost` | Adds a host-agent to the registry |
| `scheduler.go` | `Enqueue` | Adds sandbox to priority queue |
| `scheduler.go` | `bestHost` / `scoreHost` | Picks best machine for placement |
| `scheduler.go` | `RunDispatchLoop` | Background placement loop |
| `scheduler.go` | `dispatchPending` | Places one sandbox per iteration |
| `scheduler.go` | `grpcPlacer.PlaceSandbox` | HTTP call to host-agent |

---

### `cmd/host-agent/` — VM runtime (Linux)

| File | Function | Purpose |
|---|---|---|
| `main.go` | `main()` | Starts internal API + metrics + pool reconciler |
| `internal_api.go` | `registerInternalRoutes` | `/internal/v1/*` endpoints |
| `internal_api.go` | `handlePlace/Terminate/Exec/Heartbeat` | HTTP wrappers around server methods |
| `server.go` | `PlaceSandbox` | Full VM boot pipeline (overlay → cgroup → Firecracker → eBPF) |
| `server.go` | `TerminateSandbox` | Cancel VM context |
| `server.go` | `ExecInSandbox` | vsock forward to vm-agent |
| `server.go` | `Heartbeat` | Returns active VM count |
| `server.go` | `watchVM` | Reaps VM on exit, cleans cgroup/overlay |
| `server.go` | `runPoolReconciler` | Keeps warm VM pool at target size |
| `server.go` | `reconcilePool` | Emits pool metrics, future: pre-boot idle VMs |
| `server.go` | `waitForSocket` | Polls for Firecracker Unix socket |
| `server.go` | `cid` | Derives vsock context ID |
| `server.go` | `copyFile` | Copies base rootfs for sandbox disk |
| `proto_types.go` | `RegisterHostAgentServer` | gRPC stub (placeholder until protoc runs) |

---

### `internal/agentapi/` — Internal HTTP protocol

| Function | Purpose |
|---|---|
| `NewClient(baseURL, secret)` | Creates authenticated HTTP client |
| `Client.PlaceSandbox` | POST place — boot VM |
| `Client.TerminateSandbox` | POST terminate — kill VM |
| `Client.ExecInSandbox` | POST exec — run command |
| `Client.Heartbeat` | POST heartbeat — health check |
| `Client.post` | Generic JSON HTTP POST helper |

---

### `internal/spec/` — Request types

| Function | Purpose |
|---|---|
| `SandboxSpec.Validate()` | Validates CPU, memory, timeout, image fields |
| `SandboxSpec` | What the user wants (image, cpu, memory, timeout) |
| `ExecRequest` | Command + stdin + timeout for exec |
| `ExecResult` | stdout, stderr, exit_code, duration_ms |

---

### `internal/firecracker/` — Firecracker API client

| Function | Purpose |
|---|---|
| `NewClient(socketPath)` | HTTP over Unix socket to one Firecracker process |
| `PutMachineConfig` | Set vCPUs and RAM |
| `PutBootSource` | Set kernel path and boot args |
| `PutDrive` | Attach block device (rootfs) |
| `PutNetworkInterface` | Attach tap/veth network device |
| `StartInstance` | Boot the VM (cold start) |
| `SnapshotCreate` | Save VM state to files |
| `SnapshotLoad` | Restore VM from snapshot (warm start) |

---

### `internal/cgroup/` — Resource limits

| Function | Purpose |
|---|---|
| `Setup(id, Limits)` | Create cgroup, write cpu.max and memory.max |
| `AddPID(id, pid)` | Move Firecracker process into cgroup |
| `Teardown(id)` | Delete cgroup subtree |
| `OOMCount(id)` | Read how many times kernel OOM-killed this cgroup |

---

### `internal/overlay/` — Filesystem isolation

| Function | Purpose |
|---|---|
| `Setup(cfg)` | Create dirs, mount overlayfs (lower+upper→merged) |
| `Teardown(cfg)` | Unmount, wipe upper layer, verify empty (anti-leak) |
| `mountOverlay` (linux) | `syscall.Mount("overlay", ...)` |
| `verifyEmptyUpper` | sha256 check that upper layer has no leftover tenant data |

---

### `internal/vsock/` — Host → VM communication

| Function | Purpose |
|---|---|
| `Exec(cid, req)` | Dial vm-agent, send JSON, read JSON response |
| `dial(cid, port)` | Platform-specific AF_VSOCK dial (linux) or error (stub) |

---

### `internal/snapshot/` — Warm starts

| Function | Purpose |
|---|---|
| `NewManager(snapshotDir)` | Manager for snapshot files on disk |
| `Create(ctx, key, fc)` | Save `.snap` + `.mem` files via Firecracker API |
| `Load(ctx, key, fc)` | Restore VM from snapshot files |
| `Exists(key)` | Check both snapshot files are on disk |

---

### `internal/quota/` — Fair usage

| Function | Purpose |
|---|---|
| `MemoryStore.Acquire(tenant, limit)` | Increment counter; error if over limit |
| `MemoryStore.Release(tenant)` | Decrement counter on sandbox kill |

---

### `internal/metrics/` — Observability

Prometheus metrics (package-level vars, incremented from host-agent and scheduler):

| Metric | Meaning |
|---|---|
| `SandboxColdStartDuration` | Time to boot VM from kernel |
| `SandboxWarmStartDuration` | Time to restore from snapshot |
| `SandboxExecDuration` | Time for exec command inside VM |
| `SandboxOOMKillsTotal` | Count of OOM kills per tenant |
| `VMPoolActiveCount` | Running VMs on this host |
| `VMPoolIdleCount` | Pre-booted idle VMs in warm pool |
| `SchedulerQueueDepth` | Sandboxes waiting for placement |

---

### `ebpf/loader/` — Network egress control

| Function | Purpose |
|---|---|
| `NewEgressFilter(sandboxID, hostVeth)` | Config for one sandbox's network filter |
| `Load(objectPath)` | Load `.o` file, attach TC egress program to veth |
| `AddCIDR(cidr)` | Allow traffic to this destination IP/range |
| `Detach()` | Remove program and clsact qdisc |

**Kernel program:** `ebpf/programs/egress_filter.c` — runs on every outbound packet; drops if destination IP not in `allowed_dsts` LPM trie map.

---

### `internal/network/` — Virtual networking (Linux)

| Function | Purpose |
|---|---|
| `Setup(sandboxID)` | Create network namespace + veth pair + IP addresses |
| `Destroy(sandboxID)` | Delete veth and netns |
| `ensureBridge` | Create `sandock0` bridge if missing |
| `allocateIPs` | Deterministic /30 IP pair from sandbox ID hash |

---

### `internal/registry/` — Image integrity

| Function | Purpose |
|---|---|
| `Resolve(name)` | Look up image in local cache |
| `VerifyDigest(ref)` | sha256 hash file, compare to expected digest |
| `ComputeDigest(path)` | Helper to compute digest when adding images |

---

### `internal/persistence/` — Filesystem archives

| Function | Purpose |
|---|---|
| `LocalStore.Save(key, srcDir)` | Tar + zstd compress directory to `.tar.zst` |
| `LocalStore.Restore(key, dstDir)` | Decompress and extract archive |
| `LocalStore.Exists/Delete` | Manage archive lifecycle |

---

### `internal/wireguard/` — Tenant network isolation (Linux)

| Function | Purpose |
|---|---|
| `NewManager()` | Open wgctrl client |
| `EnsureTunnel(t)` | Create WireGuard interface, configure peer, add routes |
| `RemoveTunnel(tenantID)` | Delete WireGuard interface |
| `Stats(tenantID)` | Read tunnel byte counters |

---

### `internal/tracing/` — Distributed tracing

| Function | Purpose |
|---|---|
| `Init(service, endpoint)` | Connect to OTLP collector (Jaeger/Tempo) |
| `Start(ctx, name)` | Open a child span (e.g. `"api.create_sandbox"`) |
| `FromContext(ctx)` | Get current span for adding attributes |

---

### `internal/pool/` — Warm VM pool math

| Function | Purpose |
|---|---|
| `PoolManager.UpdateRPS` | Record requests-per-second sample |
| `PoolManager.TargetSize` | EWMA-smoothed prediction of how many idle VMs to keep |
| `PoolManager.Delta` | How many VMs to boot or destroy to reach target |

---

### `vm-agent/` — Rust in-VM exec service

| Function | Purpose |
|---|---|
| `main()` | Bind socket, accept connections forever |
| `handle_connection(conn)` | One request → one response per connection |
| `read_line(conn)` | Read newline-terminated JSON |
| Exec via `/bin/sh -c` | Run user's shell command |
| `wait_with_output()` | Capture stdout, stderr, exit code |

---

### `sdk/typescript/` and `sdk/go/sandock/` — Client libraries

Both expose the same operations:

| Method | HTTP call |
|---|---|
| `Create(spec)` | POST `/v1/sandboxes` |
| `Get(id)` | GET `/v1/sandboxes/:id` |
| `List()` | GET `/v1/sandboxes` |
| `Kill(id)` | DELETE `/v1/sandboxes/:id` |
| `Exec(id, req)` | POST `/v1/sandboxes/:id/exec` |
| `WaitRunning(id)` | Poll Get until state is `running` |

---

## 14. Security — defense in depth

Each layer is **independent**. Even if one fails, others still protect you.

```
Layer 1: Firecracker + KVM     → Hardware VM boundary
Layer 2: Linux namespaces        → PID, mount, network isolation inside guest
Layer 3: seccomp-bpf             → Syscall allowlist in Firecracker jailer
Layer 4: cgroup v2               → CPU + memory caps
Layer 5: overlayfs upper wipe    → No disk data leaks between tenants
Layer 6: eBPF egress filter        → Network allowlist per sandbox
Layer 7: WireGuard per tenant    → Encrypted tunnel between tenants
Layer 8: Quota                   → Prevent one tenant monopolizing all VMs
Layer 9: STONITH fencing         → Dead hosts are powered off before reuse
Layer 10: Image digest verify    → Supply-chain integrity for base images
```

---

## 15. How to trace a request yourself

### Recommended reading order

1. `internal/spec/spec.go` — understand the data shapes
2. `internal/state/state.go` — understand lifecycle states
3. `cmd/api/handlers.go` — see HTTP entry points
4. `cmd/api/scheduler.go` — see how placement works
5. `internal/agentapi/agentapi.go` — see internal RPC types
6. `cmd/host-agent/server.go` — see VM boot (the heart of the system)
7. `internal/firecracker/client.go` — see how Firecracker is controlled
8. `vm-agent/src/main.rs` — see what runs inside the VM

### Debug tips

```bash
# Watch API logs
./bin/api --config deploy/config.example.yaml

# Watch host-agent logs (Linux + root)
sudo ./bin/host-agent --config deploy/config.example.yaml

# Create a sandbox
curl -X POST http://localhost:8080/v1/sandboxes \
  -H "Authorization: Bearer change-me-in-production" \
  -H "Content-Type: application/json" \
  -d '{"image":"base","cpu_millis":500,"memory_mib":256,"timeout_ms":30000}'

# Poll state
curl http://localhost:8080/v1/sandboxes/sb-XXXX \
  -H "Authorization: Bearer change-me-in-production"

# Exec
curl -X POST http://localhost:8080/v1/sandboxes/sb-XXXX/exec \
  -H "Authorization: Bearer change-me-in-production" \
  -H "Content-Type: application/json" \
  -d '{"command":"echo hello"}'
```

### Questions to ask while reading any function

1. **Who calls this?** (upstream)
2. **What does it return?** (downstream)
3. **What state does it change?** (`state.Store`, maps, cgroups, etc.)
4. **What happens if it fails?** (cleanup? retry? fail sandbox?)
5. **Is it sync or async?** (goroutine? background watcher?)

---

## 16. Glossary

| Term | Definition |
|---|---|
| **API Gateway** | `cmd/api` — the only public-facing HTTP server |
| **Host Agent** | `cmd/host-agent` — manages VMs on one physical machine |
| **Orchestrator** | Scheduler + state store — decides *where* sandboxes run |
| **Placer** | Component that actually calls host-agent to boot a VM |
| **Sandbox** | One microVM instance + its resources |
| **Tenant** | Customer account identified by API token |
| **microVM** | Tiny VM booted by Firecracker (minimal device model) |
| **vsock** | Virtual socket between host and guest VM |
| **vm-agent** | Rust process inside guest that runs shell commands |
| **overlayfs** | Layered filesystem: shared read-only base + private writable upper |
| **cgroup** | Linux kernel resource limit group |
| **eBPF** | Bytecode programs running in the Linux kernel |
| **LPM trie** | Longest-prefix-match map used by eBPF for IP allowlists |
| **clsact qdisc** | Traffic control hook point for attaching eBPF to a network interface |
| **STONITH** | "Shoot The Other Node In The Head" — force power-off a dead host |
| **Warm pool** | Pre-booted idle VMs ready for instant assignment |
| **Cold start** | Boot VM from kernel image (~125 ms) |
| **Warm start** | Restore VM from snapshot (~30 ms) |
| **EWMA** | Exponentially weighted moving average — smooths RPS for pool sizing |
| **OCI** | Open Container Initiative image format |
| **OTel** | OpenTelemetry — distributed tracing standard |

---

## Quick reference: one create request in 20 lines

```
Client POST /v1/sandboxes
  → handleCreateSandbox()           [auth, validate, quota]
    → inProcessOrchestrator.Submit()
      → id.NewSandbox()
      → store.Create(queued)
      → scheduler.Enqueue()
        → [background] dispatchPending()
          → bestHost()
          → store.Transition(provisioning)
          → grpcPlacer.PlaceSandbox()
            → agentapi.Client POST /internal/v1/place
              → hostAgentServer.PlaceSandbox()
                → overlay.Setup()
                → cgroup.Setup()
                → firecracker start + configure + boot
                → eBPF egress filter attach
                → go watchVM()
          → store.Transition(running)
  → return 201 { id, state: "queued" }
```

---

*This guide reflects the codebase as implemented. For build instructions see [README.md](README.md). For phase-specific deep dives see [docs/phase-1-flow.md](docs/phase-1-flow.md), [docs/phase-2-flow.md](docs/phase-2-flow.md), and [docs/phase-3-flow.md](docs/phase-3-flow.md).*
