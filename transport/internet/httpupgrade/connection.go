package httpupgrade

import "net"

type connection struct {
	net.Conn
	remoteAddr net.Addr
	headers    map[string]string
}

func newConnection(conn net.Conn, remoteAddr net.Addr, headers map[string]string) *connection {
	return &connection{
		Conn:       conn,
		remoteAddr: remoteAddr,
		headers:    headers,
	}
}

func (c *connection) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *connection) Headers() map[string]string {
	return c.headers
}
