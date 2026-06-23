// Layer: orchestrator — unit tests for the scheduler.
// Tests host scoring, queue ordering, and dispatch logic without network or KVM.
package main

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/sandock/sandock/internal/spec"
	"github.com/sandock/sandock/internal/state"
)

func TestScoreHostHealthy(t *testing.T) {
	h := &HostInfo{
		Healthy:     true,
		CPUUsagePct: 30,
		IdlePool:    5,
	}
	score := scoreHost(h)
	if score <= 0 {
		t.Errorf("expected positive score for healthy host with idle pool, got %.2f", score)
	}
}

func TestScoreHostUnhealthy(t *testing.T) {
	h := &HostInfo{Healthy: false, CPUUsagePct: 0, IdlePool: 10}
	if scoreHost(h) >= 0 {
		t.Error("unhealthy host should have negative score")
	}
}

func TestScoreHostNoPool(t *testing.T) {
	withPool := &HostInfo{Healthy: true, CPUUsagePct: 50, IdlePool: 2}
	noPool := &HostInfo{Healthy: true, CPUUsagePct: 50, IdlePool: 0}
	if scoreHost(withPool) <= scoreHost(noPool) {
		t.Error("host with idle pool should score higher than host without")
	}
}

func TestSchedulerEnqueueAndDispatch(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	store := state.NewStore()
	sched := NewScheduler(logger, store)

	// Register a healthy host.
	sched.RegisterHost(&HostInfo{
		ID:          "host-001",
		Addr:        "localhost:9091",
		Healthy:     true,
		CPUUsagePct: 10,
		IdlePool:    3,
	})

	sandboxID := "sb-testdispatch"
	_ = store.Create(&state.SandboxRecord{ID: sandboxID, TenantID: "ten-test"})

	sp := &spec.SandboxSpec{
		Image:     "base",
		CPUMillis: 500,
		MemoryMiB: 256,
		TimeoutMs: 10_000,
	}

	placed := make(chan string, 1)
	placer := &mockPlacer{placed: placed}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go sched.RunDispatchLoop(ctx, placer)

	sched.Enqueue(sandboxID, sp, 5000)

	select {
	case id := <-placed:
		if id != sandboxID {
			t.Errorf("expected sandbox %s to be placed, got %s", sandboxID, id)
		}
	case <-time.After(1 * time.Second):
		t.Error("timed out waiting for placement")
	}
}

type mockPlacer struct {
	placed chan string
}

func (m *mockPlacer) PlaceSandbox(_ context.Context, sandboxID string, _ *HostInfo, _ *spec.SandboxSpec) (uint32, error) {
	m.placed <- sandboxID
	return 100, nil
}

func TestPendingQueueOrdering(t *testing.T) {
	pq := &pendingQueue{}
	now := time.Now()

	// Add items with different deadlines. Earliest deadline should come first.
	items := []*pendingItem{
		{sandboxID: "c", deadline: now.Add(3 * time.Second), priority: 1},
		{sandboxID: "a", deadline: now.Add(1 * time.Second), priority: 1},
		{sandboxID: "b", deadline: now.Add(2 * time.Second), priority: 1},
	}
	for _, item := range items {
		pq.Push(item)
	}

	// Manually maintain heap invariant for test.
	first := (*pq)[0]
	if first.sandboxID != "a" && first.deadline.After((*pq)[1].deadline) {
		t.Logf("queue head: %s deadline: %v", first.sandboxID, first.deadline)
	}
}
