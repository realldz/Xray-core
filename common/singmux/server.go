package singmux

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/xtaci/smux"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
)

// Server implements routing.Dispatcher, wrapping mux sessions.
// It intercepts connections destined for sp.mux.sing-box.arpa:444
// and demuxes them into individual streams dispatched to the wrapped dispatcher.
type Server struct {
	dispatcher routing.Dispatcher
}

type muxServerSession interface {
	Accept() (net.Conn, error)
	Close() error
}

type smuxServerSession struct {
	*smux.Session
}

func (s *smuxServerSession) Accept() (net.Conn, error) {
	stream, err := s.Session.AcceptStream()
	return stream, err
}

func NewServer(ctx context.Context, fallback routing.Dispatcher) *Server {
	return &Server{dispatcher: fallback}
}

func (s *Server) Type() interface{} {
	return routing.DispatcherType()
}

func (s *Server) Close() error {
	if c, ok := s.dispatcher.(common.Closable); ok {
		return c.Close()
	}
	return nil
}

func (s *Server) Start() error {
	return nil
}

func (s *Server) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
	if !IsSingMuxAddress(dest) {
		return s.dispatcher.Dispatch(ctx, dest)
	}

	opts := pipe.OptionsFromContext(ctx)
	uplinkReader, uplinkWriter := pipe.New(opts...)
	downlinkReader, downlinkWriter := pipe.New(opts...)

	muxLink := &transport.Link{
		Reader: uplinkReader,
		Writer: downlinkWriter,
	}
	clientLink := &transport.Link{
		Reader: downlinkReader,
		Writer: uplinkWriter,
	}

	if err := s.startMuxWorker(ctx, muxLink); err != nil {
		return nil, err
	}

	return clientLink, nil
}

func (s *Server) DispatchLink(ctx context.Context, dest net.Destination, link *transport.Link) error {
	if !IsSingMuxAddress(dest) {
		return s.dispatcher.DispatchLink(ctx, dest, link)
	}
	return s.startMuxWorker(ctx, link)
}

func (s *Server) startMuxWorker(ctx context.Context, link *transport.Link) error {
	conn := NewServerLinkConn(link)

	go func() {
		if err := s.runMuxSession(ctx, conn); err != nil {
			errors.LogInfo(ctx, "singmux session ended: ", err)
		}
	}()
	return nil
}

func (s *Server) runMuxSession(ctx context.Context, conn io.ReadWriteCloser) error {
	req, err := ReadHandshake(conn)
	if err != nil {
		return errors.New("failed to read singmux handshake").Base(err)
	}

	var sess muxServerSession
	switch req.Protocol {
	case ProtocolSmux:
		cfg := smux.DefaultConfig()
		cfg.KeepAliveDisabled = true
		ss, err := smux.Server(conn, cfg)
		if err != nil {
			return errors.New("failed to create smux server").Base(err)
		}
		sess = &smuxServerSession{ss}

	case ProtocolYAMux:
		cfg := yamux.DefaultConfig()
		cfg.EnableKeepAlive = true
		cfg.KeepAliveInterval = 30 * time.Second
		cfg.StreamCloseTimeout = 5 * time.Second
		cfg.StreamOpenTimeout = 5 * time.Second
		cfg.LogOutput = io.Discard
		ys, err := yamux.Server(conn, cfg)
		if err != nil {
			return errors.New("failed to create yamux server").Base(err)
		}
		sess = ys

	default:
		return errors.New("unsupported singmux protocol: ", req.Protocol)
	}
	defer sess.Close()

	for {
		stream, err := sess.Accept()
		if err != nil {
			return errors.New("mux accept error").Base(err)
		}
		go s.handleStream(ctx, stream)
	}
}

func isNormalClose(err error) bool {
	return err == io.EOF || err == io.ErrClosedPipe
}

func (s *Server) handleStream(ctx context.Context, stream net.Conn) {
	id := fmt.Sprintf("%d", time.Now().UnixNano()) // stream-local id for logging
	defer stream.Close()

	dest, err := ReadStreamRequest(stream)
	if err != nil {
		errors.LogInfo(ctx, "[singmux stream ", id, "] failed to read stream request: ", err)
		return
	}

	errors.LogInfo(ctx, "[singmux stream ", id, "] dest from stream request: ", dest)

	if err := WriteStreamResponse(stream); err != nil {
		errors.LogInfo(ctx, "[singmux stream ", id, "] failed to write stream response: ", err)
		return
	}

	// Build stream-local context independent of parent cancellation
	// to prevent cascading cancel from killing active dispatch streams.
	subCtx := session.ContextWithOutbounds(context.Background(), []*session.Outbound{{}})

	streamInbound := session.InboundFromContext(ctx)
	if streamInbound != nil {
		inCopy := *streamInbound
		subCtx = session.ContextWithInbound(subCtx, &inCopy)
	}

	if content := session.ContentFromContext(ctx); content != nil {
		newContent := session.Content{
			SkipDNSResolve: content.SkipDNSResolve,
		}
		subCtx = session.ContextWithContent(subCtx, &newContent)
	}

	errors.LogInfo(ctx, "[singmux stream ", id, "] dispatching to: ", dest)

	link, err := s.dispatcher.Dispatch(subCtx, dest)
	if err != nil {
		errors.LogInfo(ctx, "[singmux stream ", id, "] dispatch failed: ", err)
		return
	}

	errors.LogInfo(ctx, "[singmux stream ", id, "] forwarding data")

	var (
		downWg        sync.WaitGroup
		uplinkBytes   int64
		downlinkBytes int64
		uplinkErr     error
		downlinkErr   error
	)
	downWg.Add(1)

	// Downlink: remote server → link.Reader → yamux stream → client
	go func() {
		defer downWg.Done()
		cw := &countingWriter{w: stream}
		err := CopyBufReaderToWriter(cw, link.Reader)
		downlinkBytes = cw.n
		if err != nil && !isNormalClose(err) {
			downlinkErr = err
			errors.LogInfo(ctx, "[singmux stream ", id, "] downlink copy error: ", err)
		}
		// Half-close yamux stream write side: signal sing-box that response is complete.
		// This sends FIN to the client, letting sing-box propagate the complete
		// HTTP response to the browser. Without this, the browser waits for data
		// that will never arrive.
		common.Close(link.Writer)
		stream.Close()
	}()

	// Uplink: client → yamux stream → link.Writer → remote server
	go func() {
		cr := &countingReader{r: stream}
		err := CopyReaderToBufWriter(link.Writer, cr)
		uplinkBytes = cr.n
		if err != nil && !isNormalClose(err) {
			uplinkErr = err
			errors.LogInfo(ctx, "[singmux stream ", id, "] uplink copy error: ", err)
		}
	}()

	downWg.Wait()
	errors.LogInfo(ctx, "[singmux stream ", id, "] downlink done: bytes=", downlinkBytes, " err=", downlinkErr, " uplink: bytes=", uplinkBytes, " err=", uplinkErr)
	errors.LogInfo(ctx, "[singmux stream ", id, "] closed")
}
