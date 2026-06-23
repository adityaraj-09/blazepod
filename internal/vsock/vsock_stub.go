// Layer: host-agent internal — vsock stub for non-Linux builds.
//go:build !linux

package vsock

import (
	"fmt"
	"net"
)

func dialFirecracker(udsPath string, port uint32) (net.Conn, error) {
	return nil, fmt.Errorf("vsock: firecracker UDS dial requires Linux (uds=%s port=%d)", udsPath, port)
}
