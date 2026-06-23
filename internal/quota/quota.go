// Layer: API gateway internal — Phase 2 tenant quota enforcement.
// Uses a Redis INCR+EXPIRE token-bucket per tenant to cap concurrent sandboxes.
// On each create request the API increments the counter; on sandbox termination it decrements.
//
// Phase 1: in-memory quota store (no Redis dependency).
// Phase 2: swap the in-memory store for a Redis client using INCR with EXPIRE.
//
// The interface is stable so Phase 1 → 2 is a drop-in swap of the Store implementation.
package quota

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Store tracks per-tenant sandbox concurrency.
type Store interface {
	// Acquire increments the tenant's concurrency counter.
	// Returns an error if the tenant is at or above their limit.
	Acquire(tenantID string, limit int) error
	// Release decrements the tenant's concurrency counter.
	Release(tenantID string)
}

// MemoryStore is the Phase 1 in-process quota store.
// Thread-safe via atomic operations on a per-tenant counter map.
type MemoryStore struct {
	mu       sync.RWMutex
	counters map[string]*int64
}

// NewMemoryStore creates an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{counters: make(map[string]*int64)}
}

// Acquire increments the counter and returns an error if it exceeds limit.
func (s *MemoryStore) Acquire(tenantID string, limit int) error {
	ptr := s.getOrCreate(tenantID)
	newVal := atomic.AddInt64(ptr, 1)
	if int(newVal) > limit {
		// Roll back the increment before returning the error.
		atomic.AddInt64(ptr, -1)
		return fmt.Errorf("quota: tenant %s at limit %d", tenantID, limit)
	}
	return nil
}

// Release decrements the counter. Safe to call even if the counter is already zero.
func (s *MemoryStore) Release(tenantID string) {
	ptr := s.getOrCreate(tenantID)
	if atomic.LoadInt64(ptr) > 0 {
		atomic.AddInt64(ptr, -1)
	}
}

// Current returns the current concurrent sandbox count for a tenant.
func (s *MemoryStore) Current(tenantID string) int {
	ptr := s.getOrCreate(tenantID)
	return int(atomic.LoadInt64(ptr))
}

func (s *MemoryStore) getOrCreate(tenantID string) *int64 {
	s.mu.RLock()
	ptr, ok := s.counters[tenantID]
	s.mu.RUnlock()
	if ok {
		return ptr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Double-check after acquiring write lock.
	if ptr, ok = s.counters[tenantID]; ok {
		return ptr
	}
	var v int64
	s.counters[tenantID] = &v
	return &v
}
