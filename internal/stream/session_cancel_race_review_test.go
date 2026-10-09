package stream_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
)

func TestCancelWinsBeforeHandlerReturnsNoResponse(t *testing.T) {
	peer, sessionConn := streamtest.Pipe(streamtest.PipeOptions{Buffer: 8})
	recorded := newRecordingConn(t, sessionConn, 8)
	session := stream.NewSession(recorded, quietSessionOptions())
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	started := make(chan struct{})
	cancelObserved := make(chan struct{})
	releaseHandler := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseHandler) }) }
	handlerReturned := make(chan struct{})
	resultMarshaled := make(chan struct{}, 1)
	session.Handle("cancel-race", func(handlerCtx context.Context, _ *stream.Request) (any, error) {
		close(started)
		<-handlerCtx.Done()
		close(cancelObserved)
		<-releaseHandler
		defer close(handlerReturned)
		return observingResult{marshaled: resultMarshaled, value: `"late"`}, nil
	})
	go func() { runDone <- session.Run(ctx) }()
	t.Cleanup(func() {
		release()
		session.Close(stream.CloseNormal, "test cleanup")
		cancel()
		_ = peer.CloseNow()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Error("session did not stop")
		}
	})

	writeTestFrame(t, peer, &stream.Frame{T: stream.TReq, ID: "cancel-first", M: "cancel-race", P: json.RawMessage(`null`)})
	waitTestSignal(t, started, "handler start")
	writeTestFrame(t, peer, &stream.Frame{T: stream.TCancel, ID: "cancel-first"})
	waitTestSignal(t, cancelObserved, "cancel frame to cancel handler context")
	release()
	waitTestSignal(t, handlerReturned, "handler return after cancel")
	assertNoWireFrame(t, peer, recorded, 100*time.Millisecond, "canceled handler response")
	select {
	case <-resultMarshaled:
		t.Fatal("handler result was encoded after cancel won")
	default:
	}
}

func TestHandlerCompletionWinsBeforeCancelExactlyOneTerminal(t *testing.T) {
	peer, sessionConn := streamtest.Pipe(streamtest.PipeOptions{Buffer: 8})
	recorded := newRecordingConn(t, sessionConn, 8)
	session := stream.NewSession(recorded, quietSessionOptions())
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	marshalStarted := make(chan struct{})
	releaseMarshal := make(chan struct{})
	var releaseMarshalOnce sync.Once
	release := func() { releaseMarshalOnce.Do(func() { close(releaseMarshal) }) }
	cancelProcessed := make(chan struct{})
	session.Handle("complete-race", func(context.Context, *stream.Request) (any, error) {
		return blockingResult{started: marshalStarted, release: releaseMarshal}, nil
	})
	// The read loop handles frames in order. Observing this event therefore
	// proves the preceding cancel frame has already been processed.
	session.OnEvent("cancel-barrier", func(context.Context, string, json.RawMessage) {
		close(cancelProcessed)
	})
	go func() { runDone <- session.Run(ctx) }()
	t.Cleanup(func() {
		release()
		session.Close(stream.CloseNormal, "test cleanup")
		cancel()
		_ = peer.CloseNow()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Error("session did not stop")
		}
	})

	writeTestFrame(t, peer, &stream.Frame{T: stream.TReq, ID: "complete-first", M: "complete-race", P: json.RawMessage(`null`)})
	waitTestSignal(t, marshalStarted, "handler completion to win its terminal CAS")
	writeTestFrame(t, peer, &stream.Frame{T: stream.TCancel, ID: "complete-first"})
	writeTestFrame(t, peer, &stream.Frame{T: stream.TEvt, M: "cancel-barrier", P: json.RawMessage(`{}`)})
	waitTestSignal(t, cancelProcessed, "cancel to be ignored after handler completion")
	release()

	frame := readTestFrame(t, peer)
	if frame.T != stream.TRes || frame.ID != "complete-first" || !frame.OK {
		t.Fatalf("terminal frame = %+v, want one successful res for complete-first", frame)
	}
	recordedFrame := readRecordedFrame(t, recorded)
	if recordedFrame.T != frame.T || recordedFrame.ID != frame.ID || !recordedFrame.OK {
		t.Fatalf("recorded terminal frame = %+v, wire frame = %+v", recordedFrame, frame)
	}
	assertNoWireFrame(t, peer, recorded, 100*time.Millisecond, "duplicate terminal response")
}

