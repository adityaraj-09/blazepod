# sandock — Sandbox Provider

> **New to the codebase?** Read **[LEARN.md](LEARN.md)** — a complete beginner's guide that walks every request flow step-by-step and explains what each function does.

A world-class sandbox provider built from scratch following the architecture in `sandbox-provider-guide.html`.
Runs untrusted code inside Firecracker microVMs with hardware KVM isolation, overlayfs layered storage,
cgroup v2 resource limits, eBPF egress filtering, and a TypeScript/Go SDK.

## Architecture

```
[TypeScript SDK / CLI]
        ↓  REST + WebSocket
[cmd/api]            Go — public REST API, auth, exec streaming
        ↓  in-process (Phase 1) / gRPC (Phase 2+)
[cmd/orchestrator]   Go — scheduler, state machine, VM pool, host placement
        ↓  gRPC (proto/sandock.proto)
[cmd/host-agent]     Go — Firecracker VMM manager, overlayfs, cgroups, networking
        ↓  Firecracker API (Unix socket)
[Firecracker]        Rust (AWS) — KVM-backed microVM, virtio devices
        ↓  AF_VSOCK
[vm-agent]           Rust — in-VM exec service (runs inside every microVM)
```

## Language Choices

| Component | Language | Why |
|---|---|---|
| API, Orchestrator, Host Agent | **Go** | Best goroutine model for thousands of concurrent VMs |
| vm-agent (in-VM exec) | **Rust** | Zero-GC predictable latency in the hot path |
| eBPF egress filter | **C** (restricted) | Only option; CO-RE for kernel portability |
| SDK + CLI | **TypeScript** | Developer-facing, type-safe, npm-publishable |

## Repository Layout

```
sandock/
├── cmd/
│   ├── api/            Go — REST/WebSocket API server
│   ├── orchestrator/   Go — scheduler and state machine
│   └── host-agent/     Go — per-host Firecracker runtime (Linux only)
├── internal/
│   ├── config/         shared config loading
│   ├── state/          sandbox lifecycle state machine + in-memory store
│   ├── spec/           SandboxSpec types and validation
│   ├── firecracker/    Firecracker VMM API client
│   ├── cgroup/         cgroup v2 resource limits
│   ├── overlay/        overlayfs mount/teardown lifecycle
│   ├── vsock/          AF_VSOCK host↔VM communication
│   ├── pool/           Phase 2: EWMA VM pool demand prediction
│   ├── snapshot/       Phase 2: Firecracker snapshot/restore
│   ├── quota/          Phase 2: tenant quota enforcement
│   ├── metrics/        Phase 2: Prometheus metric definitions
│   ├── network/        Phase 2: veth pair + net namespace setup
│   ├── fencing/        Phase 3: STONITH host fencing
│   ├── tracing/        Phase 3: OpenTelemetry distributed tracing
│   ├── registry/       Phase 3: OCI image registry + digest verification
│   ├── persistence/    Phase 3: tar.zst filesystem persistence (S3/R2)
│   └── wireguard/      Phase 3: per-tenant WireGuard network isolation
├── proto/
│   └── sandock.proto   gRPC service definitions
├── vm-agent/           Rust — in-VM exec agent (serde_json wire protocol)
├── ebpf/
│   ├── programs/       C — eBPF TC egress filter
│   └── loader/         Go — eBPF object loader (cilium/ebpf)
├── sdk/
│   └── typescript/     TypeScript SDK + sandock CLI
├── deploy/             Linux host setup scripts, example config
├── docs/               Phase flow explanations
│   ├── phase-1-flow.md
│   ├── phase-2-flow.md
│   └── phase-3-flow.md
└── Makefile
```

## Build

```bash
# Build all Go binaries
make build

# Run all tests (Go + Rust + TypeScript)
make test

# Run only Go tests (no KVM required — works on macOS)
make test-go

# Build TypeScript SDK
cd sdk/typescript && npm install && npm run build
```

## Phase 1 Quick Start (Linux with KVM)

```bash
# 1. Set up the Linux host (downloads Firecracker, creates dirs)
sudo bash deploy/linux-host-setup.sh

# 2. Configure
cp deploy/config.example.yaml /etc/sandock/config.yaml
# Edit /etc/sandock/config.yaml: set kernel_image and base_rootfs paths

# 3. Build
make build

# 4. Start the host agent (manages Firecracker VMs)
sudo ./bin/host-agent --config /etc/sandock/config.yaml &

# 5. Start the API (includes in-process orchestrator in Phase 1)
./bin/api --config /etc/sandock/config.yaml &

# 6. Test with the CLI
export SANDOCK_URL=http://localhost:8080
export SANDOCK_TOKEN=change-me-in-production
node sdk/typescript/bin/sandock.js create --image base --cpu 500 --memory 256 --timeout 30000
```

## Phase 1 SDK Example

```typescript
import { SandockClient } from "@sandock/sdk";

const client = new SandockClient({
  baseURL: "http://localhost:8080",
  apiKey: "change-me-in-production",
});

const sb = await client.sandboxes.create({
  image: "base",
  cpu_millis: 500,
  memory_mib: 256,
  timeout_ms: 30_000,
});

await sb.waitUntilRunning();

const result = await sb.exec("echo hello from sandock");
console.log(result.stdout); // "hello from sandock\n"

await sb.kill();
```

## Build Roadmap

| Phase | Weeks | Goal | Target |
|---|---|---|---|
| Phase 1 | 1–6 | Single-host POC | 200ms p99 cold start |
| Phase 2 | 7–14 | Multi-host, pool, eBPF, quota | 30ms p99 warm start, 3-nines |
| Phase 3 | 15–24 | Production scale | <15ms p99 at 10k concurrent VMs |

## Security Model

Defense in depth — each layer is independent:

1. **Hardware isolation** — Firecracker KVM (VT-x / AMD-V)
2. **Namespace isolation** — PID, Net, Mount, User, IPC, UTS, Time
3. **Syscall allowlist** — seccomp-bpf (SIGKILL on violation)
4. **Resource limits** — cgroup v2 (CPU, memory)
5. **Network egress policy** — eBPF TC filter with per-sandbox LPM trie
6. **Upper layer wipe** — cryptographic emptiness check before slot reuse
7. **STONITH** — host fencing before slot reassignment (Phase 3)
