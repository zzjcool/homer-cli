package stream_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
)

const testTimeout = 3 * time.Second

func trackGoroutineBaseline(t *testing.T) {
	t.Helper()
	baseline := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(time.Second)
		for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
			runtime.GC()
			select {
			case <-time.After(10 * time.Millisecond):
			}
		}
		if got := runtime.NumGoroutine(); got > baseline+2 {
			t.Errorf("goroutines after test = %d, baseline %d (+2 tolerance)", got, baseline)
		}
	})
}

func waitDone(t *testing.T, done <-chan error, name string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Errorf("%s did not stop before timeout", name)
	}
}

func callJSON[T any](t *testing.T, session *Session, method string, params any, opts ...CallOption) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	payload, err := session.Call(ctx, method, params, opts...)
	if err != nil {
		t.Fatalf("Call(%q) error = %v", method, err)
	}
	var result T
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode result %s: %v", payload, err)
	}
	return result
}

func TestSessionHappyPath(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	client := NewSession(left, Options{PingInterval: time.Hour})
	server := NewSession(right, Options{PingInterval: time.Hour})
	server.Handle("echo", func(_ context.Context, req *Request) (any, error) {
		var params struct {
			Value string `json:"value"`
		}
		if err := req.Decode(&params); err != nil {
			return nil, err
		}
		req.Progress(map[string]int{"step": 1})
		req.Progress(map[string]int{"step": 2})
		return params.Value, nil
	})
	clientDone, serverDone, cancel := runSessions(t, client, server)
	defer cleanupSessions(t, client, server, clientDone, serverDone, cancel)
	var steps []int
	ctx, callCancel := context.WithTimeout(context.Background(), testTimeout)
	defer callCancel()
	payload, err := client.Call(ctx, "echo", map[string]string{"value": "hello"}, WithProgress(func(raw json.RawMessage) {
		var progress struct {
			Step int `json:"step"`
		}
		if json.Unmarshal(raw, &progress) == nil {
			steps = append(steps, progress.Step)
		}
	}))
	if err != nil || string(payload) != `"hello"` {
		t.Fatalf("Call = %s, %v", payload, err)
	}
	if fmt.Sprint(steps) != "[1 2]" {
		t.Fatalf("progress = %v", steps)
	}
}

