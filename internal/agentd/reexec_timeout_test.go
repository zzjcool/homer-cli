package agentd

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
)

func TestRequestReexecTimesOutWhileReadRequestsContinue(t *testing.T) {
	agentConn, hubConn := streamtest.Pipe(streamtest.PipeOptions{})
	logger := &captureLogger{}
	d := New(Config{
		AgentID: "reexec-timeout",
		Stream:  stream.Options{Logger: logger, PingInterval: time.Hour, PingTimeout: 2 * time.Hour},
	}, &testExecutor{})
	agentSession := stream.NewSession(agentConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	readStarted := make(chan struct{}, 1)
	agentSession.Handle("test.read", func(ctx context.Context, _ *stream.Request) (any, error) {
		select {
		case readStarted <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	hubSession := stream.NewSession(hubConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})

	ctx, stopSessions := context.WithCancel(context.Background())
	agentDone := make(chan error, 1)
	hubDone := make(chan error, 1)
	go func() { agentDone <- agentSession.Run(ctx) }()
	go func() { hubDone <- hubSession.Run(ctx) }()

	var calls sync.WaitGroup
	stopRequests := make(chan struct{})
	requestPumpDone := make(chan struct{})
	var stopRequestsOnce sync.Once
	go func() {
		defer close(requestPumpDone)
		for {
			select {
			case <-stopRequests:
				return
			default:
			}
			callCtx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
			calls.Add(1)
			go func() {
				defer calls.Done()
				defer cancel()
				_, _ = hubSession.Call(callCtx, "test.read", struct{}{})
			}()
			select {
			case <-stopRequests:
				return
			case <-time.After(25 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() {
		stopRequestsOnce.Do(func() { close(stopRequests) })
		<-requestPumpDone
		agentSession.Close(stream.CloseNormal, "test cleanup")
		hubSession.Close(stream.CloseNormal, "test cleanup")
		stopSessions()
		for name, done := range map[string]<-chan error{"agent": agentDone, "hub": hubDone} {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Errorf("%s session did not stop", name)
			}
		}
		callsDone := make(chan struct{})
		go func() {
			calls.Wait()
			close(callsDone)
		}()
		select {
		case <-callsDone:
		case <-time.After(3 * time.Second):
			t.Error("read request goroutines did not stop")
		}
	})

	select {
	case <-readStarted:
	case <-time.After(time.Second):
		t.Fatal("read request did not reach the agent")
	}
	if got := agentSession.Stats().InflightIn; got == 0 {
		t.Fatal("expected at least one read request in flight before requesting reexec")
	}

	reexecCalled := make(chan time.Time, 1)
	d.reexec = func() { reexecCalled <- time.Now() }
	requestedAt := time.Now()
	d.requestReexec(agentSession)

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(12 * time.Second)
	for {
		select {
		case calledAt := <-reexecCalled:
			elapsed := calledAt.Sub(requestedAt)
			if elapsed < 9*time.Second || elapsed > 11*time.Second {
				t.Fatalf("reexec ran after %s, want the 10 second in-flight deadline", elapsed)
			}
			if got := agentSession.Stats().InflightIn; got == 0 {
				t.Fatal("test failed to keep agent read requests in flight until reexec")
			}
			if !logger.contains("timed out") {
				t.Fatalf("timeout-triggered reexec was not logged: %v", logger.snapshot())
			}
			return
		case <-ticker.C:
			if got := agentSession.Stats().InflightIn; got == 0 {
				t.Fatal("read workload did not keep InflightIn above zero while waiting for reexec")
			}
		case <-deadline:
			t.Fatal("reexec did not run despite the bounded in-flight wait")
		}
	}

}
