package hub

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
)

func TestDispatcherQueuedCallAfterDetachReturnsOffline(t *testing.T) {
	registry := NewRegistry()
	const calls = agentCallConcurrency + 1
	agent := newDispatcherAgent(t, registry, "offline-while-queued", string(TaskKindStatus))
	var started atomic.Int32
	startedCalls := make(chan struct{}, calls)
	agent.Handle(string(TaskKindStatus), func(ctx context.Context, _ *stream.Request) (any, error) {
		callNumber := started.Add(1)
		startedCalls <- struct{}{}
		if callNumber > agentCallConcurrency {
			return map[string]any{"unexpectedlyDispatched": true}, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	agent.Run()
	dispatcher := NewDispatcher(registry, "")
	type outcome struct {
		index int
		err   error
	}
	outcomes := make(chan outcome, calls)
	ctxs := make([]context.Context, calls)
	cancels := make([]context.CancelFunc, calls)
	for index := range ctxs {
		ctxs[index], cancels[index] = context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancels[index]()
	}
	var wg sync.WaitGroup
	launch := func(index int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := dispatcher.AgentStatus(ctxs[index], "offline-while-queued")
			outcomes <- outcome{index: index, err: err}
		}()
	}
	for index := 0; index < agentCallConcurrency; index++ {
		launch(index)
	}
	for index := 0; index < agentCallConcurrency; index++ {
		select {
		case <-startedCalls:
		case <-time.After(2 * time.Second):
			agent.hub.Close(stream.CloseGoingAway, "test cleanup")
			wg.Wait()
			var failures []string
			for len(outcomes) > 0 {
				result := <-outcomes
				failures = append(failures, fmt.Sprintf("call[%d]=%v", result.index, result.err))
			}
			t.Fatalf("only %d status handlers started, want %d; outcomes=%v calls=%+v hubErr=%v hubStats=%+v agentErr=%v agentStats=%+v",
				index, agentCallConcurrency, failures, agent.Calls(), agent.hub.Err(), agent.hub.Stats(), agent.session.Err(), agent.session.Stats())
		}
	}
	launch(calls - 1)
	waitForDispatcherQueue(t, dispatcher, "offline-while-queued", 1)
	if got := activeAgentCalls(dispatcher, "offline-while-queued"); got != agentCallConcurrency {
		t.Fatalf("active dispatcher tokens = %d, want %d", got, agentCallConcurrency)
	}
	if got := started.Load(); got != agentCallConcurrency {
		t.Fatalf("status handlers started before queued call acquired a token = %d, want %d", got, agentCallConcurrency)
	}

	// The queued caller has passed its first liveness check and is waiting on
	// the per-agent semaphore. Detach and close the actual session before any
	// token is released; when the queued call gets a token, the second liveness
	// check must reject it instead of sending to a dead session.
	registry.Detach("offline-while-queued", agent.hub)
	for index := 0; index < agentCallConcurrency; index++ {
		cancels[index]()
	}
	wg.Wait()
	close(outcomes)
	results := make([]error, calls)
	for result := range outcomes {
		results[result.index] = result.err
	}
	queuedErr := results[calls-1]
	if !hasAgentCode(queuedErr, "agent-offline", http.StatusServiceUnavailable) {
		t.Fatalf("queued call after session detach = %v, want agent-offline", queuedErr)
	}
	if got := started.Load(); got != agentCallConcurrency {
		t.Fatalf("detached session received %d new call(s), want none (total handler starts %d)", got-agentCallConcurrency, got)
	}
}
