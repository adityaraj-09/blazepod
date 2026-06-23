# Phase 3 — Flow Explanation (Production Scale)

## What Gets Added in Phase 3

Phase 3 adds fault tolerance, end-to-end observability, a full image supply chain,
filesystem persistence for agent workloads, WireGuard per-tenant isolation,
and SDK release packaging. Target: <15ms p99 warm start at 10k concurrent sandboxes.

---

## STONITH Host Fencing Flow

```
Problem: orchestrator cannot reach host-agent via gRPC (network partition or crash).

Without STONITH:
  Orchestrator marks host's VMs as free.
  New tenant A is placed on those slots.
  Old tenant B's VM may still be running (split-brain) — two tenants own same VM.

With STONITH:
  1. Orchestrator misses N consecutive heartbeats from host-X.
  2. fencing.HealthScorer.Score("host-X") returns 0.0.
  3. Scheduler stops scheduling onto host-X.
  4. fencing.IPMIFencer.Fence("host-X")              internal/fencing/fencing.go
       → IPMI chassis power-off + power-on via BMC
       → Wait for power-on confirmation
  5. Only after successful fence: mark host-X slots as free.
  6. New sandboxes are placed on those slots.
```

**File:** `internal/fencing/fencing.go`

---

## OpenTelemetry Distributed Tracing Flow

```
Single sandbox request generates one trace spanning all services:

  API handler span
    └── quota check span
    └── scheduler decision span
          └── host-agent PlaceSandbox span
                └── VM restore span
                └── cgroup setup span
                └── overlay mount span
    └── exec span
          └── vsock roundtrip span
                └── vm-agent exec span

tracing.Init(serviceName, otlpEndpoint)       internal/tracing/tracing.go
  Phase 3: configures OTLP exporter → Jaeger / Grafana Tempo

tracing.StartSpan(ctx, "scheduler.decision")
  → adds span to the trace
  → span.AddAttr("host_id", hostID)
  → span.AddAttr("sandbox_id", sandboxID)
```

**File:** `internal/tracing/tracing.go`

---

## OCI Image Registry Flow

```
Image supply chain rule: NEVER pull from Docker Hub at runtime.
All images must live in the internal OCI registry.

Phase 3 workflow:
  1. Build base rootfs (Dockerfile) and push to internal registry:
       docker build -t registry.internal/sandock/base:python312 .
       oras push registry.internal/sandock/base:python312 rootfs.tar.zst

  2. Host agent resolves images at PlaceSandbox time:
       registry.Resolve("python:3.12")              internal/registry/registry.go
         → fetch OCI manifest from internal registry
         → verify sha256 digest === manifest.digest  (supply chain integrity)
         → download layer if not cached at base_rootfs_dir

  3. Mount the verified layer as overlayfs lowerdir.
```

**File:** `internal/registry/registry.go`

---

## Filesystem Persistence Flow

```
Use case: AI agent runs a sandbox, installs packages, writes files.
Next sandbox resume should see those files.

On sandbox exit (persist mode):
  persistence.S3Store.Save(ctx, sandboxID, tenantID, upperDir)
    ├── tar the upper overlayfs directory (only files written by the sandbox)
    ├── zstd compress (faster than gzip at similar ratio)
    └── upload to s3://bucket/<tenantID>/<sandboxID>.tar.zst

On next sandbox start for same sandboxID:
  persistence.S3Store.Load(ctx, sandboxID, tenantID, upperDir)
    ├── download from S3
    ├── zstd decompress
    ├── tar extract into fresh upperDir
    └── mount overlayfs → sandbox sees previous filesystem state
```

**File:** `internal/persistence/persistence.go`

---

## WireGuard Per-Tenant Isolation Flow

```
Defense in depth: even if eBPF is misconfigured, tenant traffic cannot
be read by another tenant because it's encrypted with tenant-specific WireGuard keys.

Setup (once per tenant per host):
  wireguard.Manager.EnsureTunnel(&TenantTunnel{       internal/wireguard/wireguard.go
    TenantID:      "ten-abc",
    Interface:     "wg-ten-abc",
    PrivateKey:    hostPrivKey,
    PeerPublicKey: tenantPubKey,
    Endpoint:      "tenant-vpn.example.com:51820",
    AllowedIPs:    ["10.200.1.0/24"],
  })

Per sandbox:
  Route sandbox traffic through the tenant WireGuard interface:
    ip rule add from <sandboxIP> lookup <tenantRouteTable>
    ip route add default dev wg-ten-abc table <tenantRouteTable>

On tenant offboard:
  wireguard.Manager.RemoveTunnel(tenantID)
```

**File:** `internal/wireguard/wireguard.go`

---

## TypeScript SDK Release

```
cd sdk/typescript
npm run build                → compiles src/ → dist/
npm publish --access public  → publishes @sandock/sdk to npm

Usage by developers:
  npm install @sandock/sdk

  import { SandockClient } from "@sandock/sdk";
  const client = new SandockClient({ baseURL: "https://api.sandock.dev", apiKey: "..." });
  const sb = await client.sandboxes.create({ image: "python:3.12", cpu_millis: 500, ... });
  await sb.waitUntilRunning();
  const result = await sb.exec("python3 -c 'print(42)'");
  console.log(result.stdout); // "42\n"
  await sb.kill();
```

**Files:** `sdk/typescript/src/client.ts`, `sdk/typescript/src/types.ts`, `sdk/typescript/bin/sandock.js`