func TestMultiplexCorrelation(t *testing.T) {
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	client := NewSession(left, Options{PingInterval: time.Hour})
	server := NewSession(right, Options{PingInterval: time.Hour})
	server.Handle("echo", func(_ context.Context, req *Request) (any, error) {
		var value int
		if err := req.Decode(&value); err != nil {
			return nil, err
		}
		return value, nil
	})
	clientDone, serverDone, cancel := runSessions(t, client, server)
	defer cleanupSessions(t, client, server, clientDone, serverDone, cancel)

	const calls = 100
	var wg sync.WaitGroup
	errCh := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(want int) {
			defer wg.Done()
			ctx, ctxCancel := context.WithTimeout(context.Background(), testTimeout)
			defer ctxCancel()
			payload, err := client.Call(ctx, "echo", want)
			if err != nil {
				errCh <- err
				return
			}
			var got int
			if err := json.Unmarshal(payload, &got); err != nil {
				errCh <- err
				return
			}
			if got != want {
				errCh <- fmt.Errorf("got %d, want %d", got, want)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestOutOfOrderCompletion(t *testing.T) {
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	client := NewSession(left, Options{PingInterval: time.Hour})
	server := NewSession(right, Options{PingInterval: time.Hour})
	slowStarted := make(chan struct{}, 1)
	server.Handle("delay", func(ctx context.Context, req *Request) (any, error) {
		var p struct {
			Value string        `json:"value"`
			Delay time.Duration `json:"delay"`
		}
		if err := req.Decode(&p); err != nil {
			return nil, err
		}
		if p.Value == "slow" {
			slowStarted <- struct{}{}
		}
		timer := time.NewTimer(p.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return p.Value, nil
		}
	})
	clientDone, serverDone, cancel := runSessions(t, client, server)
	defer cleanupSessions(t, client, server, clientDone, serverDone, cancel)

	type result struct {
		value string
		err   error
	}
	results := make(chan result, 2)
	go func() {
		ctx, stop := context.WithTimeout(context.Background(), testTimeout)
		defer stop()
		payload, err := client.Call(ctx, "delay", map[string]any{"value": "slow", "delay": 120 * time.Millisecond})
		results <- result{value: string(payload), err: err}
	}()
	waitSignal(t, slowStarted, "slow request began")
	go func() {
		ctx, stop := context.WithTimeout(context.Background(), testTimeout)
		defer stop()
		payload, err := client.Call(ctx, "delay", map[string]any{"value": "fast", "delay": 5 * time.Millisecond})
		results <- result{value: string(payload), err: err}
	}()
	var first result
	select {
	case first = <-results:
	case <-time.After(testTimeout):
		t.Fatal("first out-of-order call timed out")
	}
	if first.err != nil {
		t.Fatalf("first response error = %v", first.err)
	}
	if first.value != `"fast"` {
		t.Fatalf("first response = %s, want fast result first", first.value)
	}
	var second result
	select {
	case second = <-results:
	case <-time.After(testTimeout):
		t.Fatal("second out-of-order call timed out")
	}
	if second.err != nil || second.value != `"slow"` {
		t.Fatalf("second response = %s, %v; want slow", second.value, second.err)
	}
}

func TestDuplicateID(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	server := NewSession(right, Options{PingInterval: time.Hour})
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server.Handle("hold", func(ctx context.Context, _ *Request) (any, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return "first", nil
		}
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(context.Background()) }()
	defer func() {
		server.Close(CloseGoingAway, "test cleanup")
		waitDone(t, serverDone, "server")
	}()

	first := &Frame{T: TReq, ID: "same", M: "hold", P: json.RawMessage(`{}`)}
	second := *first
	for _, frame := range []*Frame{first, &second} {
		data, _ := EncodeFrame(frame, 1024)
		writeBytes(t, left, data)
	}
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("handler did not start")
	}
	duplicate := readFrame(t, left)
	if duplicate.T != TRes || duplicate.ID != "same" || duplicate.E == nil || duplicate.E.Code != CodeDuplicateID {
		t.Fatalf("duplicate response = %#v", duplicate)
	}
	close(release)
	firstResult := readFrame(t, left)
	if firstResult.T != TRes || firstResult.ID != "same" || !firstResult.OK {
		t.Fatalf("first response = %#v", firstResult)
	}
	data, _ := EncodeFrame(first, 1024)
	writeBytes(t, left, data)
	reused := readFrame(t, left)
	if reused.T != TRes || reused.ID != "same" || !reused.OK {
		t.Fatalf("reused ID response = %#v", reused)
	}
	_ = left.CloseNow()
}

func TestUnknownMethod(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	server := NewSession(right, Options{PingInterval: time.Hour})
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(context.Background()) }()
	defer func() { server.Close(CloseGoingAway, "test cleanup"); waitDone(t, serverDone, "server") }()
	writeFrame(t, left, &Frame{T: TReq, ID: "missing", M: "not-registered"})
	frame := readFrame(t, left)
	if frame.E == nil || frame.E.Code != CodeUnknownMethod {
		t.Fatalf("unknown-method response = %#v", frame)
	}
	_ = left.CloseNow()
}

func TestCancel(t *testing.T) {
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	client := NewSession(left, Options{PingInterval: time.Hour})
	server := NewSession(right, Options{PingInterval: time.Hour})
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	server.Handle("wait", func(ctx context.Context, _ *Request) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		canceled <- struct{}{}
		return nil, ctx.Err()
	})
	clientDone, serverDone, stopSessions := runSessions(t, client, server)
	defer cleanupSessions(t, client, server, clientDone, serverDone, stopSessions)
	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, "wait", nil)
		callDone <- err
	}()
	waitSignal(t, started, "handler started")
	cancel()
	select {
	case err := <-callDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Call error = %v, want context.Canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("canceled Call did not return")
	}
	waitSignal(t, canceled, "handler context canceled by cancel frame")
	assertNoFrame(t, right, 50*time.Millisecond)
}

func TestCancelFrameWithoutCall(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	server := NewSession(right, Options{PingInterval: time.Hour})
	started := make(chan struct{}, 1)
	ctxDone := make(chan struct{}, 1)
	server.Handle("wait", func(ctx context.Context, _ *Request) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		ctxDone <- struct{}{}
		return nil, ctx.Err()
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(context.Background()) }()
	defer func() { server.Close(CloseGoingAway, "test cleanup"); waitDone(t, serverDone, "server") }()
	writeFrame(t, left, &Frame{T: TReq, ID: "cancel-me", M: "wait"})
	waitSignal(t, started, "handler started")
	writeFrame(t, left, &Frame{T: TCancel, ID: "cancel-me"})
	waitSignal(t, ctxDone, "handler context canceled")
	assertNoFrame(t, left, 60*time.Millisecond)
	_ = left.CloseNow()
}

