package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

const sessionEventTestQueueSize = 64

func TestSessionEventNotificationsStayOrderedAndBounded(t *testing.T) {
	const eventCount = 2000
	left, right := newSessionEventTestPipe()
	logger := &eventQueueTestLogger{}
	sender := NewSession(left, Options{PingInterval: time.Hour})
	receiver := NewSession(right, Options{PingInterval: time.Hour, Logger: logger})
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	unblockFirst := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	completed := make(chan int, eventCount)
	receiver.OnEvent("ordered", func(_ context.Context, _ string, params json.RawMessage) {
		var value int
		if err := json.Unmarshal(params, &value); err != nil {
			t.Errorf("decode event: %v", err)
			return
		}
		if value == 0 {
			close(firstStarted)
			<-releaseFirst
		}
		completed <- value
	})

	ctx, cancel := context.WithCancel(context.Background())
	senderDone := make(chan error, 1)
	receiverDone := make(chan error, 1)
	go func() { senderDone <- sender.Run(ctx) }()
	go func() { receiverDone <- receiver.Run(ctx) }()
	defer func() {
		unblockFirst()
		sender.Close(CloseNormal, "test cleanup")
		receiver.Close(CloseNormal, "test cleanup")
		cancel()
		for name, done := range map[string]<-chan error{"sender": senderDone, "receiver": receiverDone} {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Errorf("%s session did not stop", name)
			}
		}
	}()

	notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer notifyCancel()
	if err := sender.Notify(notifyCtx, "ordered", 0); err != nil {
		t.Fatalf("Notify first event: %v", err)
	}
	waitEventSignal(t, firstStarted, "first event handler")
	for value := 1; value < eventCount; value++ {
		if err := sender.Notify(notifyCtx, "ordered", value); err != nil {
			t.Fatalf("Notify event %d: %v", value, err)
		}
	}
	// The unknown barrier is processed only after the read loop has attempted
	// to enqueue or drop every preceding event.
	if err := sender.Notify(notifyCtx, "event-barrier", nil); err != nil {
		t.Fatalf("Notify barrier: %v", err)
	}
	waitEventCondition(t, func() bool { return receiver.Stats().FramesIn == eventCount+1 }, "all 2,000 notifications and barrier received")

	// With the old per-event goroutines, later callbacks finish while event 0
	// is blocked. Releasing the serial worker allows the bounded implementation
	// to drain the one active event and 64 queued entries; overflow is dropped.
	unblockFirst()
	got := make([]int, 0, sessionEventTestQueueSize+1)
	for range sessionEventTestQueueSize + 1 {
		select {
		case value := <-completed:
			got = append(got, value)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d ordered events completed", len(got))
		}
	}
	for index := 1; index < len(got); index++ {
		if got[index] <= got[index-1] {
			t.Fatalf("event delivery is not strictly increasing at index %d: %d then %d", index, got[index-1], got[index])
		}
	}
	if got[0] != 0 {
		t.Fatalf("first delivered event = %d, want 0", got[0])
	}
	droppedCount := receiver.eventDropped.Load()
	if len(got)+int(droppedCount) != eventCount {
		t.Fatalf("delivered events (%d) + dropped events (%d) != sent events (%d)", len(got), droppedCount, eventCount)
	}
	if droppedCount != eventCount-sessionEventTestQueueSize-1 {
		t.Fatalf("dropped events = %d, want %d for one active handler plus the bounded queue", droppedCount, eventCount-sessionEventTestQueueSize-1)
	}
	if lines := logger.snapshot(); len(lines) != 1 || !strings.Contains(lines[0], "event queue") {
		t.Fatalf("event queue drop logs = %v, want one throttled queue-full log", lines)
	}
	select {
	case value := <-completed:
		t.Fatalf("unexpected extra delivered event %d", value)
	default:
	}
}

func TestSessionEventDispatcherStopsWhenSessionCloses(t *testing.T) {
	left, right := newSessionEventTestPipe()
	session := NewSession(right, Options{PingInterval: time.Hour})
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	handlerDone := make(chan struct{})
	session.OnEvent("blocked", func(context.Context, string, json.RawMessage) {
		close(started)
		<-release
		close(handlerDone)
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- session.Run(ctx) }()
	defer func() {
		unblock()
		_ = left.CloseNow()
		cancel()
	}()

	message, err := EncodeFrame(&Frame{T: TEvt, M: "blocked", P: json.RawMessage(`null`)}, DefaultMaxFrame)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if err := left.Write(context.Background(), message); err != nil {
		t.Fatalf("write blocked event: %v", err)
	}
	waitEventSignal(t, started, "blocked event handler")
	session.Close(CloseNormal, "test close")
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("session Run after Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("event dispatcher prevented Session.Run from ending after Close")
	}
	unblock()
	waitEventSignal(t, handlerDone, "blocked handler exit")
}

func waitEventSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitEventCondition(t *testing.T, condition func() bool, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", name)
}

type eventQueueTestLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *eventQueueTestLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *eventQueueTestLogger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

type sessionEventTestConn struct {
	in        <-chan []byte
	out       chan<- []byte
	localDone chan struct{}
	peerDone  <-chan struct{}
	closeOnce sync.Once
}

func newSessionEventTestPipe() (*sessionEventTestConn, *sessionEventTestConn) {
	leftToRight := make(chan []byte, 4096)
	rightToLeft := make(chan []byte, 4096)
	leftDone := make(chan struct{})
	rightDone := make(chan struct{})
	return &sessionEventTestConn{in: rightToLeft, out: leftToRight, localDone: leftDone, peerDone: rightDone},
		&sessionEventTestConn{in: leftToRight, out: rightToLeft, localDone: rightDone, peerDone: leftDone}
}

func (c *sessionEventTestConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.localDone:
		return nil, io.ErrClosedPipe
	case <-c.peerDone:
		return nil, io.ErrClosedPipe
	case message := <-c.in:
		return append([]byte(nil), message...), nil
	}
}

func (c *sessionEventTestConn) Write(ctx context.Context, message []byte) error {
	copyOfMessage := append([]byte(nil), message...)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.localDone:
		return io.ErrClosedPipe
	case <-c.peerDone:
		return io.ErrClosedPipe
	case c.out <- copyOfMessage:
		return nil
	}
}

func (c *sessionEventTestConn) Close(CloseCode, string) error {
	c.closeOnce.Do(func() { close(c.localDone) })
	return nil
}

func (c *sessionEventTestConn) CloseNow() error {
	c.closeOnce.Do(func() { close(c.localDone) })
	return nil
}

func (*sessionEventTestConn) Info() ConnInfo { return ConnInfo{Transport: "test"} }

var _ Conn = (*sessionEventTestConn)(nil)
