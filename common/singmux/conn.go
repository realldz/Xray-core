package singmux

import (
	"io"
	"net"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/transport"
)

// linkConn adapts a transport.Link (buf.Reader + buf.Writer) to a net.Conn.
// Uses a buf.BufferedReader for proper MultiBuffer→byte stream conversion,
// matching xray-core's own approach for framing-sensitive protocols like yamux.
type linkConn struct {
	link       *transport.Link
	localAddr  net.Addr
	remoteAddr net.Addr
	bufReader  *buf.BufferedReader
}

func (c *linkConn) Read(b []byte) (int, error) {
	if c.bufReader == nil {
		c.bufReader = &buf.BufferedReader{Reader: c.link.Reader}
	}
	return c.bufReader.Read(b)
}

func (c *linkConn) Write(b []byte) (int, error) {
	bb := buf.New()
	bb.Write(b)
	mb := buf.MultiBuffer{bb}
	err := c.link.Writer.WriteMultiBuffer(mb)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *linkConn) Close() error {
	common.Interrupt(c.link.Reader)
	common.Interrupt(c.link.Writer)
	return nil
}

func (c *linkConn) LocalAddr() net.Addr {
	if c.localAddr != nil {
		return c.localAddr
	}
	return &singMuxAddr{network: "tcp", addr: "xray-link-local"}
}

func (c *linkConn) RemoteAddr() net.Addr {
	if c.remoteAddr != nil {
		return c.remoteAddr
	}
	return &singMuxAddr{network: "tcp", addr: "xray-link-remote"}
}

func (c *linkConn) SetDeadline(t time.Time) error      { return nil }
func (c *linkConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *linkConn) SetWriteDeadline(t time.Time) error { return nil }

func NewClientLinkConn(link *transport.Link) net.Conn {
	return &linkConn{link: link}
}

func NewServerLinkConn(link *transport.Link) net.Conn {
	return &linkConn{link: link}
}

func CopyBufReaderToWriter(dst io.Writer, src buf.Reader) error {
	for {
		mb, err := src.ReadMultiBuffer()
		if err != nil {
			if err == io.EOF || err == io.ErrClosedPipe {
				return nil
			}
			return err
		}
		for _, b := range mb {
			if b.Len() > 0 {
				_, err = dst.Write(b.Bytes())
				if err != nil {
					buf.ReleaseMulti(mb)
					return err
				}
			}
		}
		buf.ReleaseMulti(mb)
	}
}

func CopyReaderToBufWriter(dst buf.Writer, src io.Reader) error {
	bufSize := make([]byte, 64*1024)
	for {
		n, err := src.Read(bufSize)
		if n > 0 {
			bb := buf.New()
			bb.Write(bufSize[:n])
			e := dst.WriteMultiBuffer(buf.MultiBuffer{bb})
			if e != nil {
				return e
			}
		}
		if err != nil {
			if err == io.EOF || err == io.ErrClosedPipe {
				return nil
			}
			return err
		}
	}
}

// counting wrappers for debug logging
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}
