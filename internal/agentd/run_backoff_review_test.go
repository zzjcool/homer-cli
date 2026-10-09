package agentd

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
	"github.com/zzjcool/homer-cli/internal/stream/wsconn"
)

func TestRunBackoffGrowthAndCap(t *testing.T) {
	logger := &captureLogger{}
	d := New(Config{
		HubURL: "http://hub.example", AgentID: "backoff-sequence",
		Backoff: stream.Backoff{Base: time.Second, Max: time.Minute, Factor: 2, Jitter: 0.5},
		Stream:  stream.Options{Logger: logger},
	}, &testExecutor{})
	var attempts atomic.Int32
	d.dialStream = func(context.Context, string, wsconn.DialOptions) (stream.Conn, *http.Response, error) {
		attempts.Add(1)
		return nil, nil, errors.New("network down")
	}
	waits := make(chan time.Duration, 16)
	continueRun := make(chan struct{}, 16)
	d.retryWait = func(ctx context.Context, delay time.Duration) bool {
		waits <- delay
		select {
		case <-continueRun:
			return true
		case <-ctx.Done():
			return false
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Downward jitter is allowed, but every retry must retain the exponential
	// sequence and clamp at the configured 60s maximum.
	nominal := time.Second
	for attempt := 0; attempt < 8; attempt++ {
		var got time.Duration
		select {
		case got = <-waits:
		case <-time.After(time.Second):
			cancel()
			t.Fatalf("retry %d did not enter retryWait", attempt)
		}
		floor := nominal / 2
		if got < floor || got > nominal {
			cancel()
			t.Fatalf("retry %d wait = %s, want jittered interval [%s,%s]", attempt, got, floor, nominal)
		}
		if nominal < time.Minute {
			nominal *= 2
			if nominal > time.Minute {
				nominal = time.Minute
			}
		}
		if attempt < 7 {
			continueRun <- struct{}{}
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	if attempts.Load() < 7 {
		t.Fatalf("dial attempts = %d, want retries to reach backoff cap", attempts.Load())
	}
	if !logger.contains("network down") {
		t.Fatalf("dial retry was not logged: %v", logger.snapshot())
	}
}

func TestRunStableConnectionResetsBackoffAndSupersededLogsEveryTime(t *testing.T) {
	logger := &captureLogger{}
	clock := streamtest.NewFakeClock(time.Unix(0, 0))
	d := New(Config{
		HubURL: "http://hub.example", AgentID: "stable-reset",
		Backoff: stream.Backoff{Base: time.Second, Max: time.Minute, Factor: 2},
		Stream:  stream.Options{Logger: logger, Clock: clock},
	}, &testExecutor{})
	var attempts atomic.Int32
	connectedPeers := make(chan *stream.Session, 3)
	d.dialStream = func(ctx context.Context, _ string, _ wsconn.DialOptions) (stream.Conn, *http.Response, error) {
		attempt := attempts.Add(1)
		if attempt <= 2 {
			return nil, nil, errors.New("transient network failure")
		}
		agentConn, hubConn := streamtest.Pipe(streamtest.PipeOptions{})
		peer := newTestHubSession(ctx, hubConn, func(hello hub.HelloParams) hub.WelcomeResult {
			return hub.WelcomeResult{
				Proto: hello.Proto, PingIntervalMs: 60_000, PingTimeoutMs: 120_000,
			}
		})
		connectedPeers <- peer
		return agentConn, nil, nil
	}
	waits := make(chan time.Duration, 8)
	continueRun := make(chan struct{}, 8)
	d.retryWait = func(ctx context.Context, delay time.Duration) bool {
		waits <- delay
		select {
		case <-continueRun:
			return true
		case <-ctx.Done():
			return false
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run() = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Run did not stop after cancellation")
		}
	})

	waitRetryDelay(t, waits, time.Second, "first transient error")
	continueRun <- struct{}{}
	waitRetryDelay(t, waits, 2*time.Second, "second transient error")
	continueRun <- struct{}{}

	stablePeer := nextConnectedPeer(t, connectedPeers)
	waitUntilLoggerCount(t, logger, "已连接 hub", 1)
	clock.Advance(30 * time.Second)
	stablePeer.Close(stream.CloseGoingAway, "hub restarting")
	// A normal close after a stable 30s connection must restart at Base.
	waitRetryDelay(t, waits, time.Second, "first disconnect after a stable connection")
	continueRun <- struct{}{}

	firstSuperseded := nextConnectedPeer(t, connectedPeers)
	waitUntilLoggerCount(t, logger, "已连接 hub", 2)
	firstSuperseded.Close(stream.CloseSuperseded, "same agent ID")
	waitRetryDelay(t, waits, 30*time.Second, "first superseded close")
	continueRun <- struct{}{}

	secondSuperseded := nextConnectedPeer(t, connectedPeers)
	waitUntilLoggerCount(t, logger, "已连接 hub", 3)
	secondSuperseded.Close(stream.CloseSuperseded, "same agent ID")
	waitRetryDelay(t, waits, 30*time.Second, "second superseded close")

	if got := countLogPrefix(logger, "agent 被顶替："); got != 2 {
		t.Fatalf("always-path superseded log count = %d, want one line for each of 2 closes; logs=%v", got, logger.snapshot())
	}
	cancel()
}

func TestRunUnauthorizedRetryThrottleUsesClock(t *testing.T) {
	logger := &captureLogger{}
	clock := streamtest.NewFakeClock(time.Unix(0, 0))
	d := New(Config{HubURL: "http://hub.example", AgentID: "unauthorized-throttle", Stream: stream.Options{Logger: logger}}, &testExecutor{})
	d.retries = stream.NewThrottledLoggerWithClock(logger, time.Minute, clock)
	d.dialStream = func(context.Context, string, wsconn.DialOptions) (stream.Conn, *http.Response, error) {
		return nil, nil, &wsconn.DialError{Status: http.StatusUnauthorized}
	}
	waits := make(chan time.Duration, 4)
	continueRun := make(chan struct{}, 4)
	d.retryWait = func(ctx context.Context, delay time.Duration) bool {
		waits <- delay
		select {
		case <-continueRun:
			return true
		case <-ctx.Done():
			return false
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run() = %v", err)
			}
		case <-time.After(time.Second):
			t.Error("Run did not stop after cancellation")
		}
	})

	waitRetryDelay(t, waits, 5*time.Minute, "first HTTP 401")
	if got := countLogContaining(logger, "WebSocket 401"); got != 1 {
		t.Fatalf("first 401 log count = %d, want 1; logs=%v", got, logger.snapshot())
	}
	continueRun <- struct{}{}

	// One second is still inside the one-minute key window; this also catches
	// a broken 1ns window without relying on real sleeps.
	clock.Advance(time.Second)
	waitRetryDelay(t, waits, 5*time.Minute, "second HTTP 401 inside throttle window")
	if got := countLogContaining(logger, "WebSocket 401"); got != 1 {
		t.Fatalf("401 log inside throttle window emitted %d times, want suppression; logs=%v", got, logger.snapshot())
	}
	continueRun <- struct{}{}

	clock.Advance(time.Minute)
	waitRetryDelay(t, waits, 5*time.Minute, "HTTP 401 after throttle window")
	if got := countLogContaining(logger, "WebSocket 401"); got != 2 {
		t.Fatalf("401 log after throttle window count = %d, want 2; logs=%v", got, logger.snapshot())
	}
	if !logger.contains("重新生成接入码") {
		t.Fatalf("401 retry text does not direct the operator to regenerate the enrollment code: %v", logger.snapshot())
	}
	cancel()
}

func waitRetryDelay(t *testing.T, waits <-chan time.Duration, want time.Duration, label string) {
	t.Helper()
	select {
	case got := <-waits:
		if got != want {
			t.Fatalf("%s retry delay = %s, want %s", label, got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not reach retryWait", label)
	}
}

func nextConnectedPeer(t *testing.T, peers <-chan *stream.Session) *stream.Session {
	t.Helper()
	select {
	case peer := <-peers:
		return peer
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not establish the expected stream connection")
		return nil
	}
}

func waitUntilLoggerCount(t *testing.T, logger *captureLogger, text string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countLogContaining(logger, text) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("logger did not record %q %d times: %v", text, want, logger.snapshot())
}

func countLogContaining(logger *captureLogger, text string) int {
	count := 0
	for _, line := range logger.snapshot() {
		if strings.Contains(line, text) {
			count++
		}
	}
	return count
}

func countLogPrefix(logger *captureLogger, prefix string) int {
	count := 0
	for _, line := range logger.snapshot() {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}
