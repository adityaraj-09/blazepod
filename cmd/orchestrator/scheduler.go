// Layer: orchestrator — admission and scheduling logic.
// Implements the admission flow described in the guide:
//   Request arrives → validate → quota check → priority queue → host scoring → placement.
//
// The Scheduler owns the host registry, the pending request queue, and the
// placement decision. It communicates with host agents over gRPC.
// Phase 1: single in-process host (no real gRPC), scoring is simplified.
// Phase 2: multi-host gRPC clients, EWMA pool demand prediction.
package main

import (
	"container/heap"
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/sandock/sandock/internal/spec"
	"github.com/sandock/sandock/internal/state"
)

// HostInfo tracks what the orchestrator knows about each host agent.
type HostInfo struct {
	// ID is the unique host identifier (from host-agent Heartbeat).
	ID string
	// Addr is the gRPC address of the host agent.
	Addr string
	// ActiveVMs is the last reported count of running VMs.
	ActiveVMs uint32
	// IdlePool is the last reported count of warm idle VMs.
	IdlePool uint32
	// CPUUsagePct is the last reported CPU usage (0–100).
	CPUUsagePct float32
	// LastSeen is the time of the last successful heartbeat.
	LastSeen time.Time
	// Healthy is false if the host has missed heartbeats or reported errors.
	Healthy bool
}

// scoreHost computes a placement score for a host.
// Higher is better. Formula from the guide:
//   score = 0.4×cpu_fit + 0.35×warm_pool_hit + 0.25×host_health
//
// cpu_fit:       1.0 when CPUUsagePct < 50, decreasing linearly to 0 at 100%.
// warm_pool_hit: 1.0 if IdlePool > 0, 0.0 otherwise.
// host_health:   1.0 if Healthy, 0.0 otherwise.
func scoreHost(h *HostInfo) float64 {
	if !h.Healthy {
		return -1 // Never place on an unhealthy host.
	}
	cpuFit := 1.0 - float64(h.CPUUsagePct)/100.0
	if cpuFit < 0 {
		cpuFit = 0
	}
	warmPoolHit := 0.0
	if h.IdlePool > 0 {
		warmPoolHit = 1.0
	}
	return 0.4*cpuFit + 0.35*warmPoolHit + 0.25*1.0
}

// pendingItem is a queued sandbox placement request in the priority heap.
type pendingItem struct {
	sandboxID string
	spec      *spec.SandboxSpec
	deadline  time.Time
	priority  int // higher value = dequeue sooner
	index     int // position in the heap (maintained by heap.Interface)
}

// pendingQueue is a min-heap ordered by deadline then priority.
// Items closer to their deadline are dequeued first.
type pendingQueue []*pendingItem

func (pq pendingQueue) Len() int { return len(pq) }
func (pq pendingQueue) Less(i, j int) bool {
	if pq[i].deadline.Equal(pq[j].deadline) {
		return pq[i].priority > pq[j].priority // higher priority first on tie
	}
	return pq[i].deadline.Before(pq[j].deadline)
}
func (pq pendingQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}
func (pq *pendingQueue) Push(x any) {
	n := len(*pq)
	item := x.(*pendingItem)
	item.index = n
	*pq = append(*pq, item)
}
func (pq *pendingQueue) Pop() any {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*pq = old[:n-1]
	return item
}

// Scheduler manages host registration, request queuing, and placement decisions.
type Scheduler struct {
	log   *zap.Logger
	store *state.Store

	mu    sync.RWMutex
	hosts map[string]*HostInfo

	queueMu sync.Mutex
	queue   pendingQueue

	// placeCh is notified when a new item is enqueued so the dispatch loop wakes up.
	placeCh chan struct{}
}

// NewScheduler creates and returns a new Scheduler.
func NewScheduler(log *zap.Logger, store *state.Store) *Scheduler {
	s := &Scheduler{
		log:     log,
		store:   store,
		hosts:   make(map[string]*HostInfo),
		placeCh: make(chan struct{}, 100),
	}
	heap.Init(&s.queue)
	return s
}

// RegisterHost adds or updates a host in the scheduler's registry.
// Called when a host-agent connects and on each Heartbeat response.
func (s *Scheduler) RegisterHost(h *HostInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h.LastSeen = time.Now()
	h.Healthy = true
	s.hosts[h.ID] = h
	s.log.Info("host registered", zap.String("host_id", h.ID), zap.String("addr", h.Addr))
}

