package singmux

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
)

// Server implements routing.Dispatcher, wrapping a yamux.Server.
// It intercepts connections destined for sp.mux.sing-box.arpa:444
// and demuxes them into individual streams dispatched to the wrapped dispatcher.
type Server struct {
	dispatcher routing.Dispatcher
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
	if req.Protocol != ProtocolYAMux {
		return errors.New("unsupported singmux protocol: ", req.Protocol)
	}

	config := yamux.DefaultConfig()
	config.EnableKeepAlive = true
	config.KeepAliveInterval = 30 * time.Second
	config.LogOutput = io.Discard
	sess, err := yamux.Server(conn, config)
	if err != nil {
		return errors.New("failed to create yamux server").Base(err)
	}
	defer sess.Close()

	for {
		stream, err := sess.Accept()
		if err != nil {
			return errors.New("yamux accept error").Base(err)
		}
		go s.handleStream(ctx, stream)
	}
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

	// Build stream-local context — do NOT reuse SubContextFromMuxInbound
	// as it copies SniffingRequest which can bleed across streams.
	subCtx := context.Background()

	// Fresh outbounds so routing chain is clean per stream
	subCtx = session.ContextWithOutbounds(subCtx, []*session.Outbound{{}})

	// Value-copy inbound to avoid sharing mutable pointer across goroutines
	if in := session.InboundFromContext(ctx); in != nil {
		inCopy := *in
		subCtx = session.ContextWithInbound(subCtx, &inCopy)
	}

	// Copy Content only for safe fields
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

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if err := CopyReaderToBufWriter(link.Writer, stream); err != nil {
			common.Interrupt(link.Writer)
		}
	}()

	go func() {
		defer wg.Done()
		if err := CopyBufReaderToWriter(stream, link.Reader); err != nil {
			common.Interrupt(link.Reader)
		}
	}()

	wg.Wait()
	errors.LogInfo(ctx, "[singmux stream ", id, "] closed")
}
