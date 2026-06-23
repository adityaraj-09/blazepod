// Layer: shared internal — quota unit tests. No network or Redis required.
package quota

import (
	"testing"
)

func TestAcquireAndRelease(t *testing.T) {
	store := NewMemoryStore()
	limit := 3

	for i := 0; i < limit; i++ {
		if err := store.Acquire("ten-test", limit); err != nil {
			t.Fatalf("Acquire %d should succeed: %v", i, err)
		}
	}

	// One more should fail.
	if err := store.Acquire("ten-test", limit); err == nil {
		t.Error("expected quota exceeded error, got nil")
	}

	// After releasing one, acquire should succeed again.
	store.Release("ten-test")
	if err := store.Acquire("ten-test", limit); err != nil {
		t.Errorf("Acquire after release should succeed: %v", err)
	}
}

func TestSeparateTenants(t *testing.T) {
	store := NewMemoryStore()
	// Exhaust tenant A's quota.
	for i := 0; i < 2; i++ {
		_ = store.Acquire("ten-a", 2)
	}
	if err := store.Acquire("ten-a", 2); err == nil {
		t.Error("ten-a should be at quota")
	}
	// tenant B is independent.
	if err := store.Acquire("ten-b", 2); err != nil {
		t.Errorf("ten-b quota should not be affected by ten-a: %v", err)
	}
}