func TestCancelCompleteRace(t *testing.T) {
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	recorded := &recordConn{Conn: right, writes: make(chan []byte, 1024)}
	client := NewSession(left, Options{PingInterval: time.Hour})
	server := NewSession(recorded, Options{PingInterval: time.Hour})
	gateCh := make(chan *raceGate, 1)
	server.Handle("race", func(ctx context.Context, req *Request) (any, error) {
		gate := <-gateCh
		gate.started <- req.ID
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-gate.release:
			return "done", nil
		}
	})
	clientDone, serverDone, cancel := runSessions(t, client, server)
	defer cleanupSessions(t, client, server, clientDone, serverDone, cancel)

	const iterations = 200
	for i := 0; i < iterations; i++ {
		gate := &raceGate{started: make(chan string, 1), release: make(chan struct{})}
		gateCh <- gate
		ctx, stop := context.WithTimeout(context.Background(), testTimeout)
		callDone := make(chan error, 1)
		go func() {
			_, err := client.Call(ctx, "race", nil)
			callDone <- err
		}()
		var id string
		select {
		case id = <-gate.started:
		case <-time.After(testTimeout):
			stop()
			t.Fatal("race handler did not start")
		}
		barrier := make(chan struct{})
		go func() { <-barrier; stop() }()
		go func() { <-barrier; close(gate.release) }()
		close(barrier)
		select {
		case err := <-callDone:
			stop()
			if err != nil && !errors.Is(err, context.Canceled) {
				var closed *SessionClosedError
				if !errors.As(err, &closed) {
					t.Fatalf("iteration %d Call error = %v", i, err)
				}
			}
		case <-time.After(testTimeout):
			stop()
			t.Fatalf("iteration %d Call timed out", i)
		}
		// A cancellation can legitimately win and suppress the response. Drain
		// any single terminal response that did win before the next iteration.
		waitUntil(t, func() bool { return server.Stats().InflightIn == 0 })
		flushCtx, flushCancel := context.WithTimeout(context.Background(), testTimeout)
		if err := server.Flush(flushCtx); err != nil {
			flushCancel()
			t.Fatalf("iteration %d Flush: %v", i, err)
		}
		flushCancel()
		count, _ := recorded.countTerminal(id)
		if count > 1 {
			t.Fatalf("iteration %d got %d terminal responses", i, count)
		}
	}
}

type raceGate struct {
	started chan string
	release chan struct{}
}

func TestDeadlineBudget(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	server := NewSession(right, Options{PingInterval: time.Hour})
	server.Handle("timeout", func(ctx context.Context, _ *Request) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(context.Background()) }()
	defer func() { server.Close(CloseGoingAway, "test cleanup"); waitDone(t, serverDone, "server") }()
	writeFrame(t, left, &Frame{T: TReq, ID: "timeout", M: "timeout", DL: 20})
	frame := readFrame(t, left)
	if frame.E == nil || frame.E.Code != CodeTimeout {
		t.Fatalf("deadline response = %#v", frame)
	}
	_ = left.CloseNow()
}

func TestProgress(t *testing.T) {
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	client := NewSession(left, Options{PingInterval: time.Hour})
	server := NewSession(right, Options{PingInterval: time.Hour})
	server.Handle("progress", func(_ context.Context, req *Request) (any, error) {
		for n := 1; n <= 5; n++ {
			req.Progress(map[string]int{"n": n})
		}
		return "complete", nil
	})
	clientDone, serverDone, cancel := runSessions(t, client, server)
	defer cleanupSessions(t, client, server, clientDone, serverDone, cancel)
	var got []int
	ctx, stop := context.WithTimeout(context.Background(), testTimeout)
	defer stop()
	payload, err := client.Call(ctx, "progress", nil, WithProgress(func(raw json.RawMessage) {
		var value struct {
			N int `json:"n"`
		}
		if json.Unmarshal(raw, &value) == nil {
			got = append(got, value.N)
		}
	}))
	if err != nil || string(payload) != `"complete"` {
		t.Fatalf("Call = %s, %v", payload, err)
	}
	if len(got) != 5 {
		t.Fatalf("progress = %v, want all 5 events", got)
	}
	for i, value := range got {
		if value != i+1 {
			t.Fatalf("progress order = %v", got)
		}
	}
}

