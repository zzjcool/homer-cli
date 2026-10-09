package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
)

func TestHelloAdoptsHubHeartbeatSettings(t *testing.T) {
	const (
		localInterval      = 11 * time.Second
		localTimeout       = 22 * time.Second
		negotiatedInterval = 3 * time.Second
		negotiatedTimeout  = 7 * time.Second
	)
	ctx, cancel := context.WithCancel(context.Background())
	clock := streamtest.NewFakeClock(time.Unix(0, 0))
	d := New(Config{
		Home: t.TempDir(), HubURL: "http://hub.example", AgentID: "heartbeat-negotiation",
		Stream: stream.Options{
			Clock: clock, PingInterval: localInterval, PingTimeout: localTimeout,
		},
	}, &testExecutor{})
	agentConn, hubConn := streamtest.Pipe(streamtest.PipeOptions{})
	recorded := &heartbeatRecordingConn{Conn: agentConn, pings: make(chan *stream.Frame, 8)}
	agentSession := stream.NewSession(recorded, d.cfg.Stream)
	agentDone := make(chan error, 1)
	agentExited := make(chan struct{})
	go func() {
		agentDone <- agentSession.Run(ctx)
		close(agentExited)
	}()
	peerDone := make(chan error, 1)
	peerExited := make(chan struct{})
	go func() {
		peerDone <- writeWelcomeWithHeartbeat(ctx, hubConn, negotiatedInterval, negotiatedTimeout)
		close(peerExited)
	}()
	t.Cleanup(func() {
		agentSession.Close(stream.CloseNormal, "test complete")
		_ = hubConn.CloseNow()
		cancel()
		select {
		case <-agentExited:
		case <-time.After(time.Second):
			t.Error("agent session did not stop")
		}
		select {
		case <-peerExited:
		case <-time.After(time.Second):
			t.Error("hello peer did not stop")
		}
	})

	if err := d.hello(ctx, agentSession); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := <-peerDone; err != nil {
		t.Fatalf("fake hub welcome: %v", err)
	}
	// SetHeartbeat wakes the ping loop immediately; after this first ping the
	// negotiated three-second interval, rather than the local eleven-second
	// setting, controls the next ping.
	waitHeartbeatPing(t, recorded.pings)
	waitHeartbeatTimer(t, clock)
	clock.Advance(2 * time.Second)
	waitHeartbeatTimer(t, clock)
	select {
	case frame := <-recorded.pings:
		t.Fatalf("heartbeat ping arrived before the negotiated interval: %+v", frame)
	default:
	}
	clock.Advance(time.Second)
	waitHeartbeatPing(t, recorded.pings)
	waitHeartbeatTimer(t, clock)

	// The fake hub intentionally sends no more frames. A close at the hub's
	// seven-second timeout (rather than the local twenty-two-second timeout)
	// proves the welcome timeout was adopted as well.
	clock.Advance(4*time.Second + time.Nanosecond)
	select {
	case <-agentSession.Done():
	case <-time.After(time.Second):
		t.Fatal("session did not apply the negotiated ping timeout")
	}
	var closeErr *stream.CloseError
	if err := agentSession.Err(); !errors.As(err, &closeErr) || closeErr.Code != stream.CloseHeartbeat {
		t.Fatalf("session error = %v, want heartbeat timeout after hub welcome settings", err)
	}
}

type heartbeatRecordingConn struct {
	stream.Conn
	pings chan *stream.Frame
}

func (c *heartbeatRecordingConn) Write(ctx context.Context, message []byte) error {
	frame, err := stream.DecodeFrame(message, stream.DefaultMaxFrame)
	if err == nil && frame.T == stream.TPing {
		select {
		case c.pings <- frame:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c.Conn.Write(ctx, message)
}

func writeWelcomeWithHeartbeat(ctx context.Context, conn stream.Conn, interval, timeout time.Duration) error {
	message, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	request, err := stream.DecodeFrame(message, stream.DefaultMaxFrame)
	if err != nil {
		return err
	}
	if request.T != stream.TReq || request.M != hub.MethodHello {
		return errors.New("agent did not send a hello request")
	}
	var hello hub.HelloParams
	if err := json.Unmarshal(request.P, &hello); err != nil {
		return err
	}
	welcome, err := json.Marshal(hub.WelcomeResult{
		Proto: hello.Proto, PingIntervalMs: int(interval.Milliseconds()), PingTimeoutMs: int(timeout.Milliseconds()),
	})
	if err != nil {
		return err
	}
	response, err := stream.EncodeFrame(&stream.Frame{T: stream.TRes, ID: request.ID, OK: true, P: welcome}, stream.DefaultMaxFrame)
	if err != nil {
		return err
	}
	return conn.Write(ctx, response)
}

func waitHeartbeatPing(t *testing.T, pings <-chan *stream.Frame) {
	t.Helper()
	select {
	case frame := <-pings:
		if frame.T != stream.TPing {
			t.Fatalf("recorded heartbeat frame = %+v, want ping", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("session did not send its heartbeat ping")
	}
}

func waitHeartbeatTimer(t *testing.T, clock *streamtest.FakeClock) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if clock.PendingTimers() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("heartbeat loop did not schedule a FakeClock timer")
}
