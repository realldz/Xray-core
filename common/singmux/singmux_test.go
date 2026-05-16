package singmux

import (
	"context"
	"io"
	stdnet "net"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
)

func TestIsNormalClose(t *testing.T) {
	if !isNormalClose(io.EOF) {
		t.Fatal("io.EOF should be normal close")
	}
	if !isNormalClose(io.ErrClosedPipe) {
		t.Fatal("io.ErrClosedPipe should be normal close")
	}
	if isNormalClose(io.ErrUnexpectedEOF) {
		t.Fatal("io.ErrUnexpectedEOF should NOT be normal close")
	}
}

func TestCopyBufReaderToWriterEOF(t *testing.T) {
	pr, pw := pipe.New()

	common.Close(pw)

	err := CopyBufReaderToWriter(&discardWriter{}, pr)
	if err != nil {
		t.Fatalf("CopyBufReaderToWriter returned non-nil on clean close: %v", err)
	}
}

func TestCopyReaderToBufWriterEOF(t *testing.T) {
	ir, iw := io.Pipe()
	iw.Close()

	_, pw := pipe.New()

	err := CopyReaderToBufWriter(pw, ir)
	if err != nil {
		t.Fatalf("CopyReaderToBufWriter returned non-nil on clean EOF: %v", err)
	}
	common.Close(pw)
}

func TestCopyReaderToBufWriterWriteError(t *testing.T) {
	ir, iw := io.Pipe()
	defer iw.Close()

	_, pw := pipe.New()

	common.Interrupt(pw)
	time.Sleep(10 * time.Millisecond)
	go io.WriteString(iw, "hello world")

	err := CopyReaderToBufWriter(pw, ir)
	if err == nil {
		t.Fatal("expected error on interrupted writer")
	}
	common.Close(pw)
}

func TestStreamContextIndependentOfParentCancel(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	inbound := &session.Inbound{
		Tag:    "test-in",
		Source: net.TCPDestination(net.IPAddress([]byte{192, 168, 1, 1}), net.Port(12345)),
	}
	ctx := session.ContextWithInbound(parentCtx, inbound)
	subCtx := session.ContextWithOutbounds(context.Background(), []*session.Outbound{})

	if streamInbound := session.InboundFromContext(ctx); streamInbound != nil {
		inCopy := *streamInbound
		subCtx = session.ContextWithInbound(subCtx, &inCopy)
	}

	parentCancel()

	select {
	case <-subCtx.Done():
		t.Fatal("subCtx should NOT inherit parent cancellation")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestStreamInboundIsolated(t *testing.T) {
	parentCtx := context.Background()

	inbound := &session.Inbound{
		Tag:    "test-in",
		Source: net.TCPDestination(net.IPAddress([]byte{192, 168, 1, 1}), net.Port(12345)),
	}
	ctx := session.ContextWithInbound(parentCtx, inbound)
	subCtx := session.ContextWithOutbounds(ctx, []*session.Outbound{})

	if streamInbound := session.InboundFromContext(ctx); streamInbound != nil {
		inCopy := *streamInbound
		subCtx = session.ContextWithInbound(subCtx, &inCopy)
	}

	inbound.Tag = "modified"

	got := session.InboundFromContext(subCtx)
	if got.Tag != "test-in" {
		t.Fatalf("inbound tag should remain 'test-in', got '%s'", got.Tag)
	}
}

func TestSubContextOutboundChainResets(t *testing.T) {
	parentCtx := context.Background()
	originalOB := []*session.Outbound{{
		Target: net.TCPDestination(net.DomainAddress("original.addr"), net.Port(443)),
		Tag:    "old-out",
	}}
	ctx := session.ContextWithOutbounds(parentCtx, originalOB)
	subCtx := session.ContextWithOutbounds(ctx, []*session.Outbound{{}})

	got := session.OutboundsFromContext(subCtx)
	if len(got) != 1 {
		t.Fatalf("expected 1 fresh outbound, got %d", len(got))
	}
}

func TestHandleStreamCopyErrorDoesNotPanic(t *testing.T) {
	pr, pw := pipe.New()
	s := &Server{
		dispatcher: &fakeDispatch{link: &transport.Link{
			Reader: pr,
			Writer: pw,
		}},
	}

	inbound := &session.Inbound{Tag: "test"}
	ctx := session.ContextWithInbound(context.Background(), inbound)

	stream := &testStream{
		readData: []byte{},
		readErr:  io.EOF,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.handleStream(ctx, stream)
	}()
	wg.Wait()
}

func TestHandleStreamNormalCloseWriter(t *testing.T) {
	pr2, pw2 := pipe.New()
	s := &Server{
		dispatcher: &fakeDispatch{link: &transport.Link{
			Reader: pr2,
			Writer: pw2,
		}},
	}

	inbound := &session.Inbound{Tag: "test"}
	ctx := session.ContextWithInbound(context.Background(), inbound)

	data := []byte{0x00, 0x02, 0x00, 0x03, 'g', 'o', 'o', 0x01, 0xbb}
	pos := 0
	stream := &testStream{
		readFn: func(b []byte) (int, error) {
			if pos >= len(data) {
				return 0, io.EOF
			}
			n := copy(b, data[pos:])
			pos += n
			return n, nil
		},
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.handleStream(ctx, stream)
	}()
	wg.Wait()
}

type fakeDispatch struct {
	link *transport.Link
}

func (d *fakeDispatch) Type() interface{} { return nil }
func (d *fakeDispatch) Start() error      { return nil }
func (d *fakeDispatch) Close() error      { return nil }
func (d *fakeDispatch) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
	return d.link, nil
}
func (d *fakeDispatch) DispatchLink(ctx context.Context, dest net.Destination, link *transport.Link) error {
	return nil
}

type discardWriter struct{}

func (w *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardWriter) Close() error                 { return nil }

type testStream struct {
	readData []byte
	readErr  error
	readFn   func([]byte) (int, error)
	writeErr error
}

func (s *testStream) Read(b []byte) (int, error) {
	if s.readFn != nil {
		return s.readFn(b)
	}
	if len(s.readData) > 0 {
		n := copy(b, s.readData)
		s.readData = s.readData[n:]
		return n, nil
	}
	return 0, s.readErr
}
func (s *testStream) Write(b []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return len(b), nil
}
func (s *testStream) Close() error                        { return nil }
func (s *testStream) LocalAddr() stdnet.Addr               { return &singMuxAddr{network: "tcp", addr: "127.0.0.1:1"} }
func (s *testStream) RemoteAddr() stdnet.Addr              { return &singMuxAddr{network: "tcp", addr: "127.0.0.1:2"} }
func (s *testStream) SetDeadline(t time.Time) error       { return nil }
func (s *testStream) SetReadDeadline(t time.Time) error   { return nil }
func (s *testStream) SetWriteDeadline(t time.Time) error  { return nil }
