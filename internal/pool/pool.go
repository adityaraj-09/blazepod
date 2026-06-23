// Layer: host-agent internal — Phase 2 VM warm pool manager.
// Maintains a pool of pre-booted, snapshot-restored Firecracker VMs
// so sandbox requests can be satisfied in <30ms instead of ~125ms cold boot.
//
// Pool sizing uses Little's Law with EWMA-smoothed RPS:
//   target = ewmaRPS × p95DurationSec × 1.3 (headroom factor)
//
// Phase 1: this package is scaffolded only. The pool manager is a no-op
// stub so the package compiles and the types are ready for Phase 2 wiring.
// Phase 2: implement warm/idle VM bookkeeping, snapshot restore, and auto-scaling.
package pool

import (
	"sync"
	"sync/atomic"
)

const (
	// HeadroomFactor is the multiplier applied to Little's Law concurrency estimate.
	// 1.3 = maintain 30% headroom above predicted demand.
	HeadroomFactor = 1.3
	// MinPoolSize is the minimum number of warm VMs to keep regardless of demand.
	MinPoolSize = 10
	// EWMAAlpha is the smoothing factor for the EWMA RPS estimator.
	// Larger alpha → faster response to demand changes; smaller → smoother.
	EWMAAlpha = 0.3
)

// PoolManager tracks warm VM demand and emits target pool sizes.
type PoolManager struct {
	mu          sync.Mutex
	ewmaRPS     float64
	p95DurSec   float64
	currentPool int32 // current count of warm VMs (atomic for read-heavy paths)
}

// NewPoolManager creates a PoolManager with initial estimates.
func NewPoolManager(initialRPS, p95DurSec float64) *PoolManager {
	return &PoolManager{
		ewmaRPS:   initialRPS,
		p95DurSec: p95DurSec,
	}
}

// UpdateRPS feeds a new observed RPS sample into the EWMA estimator.
// Call this every time the metrics pipeline emits a new request rate sample.
func (p *PoolManager) UpdateRPS(newRPS float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// EWMA: smoothed = alpha × new + (1-alpha) × previous
	p.ewmaRPS = EWMAAlpha*newRPS + (1-EWMAAlpha)*p.ewmaRPS
}

// TargetSize returns the recommended warm pool size based on current demand.
// Uses Little's Law: avg_concurrency = arrival_rate × avg_duration.
func (p *PoolManager) TargetSize() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	concurrency := p.ewmaRPS * p.p95DurSec
	target := int(concurrency * HeadroomFactor)
	if target < MinPoolSize {
		return MinPoolSize
	}
	return target
}

// CurrentSize returns the number of warm VMs currently in the pool.
func (p *PoolManager) CurrentSize() int {
	return int(atomic.LoadInt32(&p.currentPool))
}

// SetCurrentSize updates the observed pool size (called by the host agent after boot/teardown).
func (p *PoolManager) SetCurrentSize(n int) {
	atomic.StoreInt32(&p.currentPool, int32(n))
}

// Delta returns how many VMs need to be added (+) or removed (-) to hit the target.
func (p *PoolManager) Delta() int {
	return p.TargetSize() - p.CurrentSize()
}
