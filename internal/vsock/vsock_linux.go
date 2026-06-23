// Layer: host-agent internal — Linux vsock dial implementation.
// Uses github.com/mdlayher/vsock (same approach as firecracker-go-sdk).
//go:build linux

package vsock

import (
	"fmt"
	"net"

	"github.com/mdlayher/vsock"
)

// dialVsock opens a streaming AF_VSOCK connection to (cid, port).
func dialVsock(cid uint32, port uint32) (net.Conn, error) {
	conn, err := vsock.Dial(cid, port, nil)
	if err != nil {
		return nil, fmt.Errorf("vsock connect cid=%d port=%d: %w (try: sudo modprobe vhost_vsock && ls /dev/vhost-vsock)", cid, port, err)
	}
	return conn, nil
}
