// Layer: host-agent internal — Firecracker vsock dial (Linux).
// Firecracker uses a vhost-less vsock device: the host connects to the UDS at
// uds_path, sends "CONNECT <port>\n", reads "OK <port>\n", then uses the same
// connection for application data. AF_VSOCK to guest CID does not work here.
// See: https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md
//go:build linux

package vsock

import (
	"bufio"
	"fmt"
	"net"
	"strings"
)

// dialFirecracker connects to a guest vsock port via Firecracker's uds_path socket.
func dialFirecracker(udsPath string, port uint32) (net.Conn, error) {
	conn, err := net.Dial("unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("dial firecracker vsock uds %s: %w", udsPath, err)
	}

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT handshake write: %w", err)
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT handshake read: %w (is vm-agent listening on port %d?)", err, port)
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT rejected: %q", line)
	}

	return &fcVsockConn{Conn: conn, r: reader}, nil
}

// fcVsockConn wraps the post-handshake connection; reads may be buffered.
type fcVsockConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *fcVsockConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}