func quietSessionOptions() stream.Options {
	return stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour}
}

type recordingConn struct {
	stream.Conn
	t      *testing.T
	writes chan []byte
}

func newRecordingConn(t *testing.T, conn stream.Conn, capacity int) *recordingConn {
	t.Helper()
	return &recordingConn{Conn: conn, t: t, writes: make(chan []byte, capacity)}
}

func (c *recordingConn) Write(ctx context.Context, message []byte) error {
	copyOfMessage := append([]byte(nil), message...)
	select {
	case c.writes <- copyOfMessage:
	default:
		// A recorder must never silently discard a wire frame when its buffer
		// is full: that would turn an overloaded test into a false assertion.
		c.t.Errorf("recordingConn write buffer full; dropped frame: %s", copyOfMessage)
	}
	return c.Conn.Write(ctx, message)
}

type observingResult struct {
	marshaled chan struct{}
	value     string
}

func (r observingResult) MarshalJSON() ([]byte, error) {
	select {
	case r.marshaled <- struct{}{}:
	default:
	}
	return []byte(r.value), nil
}

type blockingResult struct {
	started chan struct{}
	release <-chan struct{}
}

func (r blockingResult) MarshalJSON() ([]byte, error) {
	close(r.started)
	<-r.release
	return []byte(`{"completed":true}`), nil
}

func writeTestFrame(t *testing.T, conn stream.Conn, frame *stream.Frame) {
	t.Helper()
	payload, err := stream.EncodeFrame(frame, stream.DefaultMaxFrame)
	if err != nil {
		t.Fatalf("EncodeFrame(%s): %v", frame.T, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, payload); err != nil {
		t.Fatalf("write %s frame: %v", frame.T, err)
	}
}

func waitTestSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func readTestFrame(t *testing.T, conn stream.Conn) *stream.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read terminal frame: %v", err)
	}
	frame, err := stream.DecodeFrame(payload, stream.DefaultMaxFrame)
	if err != nil {
		t.Fatalf("decode terminal frame: %v", err)
	}
	return frame
}

func readRecordedFrame(t *testing.T, recorder *recordingConn) *stream.Frame {
	t.Helper()
	select {
	case payload := <-recorder.writes:
		frame, err := stream.DecodeFrame(payload, stream.DefaultMaxFrame)
		if err != nil {
			t.Fatalf("decode recorded frame: %v", err)
		}
		return frame
	case <-time.After(time.Second):
		t.Fatal("recording connection did not capture the wire frame")
		return nil
	}
}

func assertNoWireFrame(t *testing.T, peer stream.Conn, recorder *recordingConn, duration time.Duration, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	if payload, err := peer.Read(ctx); err == nil {
		frame, decodeErr := stream.DecodeFrame(payload, stream.DefaultMaxFrame)
		t.Fatalf("unexpected %s on wire: frame=%+v decodeErr=%v", what, frame, decodeErr)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read while waiting for %s = %v, want no frame until context deadline", what, err)
	}
	select {
	case payload := <-recorder.writes:
		frame, err := stream.DecodeFrame(payload, stream.DefaultMaxFrame)
		if err != nil {
			t.Fatalf("recorded frame while waiting for %s was malformed: %v", what, err)
		}
		t.Fatalf("recorded unexpected %s: %+v", what, frame)
	default:
	}
}

var _ json.Marshaler = observingResult{}
var _ json.Marshaler = blockingResult{}
