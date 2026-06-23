// Layer: host-agent internal — Phase 2 Prometheus metrics definitions.
// All sandock metrics are defined here as package-level vars so every component
// imports this package and uses the same label names and histogram buckets.
//
// Metrics are labeled by tenant_id, image, and region so operators can slice
// cost, latency, and error rate per tenant and per image.
//
// Phase 1: metrics are registered but only a subset are emitted.
// Phase 2: wire all metrics to real Firecracker lifecycle events.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// SandboxColdStartDuration measures the wall-clock time from PlaceSandbox
	// to VM kernel boot completion (no snapshot involved).
	SandboxColdStartDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandbox_cold_start_duration_seconds",
			Help:    "Wall-clock time from PlaceSandbox call to VM kernel ready (cold boot).",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"tenant_id", "image", "region"},
	)

	// SandboxWarmStartDuration measures snapshot restore time.
	SandboxWarmStartDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandbox_warm_start_duration_seconds",
			Help:    "Wall-clock time from PlaceSandbox call to VM ready via snapshot restore.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5},
		},
		[]string{"tenant_id", "image", "region"},
	)

	// SandboxExecDuration measures the wall-clock time of exec commands.
	SandboxExecDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandbox_exec_duration_seconds",
			Help:    "Wall-clock time from exec request to process exit inside the VM.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"tenant_id", "image"},
	)

	// SandboxOOMKillsTotal counts the number of OOM kills per sandbox.
	SandboxOOMKillsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_oom_kills_total",
			Help: "Number of times a sandbox was OOM-killed by the kernel.",
		},
		[]string{"tenant_id", "image"},
	)

	// SandboxEgressBytesTotal counts outbound network bytes per sandbox.
	SandboxEgressBytesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_egress_bytes_total",
			Help: "Total outbound bytes from sandboxes (measured at the veth/TC layer).",
		},
		[]string{"tenant_id"},
	)

	// VMPoolIdleCount is a gauge of warm idle VMs on this host.
	VMPoolIdleCount = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vm_pool_idle_count",
			Help: "Number of warm idle VMs in the pre-boot pool on this host.",
		},
		[]string{"host"},
	)

	// VMPoolActiveCount is a gauge of running VMs on this host.
	VMPoolActiveCount = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vm_pool_active_count",
			Help: "Number of currently active (running) VMs on this host.",
		},
		[]string{"host"},
	)

	// SchedulerQueueDepth is a gauge of pending placement requests.
	SchedulerQueueDepth = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "scheduler_queue_depth",
			Help: "Current number of sandbox placement requests waiting in the scheduler queue.",
		},
	)

	// SchedulerDecisionLatency measures the time the scheduler spends picking a host.
	SchedulerDecisionLatency = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "scheduler_decision_latency_ms",
			Help:    "Time in milliseconds for the scheduler to select a host for a placement request.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 25, 50, 100},
		},
	)
)