// UpdateHostHealth marks a host as unhealthy if it has missed heartbeats.
// Called periodically by a health-check goroutine.
func (s *Scheduler) UpdateHostHealth(hostID string, healthy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.hosts[hostID]; ok {
		h.Healthy = healthy
	}
}

// Enqueue adds a sandbox placement request to the priority queue.
// timeoutMs controls the deadline — the request is dropped if not scheduled in time.
func (s *Scheduler) Enqueue(sandboxID string, sp *spec.SandboxSpec, timeoutMs uint32) {
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	item := &pendingItem{
		sandboxID: sandboxID,
		spec:      sp,
		deadline:  deadline,
		priority:  1, // Phase 2: use plan tier weight × recency factor
	}

	s.queueMu.Lock()
	heap.Push(&s.queue, item)
	s.queueMu.Unlock()

	// Wake up the dispatch loop.
	select {
	case s.placeCh <- struct{}{}:
	default:
	}
}

// bestHost returns the highest-scoring healthy host, or an error if none is available.
func (s *Scheduler) bestHost() (*HostInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best *HostInfo
	var bestScore float64 = -2

	for _, h := range s.hosts {
		sc := scoreHost(h)
		if sc > bestScore {
			bestScore = sc
			best = h
		}
	}
	if best == nil {
		return nil, fmt.Errorf("scheduler: no healthy hosts available")
	}
	return best, nil
}

// RunDispatchLoop runs the placement dispatch loop.
// It dequeues placement requests, picks the best host, and calls PlaceSandbox.
// Call this in a goroutine; cancel ctx to stop it.
func (s *Scheduler) RunDispatchLoop(ctx context.Context, placer SandboxPlacer) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.placeCh:
		case <-ticker.C:
		}

		s.dispatchPending(ctx, placer)
	}
}

func (s *Scheduler) dispatchPending(ctx context.Context, placer SandboxPlacer) {
	s.queueMu.Lock()
	if s.queue.Len() == 0 {
		s.queueMu.Unlock()
		return
	}
	item := heap.Pop(&s.queue).(*pendingItem)
	s.queueMu.Unlock()

	// Drop expired requests.
	if time.Now().After(item.deadline) {
		s.log.Warn("sandbox request expired", zap.String("sandbox_id", item.sandboxID))
		_ = s.store.Transition(item.sandboxID, state.StateQueued, state.StateFailed, "deadline exceeded")
		return
	}

	host, err := s.bestHost()
	if err != nil {
		// Re-enqueue and retry shortly.
		s.log.Warn("no host available, re-queueing", zap.String("sandbox_id", item.sandboxID), zap.Error(err))
		s.queueMu.Lock()
		heap.Push(&s.queue, item)
		s.queueMu.Unlock()
		return
	}

	_ = s.store.SetHostID(item.sandboxID, host.ID)
	if err := s.store.Transition(item.sandboxID, state.StateQueued, state.StateProvisioning, ""); err != nil {
		s.log.Error("state transition queued→provisioning failed", zap.Error(err))
		return
	}

	go func(item *pendingItem, host *HostInfo) {
		vsockCID, err := placer.PlaceSandbox(ctx, item.sandboxID, host, item.spec)
		if err != nil {
			s.log.Error("placement failed", zap.String("sandbox_id", item.sandboxID), zap.Error(err))
			_ = s.store.Transition(item.sandboxID, state.StateProvisioning, state.StateFailed, err.Error())
			return
		}
		_ = vsockCID // stored by placer
		if err := s.store.Transition(item.sandboxID, state.StateProvisioning, state.StateRunning, ""); err != nil {
			s.log.Error("state transition provisioning→running failed", zap.Error(err))
		}
		s.log.Info("sandbox running", zap.String("sandbox_id", item.sandboxID), zap.String("host_id", host.ID))
	}(item, host)
}

// SandboxPlacer is an interface the dispatch loop uses to actually provision VMs.
// The real implementation calls the host-agent gRPC PlaceSandbox RPC.
// In tests it can be replaced with a mock.
type SandboxPlacer interface {
	PlaceSandbox(ctx context.Context, sandboxID string, host *HostInfo, sp *spec.SandboxSpec) (vsockCID uint32, err error)
}
