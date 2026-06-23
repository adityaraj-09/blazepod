// Layer: host-agent internal — vsock stub for non-Linux builds.
// Allows the package to compile on macOS/Windows for development and unit testing.
// Any call to dialVsock on these platforms returns an error at runtime.
//go:build !linux

package vsock

import (
	"fmt"
	"net"
)

func dialVsock(cid uint32, port uint32) (net.Conn, error) {
	return nil, fmt.Errorf("vsock: AF_VSOCK is only available on Linux with KVM; current OS does not support it")
}