func TestProgressBackpressure(t *testing.T) {
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	slowServerConn := &streamtest.FaultConn{Conn: right, WriteDelay: time.Millisecond}
	client := NewSession(left, Options{PingInterval: time.Hour})
	server := NewSession(slowServerConn, Options{PingInterval: time.Hour})
	server.Handle("burst", func(_ context.Context, req *Request) (any, error) {
		for n := 1; n <= 256; n++ {
			req.Progress(map[string]int{"n": n})
		}
		return "terminal", nil
	})
	clientDone, serverDone, cancel := runSessions(t, client, server)
	defer cleanupSessions(t, client, server, clientDone, serverDone, cancel)
	ctx, stop := context.WithTimeout(context.Background(), testTimeout)
	defer stop()
	payload, err := client.Call(ctx, "burst", nil)
	if err != nil || string(payload) != `"terminal"` {
		t.Fatalf("Call = %s, %v", payload, err)
	}
	if dropped := server.Stats().ProgressDropped; dropped == 0 {
		t.Fatal("progress ring did not drop under backpressure")
	}
	if client.Stats().Pending != 0 {
		t.Fatalf("terminal response was lost: pending=%d", client.Stats().Pending)
	}
}

func TestBackpressureSlowConsumer(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{Buffer: 1})
	slowWriter := &streamtest.FaultConn{Conn: left, WriteDelay: time.Second}
	client := NewSession(slowWriter, Options{PingInterval: time.Hour, WriteTimeout: 100 * time.Millisecond, SendQueue: 1})
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Run(context.Background()) }()
	defer func() {
		client.Close(CloseGoingAway, "test cleanup")
		waitDone(t, clientDone, "client session")
		_ = right.CloseNow()
	}()
	calls := make([]<-chan error, 3)
	for i := range calls {
		callDone := make(chan error, 1)
		calls[i] = callDone
		go func(n int) {
			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			_, err := client.Call(ctx, fmt.Sprintf("blocked-%d", n), nil)
			callDone <- err
		}(i)
	}
	var closed *SessionClosedError
	var terminalErr error
	for i, done := range calls {
		select {
		case terminalErr = <-done:
			if i != len(calls)-1 && terminalErr == nil {
				continue
			}
		case <-time.After(testTimeout):
			t.Fatalf("Call %d did not return", i)
		}
		if errors.As(terminalErr, &closed) {
			break
		}
	}
	if !errors.As(terminalErr, &closed) {
		t.Fatalf("Call error = %T %v, want *SessionClosedError", terminalErr, terminalErr)
	}
	var closeErr *CloseError
	if !errors.As(closed.Cause, &closeErr) || closeErr.Code != ClosePolicy || closeErr.Reason != "slow-consumer" {
		t.Fatalf("session close cause = %v, want slow-consumer", closed.Cause)
	}
}

func TestPingPriorityOverQueuedResponses(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	writer := &firstWriteGate{Conn: right, firstStarted: make(chan struct{}, 1), release: make(chan struct{})}
	server := NewSession(writer, Options{PingInterval: time.Hour, WriteTimeout: time.Second, SendQueue: 2})
	server.Handle("reply", func(_ context.Context, req *Request) (any, error) { return req.ID, nil })
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(context.Background()) }()
	defer func() {
		writer.releaseOnce.Do(func() { close(writer.release) })
		server.Close(CloseGoingAway, "test cleanup")
		waitDone(t, serverDone, "server")
	}()
	writeFrame(t, left, &Frame{T: TReq, ID: "first", M: "reply"})
	waitSignal(t, writer.firstStarted, "first writer call")
	writeFrame(t, left, &Frame{T: TReq, ID: "second", M: "reply"})
	writeFrame(t, left, &Frame{T: TPing, ID: "urgent-ping"})
	waitUntil(t, func() bool { return server.Stats().FramesIn == 3 })
	writer.releaseOnce.Do(func() { close(writer.release) })
	first := readFrame(t, left)
	second := readFrame(t, left)
	third := readFrame(t, left)
	if first.T != TRes || first.ID != "first" {
		t.Fatalf("first frame = %#v", first)
	}
	if second.T != TPong || second.ID != "urgent-ping" {
		t.Fatalf("second frame = %#v, want prioritized pong", second)
	}
	if third.T != TRes || third.ID != "second" {
		t.Fatalf("third frame = %#v", third)
	}
	_ = left.CloseNow()
}

type firstWriteGate struct {
	Conn
	firstStarted chan struct{}
	release      chan struct{}
	releaseOnce  sync.Once
	firstOnce    sync.Once
}

func (c *firstWriteGate) Write(ctx context.Context, message []byte) error {
	c.firstOnce.Do(func() {
		c.firstStarted <- struct{}{}
		select {
		case <-ctx.Done():
		case <-c.release:
		}
	})
	return c.Conn.Write(ctx, message)
}

