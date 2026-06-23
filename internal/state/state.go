// Layer: shared internal — sandbox lifecycle state machine.
// Defines the complete set of valid states and enforces legal transitions.
// Every layer (API, orchestrator, host-agent) uses this package to mutate state
// so that illegal transitions are caught at compile time and at runtime.
//
// State graph:
//   Queued → Provisioning → Running → Draining → Terminated
//                        ↘ Failed (terminal)
//                                   ↘ Failed (terminal)
package state

import (
	"fmt"
	"sync"
	"time"
)

// State is the lifecycle stage of a sandbox.
type State string

const (
	// Queued: request has been validated and admitted; waiting for a host slot.
	StateQueued State = "queued"
	// Provisioning: host agent has been assigned; VM is being created.
	StateProvisioning State = "provisioning"
	// Running: VM is live, vsock exec endpoint is available.
	StateRunning State = "running"
	// Draining: timeout or kill requested; VM teardown in progress.
	StateDraining State = "draining"
	// Terminated: VM has been destroyed and resources cleaned up. Terminal.
	StateTerminated State = "terminated"
	// Failed: an unrecoverable error occurred. Terminal.
	StateFailed State = "failed"
)

// validTransitions defines all legal state moves.
// Any transition not in this map is illegal and must be rejected.
var validTransitions = map[State][]State{
	StateQueued:       {StateProvisioning, StateFailed},
	StateProvisioning: {StateRunning, StateFailed},
	StateRunning:      {StateDraining, StateFailed},
	StateDraining:     {StateTerminated},
	StateTerminated:   {},
	StateFailed:       {},
}

// Transition validates that moving from → to is legal.
// Returns a typed error on illegal transitions so callers can distinguish
// validation failures from storage errors.
func Transition(from, to State) error {
	allowed, ok := validTransitions[from]
	if !ok {
		return fmt.Errorf("state: unknown source state %q", from)
	}
	for _, a := range allowed {
		if a == to {
			return nil
		}
	}
	return fmt.Errorf("state: illegal transition %s → %s", from, to)
}

// IsTerminal returns true if the state has no further transitions.
func IsTerminal(s State) bool {
	transitions, ok := validTransitions[s]
	return ok && len(transitions) == 0
}

// SandboxRecord is the full runtime record for a sandbox instance.
// The in-memory store (Phase 1) and etcd (Phase 2+) both store this struct.
type SandboxRecord struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	State     State     `json:"state"`
	HostID    string    `json:"host_id,omitempty"`
	// VsockCID is the KVM vsock context ID assigned to this VM after placement.
	VsockCID  uint32    `json:"vsock_cid,omitempty"`
	// VMAgentSocket is the Unix socket path for the vm-agent (dev/non-vsock mode).
	VMAgentSocket string  `json:"vm_agent_socket,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// FailReason is populated when the sandbox enters the Failed state.
	FailReason string `json:"fail_reason,omitempty"`
}

// Store is a simple thread-safe in-memory sandbox registry.
// Phase 1: this is the only persistence. Phase 2+ replaces it with etcd.
type Store struct {
	mu      sync.RWMutex
	records map[string]*SandboxRecord
}

// NewStore returns an empty in-memory Store.
func NewStore() *Store {
	return &Store{records: make(map[string]*SandboxRecord)}
}

// Create inserts a new sandbox record in the Queued state.
// Returns an error if a record with the same ID already exists.
func (s *Store) Create(r *SandboxRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.records[r.ID]; exists {
		return fmt.Errorf("state: sandbox %s already exists", r.ID)
	}
	r.State = StateQueued
	r.CreatedAt = time.Now().UTC()
	r.UpdatedAt = r.CreatedAt
	s.records[r.ID] = r
	return nil
}

// Transition atomically moves sandbox id from expectedCurrent to newState.
// Returns an error if the record doesn't exist, the current state doesn't match,
// or the transition is illegal.
func (s *Store) Transition(id string, expectedCurrent, newState State, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return fmt.Errorf("state: sandbox %s not found", id)
	}
	if r.State != expectedCurrent {
		return fmt.Errorf("state: sandbox %s is in %s, expected %s", id, r.State, expectedCurrent)
	}
	if err := Transition(r.State, newState); err != nil {
		return err
	}
	r.State = newState
	r.UpdatedAt = time.Now().UTC()
	if newState == StateFailed {
		r.FailReason = reason
	}
	return nil
}

// Get retrieves a sandbox record by ID.
func (s *Store) Get(id string) (*SandboxRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[id]
	if !ok {
		return nil, fmt.Errorf("state: sandbox %s not found", id)
	}
	// Return a copy so callers cannot mutate the stored record directly.
	copy := *r
	return &copy, nil
}

// List returns all sandbox records (copy). Optionally filter by tenantID.
func (s *Store) List(tenantID string) []*SandboxRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*SandboxRecord
	for _, r := range s.records {
		if tenantID != "" && r.TenantID != tenantID {
			continue
		}
		copy := *r
		out = append(out, &copy)
	}
	return out
}

// SetHostID sets the host assigned to a sandbox (called when provisioning starts).
func (s *Store) SetHostID(id, hostID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return fmt.Errorf("state: sandbox %s not found", id)
	}
	r.HostID = hostID
	r.UpdatedAt = time.Now().UTC()
	return nil
}

// SetVsockCID stores the vsock CID and optional Unix socket path returned by the host-agent.
func (s *Store) SetVsockCID(id string, cid uint32, unixSocket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return fmt.Errorf("state: sandbox %s not found", id)
	}
	r.VsockCID = cid
	r.VMAgentSocket = unixSocket
	r.UpdatedAt = time.Now().UTC()
	return nil
}
