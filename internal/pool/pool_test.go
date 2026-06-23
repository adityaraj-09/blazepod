// Layer: shared internal — unit tests for the pool manager.
// All tests are pure math; no network or KVM required.
package pool

import (
	"testing"
)

func TestTargetSizeMinFloor(t *testing.T) {
	pm := NewPoolManager(0, 1.0) // 0 RPS → concurrency 0 → should hit MinPoolSize floor
	if pm.TargetSize() != MinPoolSize {
		t.Errorf("expected MinPoolSize %d, got %d", MinPoolSize, pm.TargetSize())
	}
}

func TestTargetSizeLittlesLaw(t *testing.T) {
	// 100 RPS × 0.5s p95 = 50 concurrency × 1.3 headroom = 65 target
	pm := NewPoolManager(100, 0.5)
	target := pm.TargetSize()
	if target != 65 {
		t.Errorf("expected target 65, got %d", target)
	}
}

func TestEWMASmoothing(t *testing.T) {
	pm := NewPoolManager(100, 1.0)
	// Spike to 200 RPS — EWMA should move toward 200 but not reach it immediately.
	pm.UpdateRPS(200)
	if pm.ewmaRPS <= 100 || pm.ewmaRPS >= 200 {
		t.Errorf("EWMA after spike should be between 100 and 200, got %.2f", pm.ewmaRPS)
	}
}

func TestDelta(t *testing.T) {
	pm := NewPoolManager(0, 1.0)
	pm.SetCurrentSize(5)
	// Target is MinPoolSize=10, current=5, delta should be +5.
	if pm.Delta() != 5 {
		t.Errorf("expected delta 5, got %d", pm.Delta())
	}
}
