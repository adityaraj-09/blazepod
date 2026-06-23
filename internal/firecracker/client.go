// Layer: host-agent internal — Firecracker VMM HTTP API client.
// Firecracker exposes a Unix domain socket REST API for VM lifecycle management.
// This package wraps those API calls: boot config, drives, network interfaces,
// machine config, start, pause, snapshot, and load.
//
// Firecracker API reference: https://github.com/firecracker-microvm/firecracker/blob/main/src/api_server/swagger/firecracker.yaml
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client wraps HTTP calls to a single Firecracker process over its Unix socket.
type Client struct {
	// socketPath is the path to the Firecracker control Unix socket.
	socketPath string
	http       *http.Client
}

// NewClient creates a Client that communicates with the Firecracker process
// via the given Unix domain socket path.
func NewClient(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
		},
	}
}

// MachineConfig describes the vCPU and memory allocation for the microVM.
type MachineConfig struct {
	// VcpuCount is the number of virtual CPUs to allocate.
	VcpuCount int `json:"vcpu_count"`
	// MemSizeMib is the guest memory in mebibytes.
	MemSizeMib int `json:"mem_size_mib"`
	// SMT controls Simultaneous Multi-Threading on x86; keep false for isolation.
	SMT bool `json:"smt"`
}

// BootSource tells Firecracker where to find the guest kernel and what
// kernel command line to pass.
type BootSource struct {
	// KernelImagePath is the absolute path to the uncompressed vmlinux image.
	KernelImagePath string `json:"kernel_image_path"`
	// BootArgs is the Linux kernel command line.
	BootArgs string `json:"boot_args"`
}

// Drive represents a block device attachment.
type Drive struct {
	// DriveID is an arbitrary identifier (e.g. "rootfs").
	DriveID    string `json:"drive_id"`
	PathOnHost string `json:"path_on_host"`
	IsRootDevice bool `json:"is_root_device"`
	IsReadOnly bool   `json:"is_read_only"`
}

// NetworkInterface describes a virtio-net interface for the guest.
type NetworkInterface struct {
	// IfaceID is an arbitrary identifier (e.g. "eth0").
	IfaceID     string `json:"iface_id"`
	// HostDevName is the name of the tap device on the host.
	HostDevName string `json:"host_dev_name"`
}

// Vsock configures the virtio-vsock device for host↔guest communication.
type Vsock struct {
	// GuestCID is the vsock context ID assigned to the guest (must be >= 3).
	GuestCID uint32 `json:"guest_cid"`
	// UDSPath is the host-side Unix socket Firecracker uses for vsock emulation.
	UDSPath string `json:"uds_path"`
}

// Action names accepted by PUT /actions.
const (
	ActionInstanceStart = "InstanceStart"
	ActionSendCtrlAltDel = "SendCtrlAltDel"
)

// PutMachineConfig calls PUT /machine-config to set vCPU and memory.
func (c *Client) PutMachineConfig(ctx context.Context, cfg MachineConfig) error {
	return c.put(ctx, "/machine-config", cfg)
}

// PutBootSource calls PUT /boot-source to set the kernel and boot args.
func (c *Client) PutBootSource(ctx context.Context, src BootSource) error {
	return c.put(ctx, "/boot-source", src)
}

// PutDrive calls PUT /drives/{drive_id} to attach a block device.
func (c *Client) PutDrive(ctx context.Context, drive Drive) error {
	return c.put(ctx, fmt.Sprintf("/drives/%s", drive.DriveID), drive)
}

// PutNetworkInterface calls PUT /network-interfaces/{iface_id}.
func (c *Client) PutNetworkInterface(ctx context.Context, iface NetworkInterface) error {
	return c.put(ctx, fmt.Sprintf("/network-interfaces/%s", iface.IfaceID), iface)
}

// PutVsock calls PUT /vsock to attach a virtio-vsock device to the guest.
func (c *Client) PutVsock(ctx context.Context, vsock Vsock) error {
	return c.put(ctx, "/vsock", vsock)
}

// StartInstance calls PUT /actions with InstanceStart to boot the VM.
// This call returns once the guest kernel has started — not once userspace is ready.
func (c *Client) StartInstance(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]string{"action_type": ActionInstanceStart})
}

// SnapshotCreate calls PUT /snapshot/create to dump a full VM snapshot to disk.
// snapshotPath and memPath are absolute paths on the host.
func (c *Client) SnapshotCreate(ctx context.Context, snapshotPath, memPath string) error {
	body := map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": snapshotPath,
		"mem_file_path": memPath,
	}
	return c.put(ctx, "/snapshot/create", body)
}

// SnapshotLoad calls PUT /snapshot/load to restore a VM from a snapshot.
func (c *Client) SnapshotLoad(ctx context.Context, snapshotPath, memPath string) error {
	body := map[string]any{
		"snapshot_path": snapshotPath,
		"mem_backend": map[string]any{
			"backend_type": "File",
			"backend_path": memPath,
		},
		"enable_diff_snapshots": true,
	}
	return c.put(ctx, "/snapshot/load", body)
}

// put is a helper that sends a PUT request to the Firecracker API socket.
func (c *Client) put(ctx context.Context, path string, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("firecracker: marshal %s: %w", path, err)
	}

	// Firecracker uses "http://localhost" as the nominal URL for its Unix socket.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("firecracker: new request %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker: PUT %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker: PUT %s returned %d: %s", path, resp.StatusCode, string(respBody))
	}
	return nil
}
