// Layer: host-agent internal — cgroup v2 resource limit setup.
// Every sandbox gets its own cgroup subtree under /sys/fs/cgroup/sandboxes/<id>/.
// This enforces CPU quota, memory limit, and memory swap limit.
//
// Requires Linux kernel 5.2+ with cgroup v2 (unified hierarchy) enabled.
// The host agent process must have write access to /sys/fs/cgroup/sandboxes/
// (either running as root or with appropriate cgroup delegation).
//
// Compile on macOS/Windows: the package compiles but all operations are no-ops
// that return a clear "Linux required" error, allowing unit tests to import the package.
package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	// cgroupBase is the parent cgroup directory for all sandbox subtrees.
	cgroupBase = "/sys/fs/cgroup/sandboxes"
)

// Limits defines the resource constraints for a sandbox cgroup.
type Limits struct {
	// CPUMillis is the CPU bandwidth in thousandths of a CPU (e.g. 500 = 50% of 1 CPU).
	CPUMillis uint32
	// MemoryMiB is the maximum RSS memory in mebibytes.
	MemoryMiB uint32
	// SwapMiB is the max swap usage. 0 disables swap.
	SwapMiB uint32
}

// Setup creates the cgroup subtree for sandboxID and writes resource limits.
func Setup(sandboxID string, l Limits) (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("cgroup: requires Linux (current OS: %s)", runtime.GOOS)
	}
	dir := filepath.Join(cgroupBase, sandboxID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("cgroup: mkdir %s: %w", dir, err)
	}

	cpuQuota := uint64(l.CPUMillis) * 1000
	if err := writeFile(dir, "cpu.max", fmt.Sprintf("%d 1000000", cpuQuota)); err != nil {
		return "", err
	}

	memBytes := uint64(l.MemoryMiB) * 1024 * 1024
	if err := writeFile(dir, "memory.max", fmt.Sprintf("%d", memBytes)); err != nil {
		return "", err
	}

	swapVal := "0"
	if l.SwapMiB > 0 {
		swapVal = fmt.Sprintf("%d", uint64(l.SwapMiB)*1024*1024)
	}
	if err := writeFile(dir, "memory.swap.max", swapVal); err != nil {
		return "", err
	}

	return dir, nil
}

// AddPID writes a PID into the cgroup's cgroup.procs file.
func AddPID(sandboxID string, pid int) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("cgroup: requires Linux")
	}
	dir := filepath.Join(cgroupBase, sandboxID)
	return writeFile(dir, "cgroup.procs", fmt.Sprintf("%d", pid))
}

// Teardown removes the cgroup subtree for sandboxID.
func Teardown(sandboxID string) error {
	if runtime.GOOS != "linux" {
		return nil // no-op on macOS
	}
	dir := filepath.Join(cgroupBase, sandboxID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("cgroup: teardown %s: %w", dir, err)
	}
	return nil
}

// OOMCount reads memory.events and returns how many times the sandbox OOM-killed.
func OOMCount(sandboxID string) (uint64, error) {
	if runtime.GOOS != "linux" {
		return 0, nil
	}
	dir := filepath.Join(cgroupBase, sandboxID)
	data, err := os.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		return 0, fmt.Errorf("cgroup: read memory.events for %s: %w", sandboxID, err)
	}
	var oom uint64
	for _, line := range splitLines(string(data)) {
		var key string
		var val uint64
		if n, _ := fmt.Sscanf(line, "%s %d", &key, &val); n == 2 && key == "oom" {
			oom = val
			break
		}
	}
	return oom, nil
}

func writeFile(dir, name, value string) error {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0644); err != nil {
		return fmt.Errorf("cgroup: write %s: %w", path, err)
	}
	return nil
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
