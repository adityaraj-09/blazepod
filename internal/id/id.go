// Layer: shared internal — sandbox and tenant ID generation.
// Uses UUID v4 (random) prefixed with a human-readable type tag so IDs are
// self-describing in logs: "sb-<uuid>", "ten-<uuid>", "host-<uuid>".
package id

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// NewSandbox generates a new unique sandbox ID.
func NewSandbox() string {
	return fmt.Sprintf("sb-%s", shortUUID())
}

// NewTenant generates a new unique tenant ID.
func NewTenant() string {
	return fmt.Sprintf("ten-%s", shortUUID())
}

// NewHost generates a new unique host ID.
func NewHost() string {
	return fmt.Sprintf("host-%s", shortUUID())
}

// shortUUID returns the first 8 hex chars of a random UUID.
// Full UUID collisions at this prefix length are astronomically unlikely
// given typical sandbox-provider concurrency (<100k active IDs).
func shortUUID() string {
	u := uuid.New().String()
	return strings.ReplaceAll(u, "-", "")[:12]
}