func TestHeartbeatTimeout(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	clock := streamtest.NewFakeClock(time.Unix(0, 0))
	session := NewSession(left, Options{Clock: clock, PingInterval: 20 * time.Second, PingTimeout: 10 * time.Second})
	done := make(chan error, 1)
	go func() { done <- session.Run(context.Background()) }()
	waitUntil(t, func() bool { return clock.PendingTimers() >= 1 })
	clock.Advance(10*time.Second + time.Nanosecond)
	select {
	case err := <-done:
		var closeErr *CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != CloseHeartbeat {
			t.Fatalf("Run error = %v, want close code %d", err, CloseHeartbeat)
		}
	case <-time.After(testTimeout):
		t.Fatal("heartbeat timeout did not close session")
	}
	_ = right.CloseNow()
}

func TestHeartbeatDataRefresh(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	clock := streamtest.NewFakeClock(time.Unix(0, 0))
	session := NewSession(left, Options{Clock: clock, PingInterval: 20 * time.Second, PingTimeout: 10 * time.Second})
	done := make(chan error, 1)
	go func() { done <- session.Run(context.Background()) }()
	waitUntil(t, func() bool { return clock.PendingTimers() >= 1 })
	clock.Advance(8 * time.Second)
	writeFrame(t, right, &Frame{T: "future"})
	writeFrame(t, right, &Frame{T: "future"})
	waitUntil(t, func() bool { return session.Stats().FramesIn > 0 })
	clock.Advance(8 * time.Second)
	select {
	case <-session.Done():
		t.Fatalf("session closed after receiving an inbound frame: %v", session.Err())
	default:
	}
	session.Close(CloseGoingAway, "test done")
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("session did not stop")
	}
	_ = right.CloseNow()
}

func TestLongHandlerKeepsAlive(t *testing.T) {
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	options := Options{PingInterval: 5 * time.Millisecond, PingTimeout: 30 * time.Millisecond}
	client := NewSession(left, options)
	server := NewSession(right, options)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	server.Handle("long", func(ctx context.Context, _ *Request) (any, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return "alive", nil
		}
	})
	clientDone, serverDone, cancel := runSessions(t, client, server)
	defer cleanupSessions(t, client, server, clientDone, serverDone, cancel)
	result := make(chan error, 1)
	go func() {
		ctx, stop := context.WithTimeout(context.Background(), testTimeout)
		defer stop()
		payload, err := client.Call(ctx, "long", nil)
		if err == nil && string(payload) != `"alive"` {
			err = fmt.Errorf("result = %s", payload)
		}
		result <- err
	}()
	waitSignal(t, started, "long handler started")
	keepAliveTimer := time.NewTimer(120 * time.Millisecond)
	select {
	case <-keepAliveTimer.C:
	case <-client.Done():
		keepAliveTimer.Stop()
		t.Fatalf("client closed while waiting: %v", client.Err())
	case <-server.Done():
		keepAliveTimer.Stop()
		t.Fatalf("server closed while waiting: %v", server.Err())
	}
	select {
	case <-client.Done():
		t.Fatalf("client session died during a long handler: %v", client.Err())
	default:
	}
	select {
	case <-server.Done():
		t.Fatalf("server session died during a long handler: %v", server.Err())
	default:
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testTimeout):
		t.Fatal("long handler Call timed out")
	}
}

func TestHandlerPanic(t *testing.T) {
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	client := NewSession(left, Options{PingInterval: time.Hour})
	server := NewSession(right, Options{PingInterval: time.Hour})
	server.Handle("panic", func(context.Context, *Request) (any, error) { panic("unexpected") })
	server.Handle("ok", func(context.Context, *Request) (any, error) { return "still alive", nil })
	clientDone, serverDone, cancel := runSessions(t, client, server)
	defer cleanupSessions(t, client, server, clientDone, serverDone, cancel)
	ctx, stop := context.WithTimeout(context.Background(), testTimeout)
	defer stop()
	_, err := client.Call(ctx, "panic", nil)
	var remote *Error
	if !errors.As(err, &remote) || remote.Code != CodeInternal {
		t.Fatalf("panic error = %T %v, want internal", err, err)
	}
	if got := callJSON[string](t, client, "ok", nil); got != "still alive" {
		t.Fatalf("post-panic call = %q", got)
	}
}

