// Layer: shared internal — configuration loading for all sandock services.
// Reads YAML config from disk, merges with environment variable overrides,
// and exposes typed structs consumed by cmd/api, cmd/orchestrator, and cmd/host-agent.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// TracingConfig configures OpenTelemetry distributed tracing.
type TracingConfig struct {
	// Endpoint is the OTLP collector endpoint (e.g. "localhost:4318").
	// Leave empty to disable tracing.
	Endpoint string `yaml:"endpoint"`
}

// Config is the root configuration object shared across all sandock services.
type Config struct {
	API          APIConfig          `yaml:"api"`
	Orchestrator OrchestratorConfig `yaml:"orchestrator"`
	HostAgent    HostAgentConfig    `yaml:"host_agent"`
	// HostAgents is the list of host-agents the orchestrator connects to (Phase 2 multi-host).
	HostAgents   []HostAgentEntry   `yaml:"host_agents"`
	Redis        RedisConfig        `yaml:"redis"`
	Tracing      TracingConfig      `yaml:"tracing"`
	Log          LogConfig          `yaml:"log"`
}

// APIConfig configures the public-facing REST/WebSocket gateway.
type APIConfig struct {
	// ListenAddr is the host:port the API server binds to.
	ListenAddr string `yaml:"listen_addr"`
	// JWTSecret is the HMAC-SHA256 key used to verify API tokens.
	JWTSecret string `yaml:"jwt_secret"`
}

// OrchestratorConfig configures the central scheduler.
type OrchestratorConfig struct {
	// GRPCAddr is the address the orchestrator HTTP server listens on.
	GRPCAddr string `yaml:"grpc_addr"`
	// StateBackend selects the state store: "memory" (phase 1) or "etcd" (phase 2+).
	StateBackend string `yaml:"state_backend"`
}

// HostAgentEntry is one host-agent registration in the orchestrator config.
type HostAgentEntry struct {
	// ID is the unique host identifier.
	ID string `yaml:"id"`
	// HTTPAddr is the host:port of the host-agent internal HTTP API.
	HTTPAddr string `yaml:"http_addr"`
}

// HostAgentConfig configures the per-host Firecracker runtime manager.
type HostAgentConfig struct {
	// GRPCAddr is the address the host-agent HTTP internal API server listens on.
	// The orchestrator connects to this address for PlaceSandbox, Exec, Heartbeat.
	GRPCAddr string `yaml:"grpc_addr"`
	// GRPCStubAddr is an optional port for the raw gRPC stub server (protoc stubs, Phase 2+).
	GRPCStubAddr string `yaml:"grpc_stub_addr"`
	// AgentSecret is the shared secret used for intra-cluster authentication (X-Agent-Secret header).
	AgentSecret string `yaml:"agent_secret"`
	// FirecrackerBin is the path to the firecracker binary.
	FirecrackerBin string `yaml:"firecracker_bin"`
	// JailerBin is the path to the jailer binary (wraps Firecracker with seccomp/chroot).
	JailerBin string `yaml:"jailer_bin"`
	// KernelImage is the path to the guest Linux kernel vmlinux image.
	KernelImage string `yaml:"kernel_image"`
	// BaseRootfs is the path to the base ext4 rootfs image (lower overlayfs layer).
	BaseRootfs string `yaml:"base_rootfs"`
	// SandboxDir is the directory under which per-sandbox overlay dirs are created.
	SandboxDir string `yaml:"sandbox_dir"`
	// SnapshotDir is the directory for Firecracker snapshot files.
	SnapshotDir string `yaml:"snapshot_dir"`
	// MetricsAddr is the Prometheus metrics listen address for this host agent.
	MetricsAddr string `yaml:"metrics_addr"`
	// WarmPoolSize is the number of idle VMs to keep pre-booted (Phase 2).
	WarmPoolSize int `yaml:"warm_pool_size"`
}

// RedisConfig configures the Redis connection used for tenant quota tracking.
type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

// LogConfig controls structured logging output.
type LogConfig struct {
	// Level is one of "debug", "info", "warn", "error".
	Level string `yaml:"level"`
	// Format is "json" (production) or "console" (development).
	Format string `yaml:"format"`
}

// Load reads a YAML config file from path and returns a validated Config.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", path, err)
	}

	applyDefaults(&cfg)
	return &cfg, nil
}

// applyDefaults fills in reasonable defaults for fields not set in the YAML file.
func applyDefaults(cfg *Config) {
	if cfg.API.ListenAddr == "" {
		cfg.API.ListenAddr = "0.0.0.0:8080"
	}
	if cfg.Orchestrator.GRPCAddr == "" {
		cfg.Orchestrator.GRPCAddr = "0.0.0.0:9090"
	}
	if cfg.Orchestrator.StateBackend == "" {
		cfg.Orchestrator.StateBackend = "memory"
	}
	if cfg.HostAgent.GRPCAddr == "" {
		cfg.HostAgent.GRPCAddr = "0.0.0.0:9091"
	}
	if cfg.HostAgent.FirecrackerBin == "" {
		cfg.HostAgent.FirecrackerBin = "/usr/local/bin/firecracker"
	}
	if cfg.HostAgent.JailerBin == "" {
		cfg.HostAgent.JailerBin = "/usr/local/bin/jailer"
	}
	if cfg.HostAgent.SandboxDir == "" {
		cfg.HostAgent.SandboxDir = "/var/lib/sandock/sandboxes"
	}
	if cfg.HostAgent.SnapshotDir == "" {
		cfg.HostAgent.SnapshotDir = "/var/lib/sandock/snapshots"
	}
	if cfg.HostAgent.MetricsAddr == "" {
		cfg.HostAgent.MetricsAddr = "0.0.0.0:9100"
	}
	if cfg.Redis.Addr == "" {
		cfg.Redis.Addr = "localhost:6379"
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = "json"
	}
}
