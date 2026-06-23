# Phase 2 — Flow Explanation (Hardening)

## What Gets Wired in Phase 2

Phase 2 separates the API and orchestrator into distinct processes communicating over gRPC,
adds a warm VM pool, snapshot/restore for <30ms starts, eBPF egress filtering, Redis quota
enforcement, and Prometheus metrics. The single-host stub becomes a real multi-host system.

---

## Multi-Host Architecture

```
                  ┌──────────────────────────────┐
  Client SDK ──▶  │  cmd/api                     │  REST/WS  :8080
                  │  handlers.go + scheduler.go   │
                  └────────────┬─────────────────┘
                               │  gRPC (proto/sandock.proto)
                  ┌────────────▼─────────────────┐
                  │  cmd/orchestrator             │  gRPC  :9090
                  │  main.go + scheduler.go       │
                  └─────┬───────────────┬─────────┘
                 gRPC   │               │  gRPC
          ┌────────────▼──┐     ┌──────▼────────────┐
          │ host-agent    │     │ host-agent         │  ...N hosts
          │ host-1 :9091  │     │ host-2 :9091       │
          └───────────────┘     └────────────────────┘
```

---

## VM Pool Manager Flow

```
host-agent startup:
  1. pool.NewPoolManager(initialRPS=0, p95DurSec=0.3)      internal/pool/pool.go
  2. Every 10s: metrics pipeline emits observed RPS
       pool.UpdateRPS(newRPS)          ← EWMA smoothing
  3. Every 5s: compare pool.TargetSize() vs pool.CurrentSize()
       if delta > 0: boot delta warm VMs from snapshot
       if delta < 0: teardown excess idle VMs

TargetSize formula (Little's Law + headroom):
  concurrency = ewmaRPS × p95DurSec
  target = max(concurrency × 1.3, MinPoolSize=10)
```

**File:** `internal/pool/pool.go`

---

## Snapshot/Restore Flow

```
Once (base image preparation):
  1. Boot a clean VM with the base rootfs.
  2. snapshot.Manager.Create(ctx, "base", fc)       internal/snapshot/snapshot.go
       → fc.SnapshotCreate()                         internal/firecracker/client.go
       → writes base.snap + base.mem to snapshot_dir

Per sandbox request (warm start):
  1. Start a new Firecracker process.
  2. snapshot.Manager.Load(ctx, "base", fc)
       → fc.SnapshotLoad()
       → mmap base.mem into guest RAM (no kernel boot)
  3. Apply per-sandbox overlayfs upper layer.
  4. VM is live in <30ms vs ~125ms cold boot.
```

**Files:** `internal/snapshot/snapshot.go`, `internal/firecracker/client.go`

---

## eBPF Egress Filter Flow

```
On PlaceSandbox (after veth pair creation):
  1. ebpf.NewEgressFilter(sandboxID, hostVeth)       ebpf/loader/loader.go
  2. filter.Load("egress_filter.o")
       → attach TC classifier to host-side veth
       → create LPM trie BPF map for this sandbox
  3. For each CIDR in spec.EgressAllowlist:
       filter.AddCIDR(cidr)
       → bpf_map_update_elem(allowed_dsts, lpm_key, 1)

On every outbound packet from sandbox (in-kernel, no userspace):
  ebpf/programs/egress_filter.c  sandbox_egress()
  ├── parse ETH header
  ├── parse IP header
  ├── LPM trie lookup on daddr
  ├── found → TC_ACT_OK (pass)
  └── not found → TC_ACT_SHOT (drop silently)

On TerminateSandbox:
  filter.Detach()  → remove TC filter + delete BPF map
```

**Files:** `ebpf/programs/egress_filter.c`, `ebpf/loader/loader.go`

---

## Redis Quota Enforcement Flow

```
On POST /v1/sandboxes:
  quota.Store.Acquire(tenantID, tenantLimit)          internal/quota/quota.go
  Phase 1: MemoryStore (in-process atomic counter)
  Phase 2: Redis INCR sandock:quota:<tenantID>
             EXPIRE sandock:quota:<tenantID> <window>
             if result > limit → return 429

On sandbox Terminated / Failed:
  quota.Store.Release(tenantID)
  Phase 2: Redis DECR sandock:quota:<tenantID>
```

**File:** `internal/quota/quota.go`

---

## Prometheus Metrics Flow

```
cmd/host-agent/main.go starts Prometheus HTTP server on MetricsAddr.

Each lifecycle event records a metric:
  PlaceSandbox cold:  metrics.SandboxColdStartDuration.Observe(elapsed)
  PlaceSandbox warm:  metrics.SandboxWarmStartDuration.Observe(elapsed)
  ExecInSandbox:      metrics.SandboxExecDuration.Observe(elapsed)
  OOM kill detected:  metrics.SandboxOOMKillsTotal.Inc()
  Pool update:        metrics.VMPoolIdleCount.Set(idle)
                      metrics.VMPoolActiveCount.Set(active)
  Scheduler dispatch: metrics.SchedulerDecisionLatency.Observe(ms)
                      metrics.SchedulerQueueDepth.Set(depth)
```

**File:** `internal/metrics/metrics.go`
