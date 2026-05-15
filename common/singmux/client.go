package singmux

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
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

type ClientManager struct {
	Enabled  bool
	Factory  *ClientFactory
	Strategy ClientStrategy
	session  *yamux.Session
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
		return errors.New("failed to open yamux stream").Base(err)
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
			common.Interrupt(link.Writer)
		}
	}()

	go func() {
		defer wg.Done()
		if err := CopyReaderToBufWriter(link.Writer, stream); err != nil {
			common.Interrupt(link.Reader)
		}
	}()

	wg.Wait()
	return nil
}

func (m *ClientManager) getOrCreateSession() (*yamux.Session, error) {
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

	if err := WriteHandshake(conn, &HandshakeRequest{Version: Version0, Protocol: ProtocolYAMux}); err != nil {
		common.Interrupt(m.conn.Reader)
		common.Interrupt(m.conn.Writer)
		return nil, errors.New("failed to write singmux handshake").Base(err)
	}

	config := yamux.DefaultConfig()
	config.EnableKeepAlive = true
	config.KeepAliveInterval = 30 * time.Second
	config.LogOutput = io.Discard
	sess, err := yamux.Client(conn, config)
	if err != nil {
		common.Interrupt(m.conn.Reader)
		common.Interrupt(m.conn.Writer)
		return nil, errors.New("failed to create yamux client").Base(err)
	}

	m.session = sess
	go func() {
		<-proxyCtx.Done()
		m.session.Close()
	}()

	return sess, nil
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