func TestSessionCloseFailsPending(t *testing.T) {
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	client := NewSession(left, Options{PingInterval: time.Hour})
	server := NewSession(right, Options{PingInterval: time.Hour})
	started := make(chan struct{}, 1)
	ctxCanceled := make(chan struct{}, 1)
	server.Handle("wait", func(ctx context.Context, _ *Request) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		ctxCanceled <- struct{}{}
		return nil, ctx.Err()
	})
	clientDone, serverDone, cancel := runSessions(t, client, server)
	defer cleanupSessions(t, client, server, clientDone, serverDone, cancel)
	callCtx, callCancel := context.WithTimeout(context.Background(), testTimeout)
	defer callCancel()
	callDone := make(chan error, 1)
	go func() {
		_, err := client.Call(callCtx, "wait", nil)
		callDone <- err
	}()
	waitSignal(t, started, "handler started")
	start := time.Now()
	client.Close(CloseGoingAway, "closed")
	select {
	case err := <-callDone:
		var closed *SessionClosedError
		if !errors.As(err, &closed) {
			t.Fatalf("pending Call error = %T %v", err, err)
		}
		if time.Since(start) > 200*time.Millisecond {
			t.Fatalf("pending Call took too long to fail: %v", time.Since(start))
		}
	case <-time.After(testTimeout):
		t.Fatal("pending Call did not fail immediately")
	}
	waitSignal(t, ctxCanceled, "handler context canceled")
}

func TestInflightLimit(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	server := NewSession(right, Options{PingInterval: time.Hour, MaxInflightIn: 1})
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server.Handle("hold", func(ctx context.Context, _ *Request) (any, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return "done", nil
		}
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(context.Background()) }()
	defer func() { server.Close(CloseGoingAway, "test cleanup"); waitDone(t, serverDone, "server") }()
	writeFrame(t, left, &Frame{T: TReq, ID: "1", M: "hold"})
	waitSignal(t, started, "first handler started")
	writeFrame(t, left, &Frame{T: TReq, ID: "2", M: "hold"})
	overloaded := readFrame(t, left)
	if overloaded.ID != "2" || overloaded.E == nil || overloaded.E.Code != CodeOverloaded {
		t.Fatalf("overloaded response = %#v", overloaded)
	}
	close(release)
	first := readFrame(t, left)
	if first.ID != "1" || !first.OK {
		t.Fatalf("first response = %#v", first)
	}
	_ = left.CloseNow()
}

func TestMaxFrame(t *testing.T) {
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	client := NewSession(left, Options{MaxFrame: 128, PingInterval: time.Hour})
	server := NewSession(right, Options{MaxFrame: 128, PingInterval: time.Hour})
	server.Handle("large", func(context.Context, *Request) (any, error) { return strings.Repeat("x", 512), nil })
	server.Handle("echo", func(context.Context, *Request) (any, error) { return "ok", nil })
	clientDone, serverDone, cancel := runSessions(t, client, server)
	defer cleanupSessions(t, client, server, clientDone, serverDone, cancel)
	oversizeCtx, oversizeCancel := context.WithTimeout(context.Background(), testTimeout)
	defer oversizeCancel()
	if _, err := client.Call(oversizeCtx, "echo", strings.Repeat("x", 512)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize request Call error = %v, want ErrFrameTooLarge", err)
	}
	ctx, stop := context.WithTimeout(context.Background(), testTimeout)
	defer stop()
	_, err := client.Call(ctx, "large", nil)
	var remote *Error
	if !errors.As(err, &remote) || remote.Code != CodeFrameTooLarge {
		t.Fatalf("oversize handler result error = %T %v, want frame-too-large", err, err)
	}
	if got := callJSON[string](t, client, "echo", nil); got != "ok" {
		t.Fatalf("session did not survive large result: %q", got)
	}
	if err := right.Write(ctx, []byte(strings.Repeat("x", 129))); err != nil {
		t.Fatalf("write oversized inbound frame: %v", err)
	}
	select {
	case <-client.Done():
	case <-time.After(testTimeout):
		t.Fatal("client session did not close for oversize inbound frame")
	}
	var closeErr *CloseError
	if !errors.As(client.Err(), &closeErr) || closeErr.Code != CloseTooBig {
		t.Fatalf("client close = %v, want close code %d", client.Err(), CloseTooBig)
	}
}

func TestUnknownFrameType(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	logger := &countLogger{}
	server := NewSession(right, Options{PingInterval: time.Hour, Logger: logger})
	server.Handle("ok", func(context.Context, *Request) (any, error) { return "ok", nil })
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(context.Background()) }()
	defer func() { server.Close(CloseGoingAway, "test cleanup"); waitDone(t, serverDone, "server") }()
	for i := 0; i < 3; i++ {
		writeBytes(t, left, []byte(`{"t":"future"}`))
	}
	waitUntil(t, func() bool { return server.Stats().FramesIn == 3 })
	if logger.count() != 1 {
		t.Fatalf("throttled log count = %d, want 1", logger.count())
	}
	writeFrame(t, left, &Frame{T: TReq, ID: "alive", M: "ok"})
	if got := readFrame(t, left); got.E != nil || !got.OK {
		t.Fatalf("session did not survive unknown frame: %#v", got)
	}
	_ = left.CloseNow()
}

