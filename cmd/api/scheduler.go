// Layer: API gateway — embedded scheduler for Phase 1 single-process mode.
// In Phase 1 the API server runs the scheduler in-process to avoid requiring
// a separate orchestrator process. This file copies the minimal scheduler types
// needed by cmd/api.
//
// Phase 2: remove this file. The API will use the gRPC orchestrator client instead.
package main

import (
	"container/heap"
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/sandock/sandock/internal/agentapi"
	"github.com/sandock/sandock/internal/spec"
	"github.com/sandock/sandock/internal/state"
)

// HostInfo tracks the orchestrator's knowledge of a host agent.
type HostInfo struct {
	ID          string
	Addr        string
	ActiveVMs   uint32
	IdlePool    uint32
	CPUUsagePct float32
	LastSeen    time.Time
	Healthy     bool
}

func scoreHost(h *HostInfo) float64 {
	if !h.Healthy {
		return -1
	}
	cpuFit := 1.0 - float64(h.CPUUsagePct)/100.0
	if cpuFit < 0 {
		cpuFit = 0
	}
	warmPoolHit := 0.0
	if h.IdlePool > 0 {
		warmPoolHit = 1.0
	}
	return 0.4*cpuFit + 0.35*warmPoolHit + 0.25
}

type pendingItem struct {
	sandboxID string
	spec      *spec.SandboxSpec
	deadline  time.Time
	priority  int
	index     int
}

type pendingQueue []*pendingItem

func (pq pendingQueue) Len() int { return len(pq) }
func (pq pendingQueue) Less(i, j int) bool {
	if pq[i].deadline.Equal(pq[j].deadline) {
		return pq[i].priority > pq[j].priority
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

// Scheduler manages host registration, request queuing, and placement.
type Scheduler struct {
	log   *zap.Logger
	store *state.Store

	mu    sync.RWMutex
	hosts map[string]*HostInfo

	queueMu sync.Mutex
	queue   pendingQueue
	placeCh chan struct{}
}

// NewScheduler returns a ready-to-use Scheduler.
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

// RegisterHost adds or updates a host entry.
func (s *Scheduler) RegisterHost(h *HostInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h.LastSeen = time.Now()
	h.Healthy = true
	s.hosts[h.ID] = h
}

// Enqueue adds a sandbox to the priority queue.
func (s *Scheduler) Enqueue(sandboxID string, sp *spec.SandboxSpec, timeoutMs uint32) {
	item := &pendingItem{
		sandboxID: sandboxID,
		spec:      sp,
		deadline:  time.Now().Add(time.Duration(timeoutMs) * time.Millisecond),
		priority:  1,
	}
	s.queueMu.Lock()
	heap.Push(&s.queue, item)
	s.queueMu.Unlock()
	select {
	case s.placeCh <- struct{}{}:
	default:
	}
}

func (s *Scheduler) bestHost() (*HostInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best *HostInfo
	bestScore := -2.0
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

// SandboxPlacer is the interface for actual VM provisioning.
type SandboxPlacer interface {
	PlaceSandbox(ctx context.Context, sandboxID string, host *HostInfo, sp *spec.SandboxSpec) (vsockCID uint32, err error)
}

// RunDispatchLoop runs continuously, dispatching queued sandboxes to hosts.
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

	if time.Now().After(item.deadline) {
		_ = s.store.Transition(item.sandboxID, state.StateQueued, state.StateFailed, "deadline exceeded")
		return
	}

	host, err := s.bestHost()
	if err != nil {
		s.queueMu.Lock()
		heap.Push(&s.queue, item)
		s.queueMu.Unlock()
		return
	}

	_ = s.store.SetHostID(item.sandboxID, host.ID)
	if err := s.store.Transition(item.sandboxID, state.StateQueued, state.StateProvisioning, ""); err != nil {
		return
	}

	go func(item *pendingItem, host *HostInfo) {
		_, err := placer.PlaceSandbox(ctx, item.sandboxID, host, item.spec)
		if err != nil {
			_ = s.store.Transition(item.sandboxID, state.StateProvisioning, state.StateFailed, err.Error())
			return
		}
		_ = s.store.Transition(item.sandboxID, state.StateProvisioning, state.StateRunning, "")
		s.log.Info("sandbox running", zap.String("sandbox_id", item.sandboxID))
	}(item, host)
}

// grpcPlacer calls the host-agent over HTTP/JSON to provision a VM.
// Phase 2: real HTTP call to agentapi endpoints on the host-agent.
type grpcPlacer struct {
	store       *state.Store
	hostAddr    string
	agentSecret string
	log         *zap.Logger
}

func (p *grpcPlacer) PlaceSandbox(ctx context.Context, sandboxID string, host *HostInfo, sp *spec.SandboxSpec) (uint32, error) {
	p.log.Info("placing sandbox via host agent",
		zap.String("sandbox_id", sandboxID),
		zap.String("host_addr", host.Addr),
	)

	client := agentapi.NewClient("http://"+host.Addr, p.agentSecret)
	resp, err := client.PlaceSandbox(ctx, &agentapi.PlaceRequest{
		SandboxID: sandboxID,
		TenantID:  sp.TenantID,
		ImageRef:  sp.Image,
		CPUMillis: sp.CPUMillis,
		MemMiB:    sp.MemoryMiB,
		TimeoutMs: sp.TimeoutMs,
	})
	if err != nil {
		return 0, fmt.Errorf("host-agent PlaceSandbox: %w", err)
	}

	// Store vsock CID / Unix socket in state for exec calls.
	_ = p.store.SetVsockCID(sandboxID, resp.VsockCID, resp.UnixSocket)
	return resp.VsockCID, nil
}
