package singmux

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/xtaci/smux"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/pipe"
)

type ClientStrategy struct {
	MaxConcurrency uint32
}

type ClientFactory struct {
	Proxy  proxy.Outbound
	Dialer internet.Dialer
}

type muxClientSession interface {
	OpenStream() (net.Conn, error)
	Close() error
	IsClosed() bool
}

type yamuxClientSession struct {
	*yamux.Session
}

func (s *yamuxClientSession) OpenStream() (net.Conn, error) {
	return s.Session.Open()
}

type smuxClientSession struct {
	*smux.Session
}

func (s *smuxClientSession) OpenStream() (net.Conn, error) {
	stream, err := s.Session.OpenStream()
	if err != nil {
		return nil, err
	}
	return stream, nil
}

type ClientManager struct {
	Enabled  bool
	Protocol string
	Factory  *ClientFactory
	Strategy ClientStrategy
	session  muxClientSession
	conn     *transport.Link
	mu       sync.Mutex
}

func (m *ClientManager) Dispatch(ctx context.Context, link *transport.Link) error {
	if !m.Enabled {
		return errors.New("singmux not enabled")
	}
	sess, err := m.getOrCreateSession()
	if err != nil {
		return err
	}

	stream, err := sess.OpenStream()
	if err != nil {
		return errors.New("failed to open mux stream").Base(err)
	}

	ob := session.OutboundsFromContext(ctx)
	dest := ob[len(ob)-1].Target

	if err := WriteStreamRequest(stream, dest); err != nil {
		stream.Close()
		return errors.New("failed to write stream request").Base(err)
	}

	if err := ReadStreamResponse(stream); err != nil {
		stream.Close()
		return errors.New("stream rejected by server").Base(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if err := CopyBufReaderToWriter(stream, link.Reader); err != nil {
			if !isNormalClose(err) {
				errors.LogInfo(ctx, "singmux client uplink copy error: ", err)
			}
		}
	}()

	go func() {
		defer wg.Done()
		if err := CopyReaderToBufWriter(link.Writer, stream); err != nil {
			if !isNormalClose(err) {
				errors.LogInfo(ctx, "singmux client downlink copy error: ", err)
			}
		}
	}()

	wg.Wait()
	common.Close(link.Writer)
	return nil
}

func (m *ClientManager) getOrCreateSession() (muxClientSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.session != nil && !m.session.IsClosed() {
		return m.session, nil
	}

	opts := []pipe.Option{pipe.WithSizeLimit(64 * 1024)}
	uplinkReader, uplinkWriter := pipe.New(opts...)
	downlinkReader, downlinkWriter := pipe.New(opts...)

	m.conn = &transport.Link{
		Reader: uplinkReader,
		Writer: downlinkWriter,
	}
	proxyLink := &transport.Link{
		Reader: downlinkReader,
		Writer: uplinkWriter,
	}

	conn := NewClientLinkConn(m.conn)

	outbounds := []*session.Outbound{{
		Target: SingMuxAddr,
	}}
	proxyCtx := session.ContextWithOutbounds(context.Background(), outbounds)
	proxyCtx, cancel := context.WithCancel(proxyCtx)

	go func() {
		if err := m.Factory.Proxy.Process(proxyCtx, proxyLink, m.Factory.Dialer); err != nil {
			errors.LogInfoInner(proxyCtx, err, "singmux proxy connection closed")
		}
		cancel()
	}()

	var protocol byte
	switch m.Protocol {
	case "smux":
		protocol = ProtocolSmux
	case "yamux":
		protocol = ProtocolYAMux
	default:
		common.Interrupt(m.conn.Reader)
		common.Interrupt(m.conn.Writer)
		return nil, errors.New("unknown singmux protocol: ", m.Protocol)
	}

	if err := WriteHandshake(conn, &HandshakeRequest{Version: Version0, Protocol: protocol}); err != nil {
		common.Interrupt(m.conn.Reader)
		common.Interrupt(m.conn.Writer)
		return nil, errors.New("failed to write singmux handshake").Base(err)
	}

	switch protocol {
	case ProtocolSmux:
		cfg := smux.DefaultConfig()
		cfg.KeepAliveDisabled = true
		ss, err := smux.Client(conn, cfg)
		if err != nil {
			common.Interrupt(m.conn.Reader)
			common.Interrupt(m.conn.Writer)
			return nil, errors.New("failed to create smux client").Base(err)
		}
		m.session = &smuxClientSession{ss}

	case ProtocolYAMux:
		cfg := yamux.DefaultConfig()
		cfg.EnableKeepAlive = true
		cfg.KeepAliveInterval = 30 * time.Second
		cfg.LogOutput = io.Discard
		ys, err := yamux.Client(conn, cfg)
		if err != nil {
			common.Interrupt(m.conn.Reader)
			common.Interrupt(m.conn.Writer)
			return nil, errors.New("failed to create yamux client").Base(err)
		}
		m.session = &yamuxClientSession{ys}
	}

	go func() {
		<-proxyCtx.Done()
		if m.session != nil {
			m.session.Close()
		}
	}()

	return m.session, nil
}

func (m *ClientManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session != nil {
		m.session.Close()
		m.session = nil
	}
	if m.conn != nil {
		common.Interrupt(m.conn.Reader)
		common.Interrupt(m.conn.Writer)
		m.conn = nil
	}
	return nil
}