func TestBinaryFrame(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	binary := &binaryConn{Conn: right}
	session := NewSession(binary, Options{PingInterval: time.Hour})
	done := make(chan error, 1)
	go func() { done <- session.Run(context.Background()) }()
	select {
	case err := <-done:
		if binary.closeCode.Load() != uint32(CloseUnsupported) {
			t.Fatalf("close code = %d, want %d (Run error %v)", binary.closeCode.Load(), CloseUnsupported, err)
		}
	case <-time.After(testTimeout):
		t.Fatal("binary message did not terminate session")
	}
	_ = left.CloseNow()
}

type binaryConn struct {
	Conn
	closeCode atomic.Uint32
}

func (c *binaryConn) Read(context.Context) ([]byte, error) {
	return nil, &ProtocolError{Code: CloseUnsupported, Msg: "binary WebSocket message"}
}
func (c *binaryConn) Write(ctx context.Context, b []byte) error { return c.Conn.Write(ctx, b) }
func (c *binaryConn) Close(code CloseCode, reason string) error {
	c.closeCode.Store(uint32(code))
	return c.Conn.Close(code, reason)
}
func (c *binaryConn) CloseNow() error { return c.Conn.CloseNow() }
func (c *binaryConn) Info() ConnInfo  { return c.Conn.Info() }

