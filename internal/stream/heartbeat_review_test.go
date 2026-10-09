package stream_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
)

func TestDefaultHeartbeatContract(t *testing.T) {
	if stream.DefaultPingInterval != 25*time.Second {
		t.Fatalf("DefaultPingInterval = %s, want 25s", stream.DefaultPingInterval)
	}
	if stream.DefaultPingTimeout != 75*time.Second {
		t.Fatalf("DefaultPingTimeout = %s, want 75s", stream.DefaultPingTimeout)
	}
	// P0 measured a completely idle Cloudflare connection closing at about
	// 124s (60s and 100s survived), so the negotiated timeout must stay below
	// the 100s survival point and application pings must keep the link active.
	if stream.DefaultPingTimeout >= 100*time.Second {
		t.Fatalf("DefaultPingTimeout = %s, must remain below the measured 100s limit", stream.DefaultPingTimeout)
	}
}

func TestHeartbeatTimeoutSendsCode4000(t *testing.T) {
	t.Run("deadline check before timer", func(t *testing.T) {
		clock := streamtest.NewFakeClock(time.Unix(0, 0))
		local, peer := streamtest.Pipe(streamtest.PipeOptions{})
		session := stream.NewSession(local, stream.Options{
			Clock: clock, PingInterval: 100 * time.Second, PingTimeout: 100 * time.Second,
		})
		done := make(chan error, 1)
		go func() { done <- session.Run(context.Background()) }()
		waitForFakeTimer(t, clock)

		// Keep the original 100s timer pending, then configure a shorter timeout
		// and wake pingLoop. This reaches its pre-wait overdue check separately
		// from the timer-expiry path.
		clock.Advance(6 * time.Second)
		session.SetHeartbeat(100*time.Second, 5*time.Second)
		assertHeartbeatCloseReceived(t, peer, done)
	})

	t.Run("deadline check after timer", func(t *testing.T) {
		clock := streamtest.NewFakeClock(time.Unix(0, 0))
		local, peer := streamtest.Pipe(streamtest.PipeOptions{})
		session := stream.NewSession(local, stream.Options{
			Clock: clock, PingInterval: 20 * time.Second, PingTimeout: 10 * time.Second,
		})
		done := make(chan error, 1)
		go func() { done <- session.Run(context.Background()) }()
		waitForFakeTimer(t, clock)
		clock.Advance(10*time.Second + time.Nanosecond)
		assertHeartbeatCloseReceived(t, peer, done)
	})
}

func waitForFakeTimer(t *testing.T, clock *streamtest.FakeClock) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if clock.PendingTimers() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("heartbeat loop did not schedule its FakeClock timer")
}

func assertHeartbeatCloseReceived(t *testing.T, peer stream.Conn, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		var closeErr *stream.CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != stream.CloseHeartbeat || closeErr.Reason != "ping timeout" {
			t.Fatalf("Session.Run() = %v, want close 4000 ping timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat timeout did not stop the session")
	}

	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := peer.Read(readCtx)
	var received *stream.CloseError
	if !errors.As(err, &received) || received.Code != 4000 || received.Reason != "ping timeout" || !received.Remote {
		t.Fatalf("peer received close = %v, want remote close 4000 ping timeout", err)
	}
}
