// Layer: shared internal — unit tests for the state machine.
// These tests run without KVM or any Linux-specific facilities.
package state

import (
	"testing"
)

func TestLegalTransitions(t *testing.T) {
	cases := []struct {
		from State
		to   State
		ok   bool
	}{
		{StateQueued, StateProvisioning, true},
		{StateQueued, StateFailed, true},
		{StateProvisioning, StateRunning, true},
		{StateProvisioning, StateFailed, true},
		{StateRunning, StateDraining, true},
		{StateRunning, StateFailed, true},
		{StateDraining, StateTerminated, true},
		// Illegal transitions:
		{StateQueued, StateRunning, false},
		{StateRunning, StateQueued, false},
		{StateTerminated, StateRunning, false},
		{StateFailed, StateRunning, false},
	}

	for _, c := range cases {
		err := Transition(c.from, c.to)
		if c.ok && err != nil {
			t.Errorf("expected %s→%s to be legal, got: %v", c.from, c.to, err)
		}
		if !c.ok && err == nil {
			t.Errorf("expected %s→%s to be illegal, got nil error", c.from, c.to)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	if !IsTerminal(StateTerminated) {
		t.Error("Terminated should be terminal")
	}
	if !IsTerminal(StateFailed) {
		t.Error("Failed should be terminal")
	}
	if IsTerminal(StateRunning) {
		t.Error("Running should not be terminal")
	}
}

func TestStoreCreateAndTransition(t *testing.T) {
	store := NewStore()
	r := &SandboxRecord{ID: "sb-test1", TenantID: "ten-abc"}

	if err := store.Create(r); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify initial state.
	got, err := store.Get("sb-test1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateQueued {
		t.Errorf("expected Queued, got %s", got.State)
	}

	// Legal transition.
	if err := store.Transition("sb-test1", StateQueued, StateProvisioning, ""); err != nil {
		t.Fatalf("Transition to Provisioning: %v", err)
	}

	// Wrong expected current state should fail.
	if err := store.Transition("sb-test1", StateQueued, StateRunning, ""); err == nil {
		t.Error("expected error for wrong current state, got nil")
	}

	// Illegal transition should fail.
	if err := store.Transition("sb-test1", StateProvisioning, StateTerminated, ""); err == nil {
		t.Error("expected error for illegal transition Provisioning→Terminated")
	}
}

func TestStoreDuplicateCreate(t *testing.T) {
	store := NewStore()
	r := &SandboxRecord{ID: "sb-dup", TenantID: "ten-abc"}
	if err := store.Create(r); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := store.Create(r); err == nil {
		t.Error("expected error on duplicate Create, got nil")
	}
}