func TestOversizeFrameCloses1009(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	tracked := &closeCodeConn{Conn: right}
	session := NewSession(tracked, Options{MaxFrame: 16, PingInterval: time.Hour})
	done := make(chan error, 1)
	go func() { done <- session.Run(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := left.Write(ctx, []byte(`{"t":"evt","m":"too-long"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var closeErr *CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != CloseTooBig || tracked.code.Load() != uint32(CloseTooBig) {
			t.Fatalf("Run error=%v close code=%d", err, tracked.code.Load())
		}
	case <-time.After(testTimeout):
		t.Fatal("oversized frame did not close the session")
	}
	_ = left.CloseNow()
}

type closeCodeConn struct {
	Conn
	code atomic.Uint32
}

func (c *closeCodeConn) Close(code CloseCode, reason string) error {
	c.code.Store(uint32(code))
	return c.Conn.Close(code, reason)
}

func TestSessionProtocolErrorCloseCode(t *testing.T) {
	trackGoroutineBaseline(t)
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	session := NewSession(right, Options{PingInterval: time.Hour})
	done := make(chan error, 1)
	go func() { done <- session.Run(context.Background()) }()
	defer func() {
		session.Close(CloseGoingAway, "protocol test cleanup")
		_ = left.CloseNow()
	}()
	writeFrame(t, left, &Frame{T: ""})
	select {
	case err := <-done:
		var closeErr *CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != CloseProtocol {
			t.Fatalf("Run error = %T %v, want close code %d", err, err, CloseProtocol)
		}
	case <-time.After(testTimeout):
		t.Fatal("malformed frame did not close session")
	}
	_ = left.CloseNow()
}

func TestCallParamsTooLargeDoesNotSend(t *testing.T) {
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	client := NewSession(left, Options{MaxFrame: 64, PingInterval: time.Hour})
	server := NewSession(right, Options{MaxFrame: 64, PingInterval: time.Hour})
	clientDone, serverDone, cancelSessions := runSessions(t, client, server)
	defer cleanupSessions(t, client, server, clientDone, serverDone, cancelSessions)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	_, err := client.Call(ctx, "too-large", strings.Repeat("x", 200))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Call error = %v, want ErrFrameTooLarge", err)
	}
	if client.Stats().FramesOut != 0 {
		t.Fatalf("FramesOut = %d, large call was sent", client.Stats().FramesOut)
	}
}

func TestSessionGoroutinesDoNotLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	client := NewSession(left, Options{PingInterval: time.Hour})
	server := NewSession(right, Options{PingInterval: time.Hour})
	started := make(chan struct{}, 1)
	handlerCanceled := make(chan struct{}, 1)
	server.Handle("wait", func(ctx context.Context, _ *Request) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		handlerCanceled <- struct{}{}
		return nil, ctx.Err()
	})
	clientDone, serverDone, cancel := runSessions(t, client, server)
	ctx, callCancel := context.WithTimeout(context.Background(), testTimeout)
	callDone := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, "wait", nil)
		callDone <- err
	}()
	waitSignal(t, started, "handler start")
	client.Close(CloseGoingAway, "goroutine leak test")
	select {
	case err := <-callDone:
		var closed *SessionClosedError
		if !errors.As(err, &closed) {
			t.Fatalf("Call error = %T %v", err, err)
		}
	case <-time.After(testTimeout):
		t.Fatal("pending Call did not return")
	}
	waitSignal(t, handlerCanceled, "handler cancellation")
	waitDone(t, clientDone, "client session")
	waitDone(t, serverDone, "server session")
	callCancel()
	cancel()
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.GC()
		select {
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := runtime.NumGoroutine(); got > baseline {
		t.Fatalf("goroutines after session shutdown = %d, baseline %d", got, baseline)
	}
}

func TestNotifyAndEvent(t *testing.T) {
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	client := NewSession(left, Options{PingInterval: time.Hour})
	server := NewSession(right, Options{PingInterval: time.Hour})
	events := make(chan string, 1)
	server.OnEvent("notify", func(_ context.Context, method string, params json.RawMessage) {
		events <- method + ":" + string(params)
	})
	clientDone, serverDone, cancel := runSessions(t, client, server)
	defer cleanupSessions(t, client, server, clientDone, serverDone, cancel)
	ctx, stop := context.WithTimeout(context.Background(), testTimeout)
	defer stop()
	if err := client.Notify(ctx, "notify", map[string]int{"n": 3}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event != `notify:{"n":3}` {
			t.Fatalf("event = %s", event)
		}
	case <-time.After(testTimeout):
		t.Fatal("event handler not called")
	}
}

func runSessions(t *testing.T, a, b *Session) (aDone, bDone <-chan error, cancel context.CancelFunc) {
	t.Helper()
	trackGoroutineBaseline(t)
	ctx, cancel := context.WithCancel(context.Background())
	ad, bd := make(chan error, 1), make(chan error, 1)
	go func() { ad <- a.Run(ctx) }()
	go func() { bd <- b.Run(ctx) }()
	return ad, bd, cancel
}

func cleanupSessions(t *testing.T, a, b *Session, aDone, bDone <-chan error, cancel context.CancelFunc) {
	t.Helper()
	a.Close(CloseGoingAway, "test cleanup")
	b.Close(CloseGoingAway, "test cleanup")
	cancel()
	waitDone(t, aDone, "first session")
	waitDone(t, bDone, "second session")
}

func writeFrame(t *testing.T, conn Conn, frame *Frame) {
	t.Helper()
	payload, err := EncodeFrame(frame, DefaultMaxFrame)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	writeBytes(t, conn, payload)
}

func writeBytes(t *testing.T, conn Conn, payload []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := conn.Write(ctx, payload); err != nil {
		t.Fatalf("Conn.Write: %v", err)
	}
}

func readFrame(t *testing.T, conn Conn) *Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("Conn.Read: %v", err)
	}
	frame, err := DecodeFrame(payload, DefaultMaxFrame)
	if err != nil {
		t.Fatalf("DecodeFrame(%s): %v", payload, err)
	}
	return frame
}

func waitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func assertNoFrame(t *testing.T, conn Conn, duration time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	if frame, err := conn.Read(ctx); err == nil {
		t.Fatalf("unexpected frame: %s", frame)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Read error = %v, want deadline exceeded", err)
	}
}

func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

type countLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *countLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}
func (l *countLogger) count() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.lines) }

// recordConn records server writes so race tests can prove that a request ID
// never receives two terminal responses.
type recordConn struct {
	Conn
	writes chan []byte
	mu     sync.Mutex
	seen   map[string]int
	frames map[string]Frame
}

func (c *recordConn) Write(ctx context.Context, data []byte) error {
	if err := c.Conn.Write(ctx, data); err != nil {
		return err
	}
	copy := append([]byte(nil), data...)
	select {
	case c.writes <- copy:
	default:
	}
	return nil
}

func (c *recordConn) countTerminal(id string) (int, bool) {
	c.mu.Lock()
	if c.seen == nil {
		c.seen = make(map[string]int)
		c.frames = make(map[string]Frame)
	}
	c.mu.Unlock()
	for {
		select {
		case data := <-c.writes:
			var frame Frame
			if json.Unmarshal(data, &frame) == nil && frame.T == TRes {
				c.mu.Lock()
				c.seen[frame.ID]++
				c.frames[frame.ID] = frame
				c.mu.Unlock()
			}
		default:
			c.mu.Lock()
			count, ok := c.seen[id]
			c.mu.Unlock()
			return count, ok
		}
	}
}

func (c *recordConn) seenFrame(id string) (Frame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	frame, ok := c.frames[id]
	return frame, ok
}
