// Layer: host-agent internal — Linux vsock dial implementation.
// Uses raw syscall to open AF_VSOCK sockets since the standard library
// does not expose this address family. Build-constrained to Linux only.
//go:build linux

package vsock

import (
	"fmt"
	"net"
	"syscall"
	"time"
	"unsafe"
)

const (
	afVsock       = 40
	sockStream    = 1
	sockaddrVMLen = 16
)

// sockaddrVM mirrors struct sockaddr_vm from <linux/vm_sockets.h>.
type sockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	_         [4]byte
}

// dialVsock opens a streaming AF_VSOCK connection to (cid, port).
func dialVsock(cid uint32, port uint32) (net.Conn, error) {
	fd, err := syscall.Socket(afVsock, sockStream, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}

	sa := sockaddrVM{
		Family: afVsock,
		Port:   port,
		CID:    cid,
	}

	_, _, errno := syscall.Syscall(
		syscall.SYS_CONNECT,
		uintptr(fd),
		uintptr(unsafe.Pointer(&sa)),
		uintptr(sockaddrVMLen),
	)
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("vsock connect cid=%d port=%d: %w", cid, port, errno)
	}

	file := fmt.Sprintf("vsock:%d:%d", cid, port)
	return &vsockConn{fd: fd, name: file}, nil
}

// vsockConn wraps a raw vsock fd as a net.Conn.
type vsockConn struct {
	fd   int
	name string
}

func (c *vsockConn) Read(b []byte) (int, error) {
	n, err := syscall.Read(c.fd, b)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (c *vsockConn) Write(b []byte) (int, error) {
	n, err := syscall.Write(c.fd, b)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (c *vsockConn) Close() error { return syscall.Close(c.fd) }

func (c *vsockConn) LocalAddr() net.Addr  { return vsockAddr(c.name) }
func (c *vsockConn) RemoteAddr() net.Addr { return vsockAddr(c.name) }

func (c *vsockConn) SetDeadline(t time.Time) error      { return nil }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return nil }

type vsockAddr string

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return string(a) }
