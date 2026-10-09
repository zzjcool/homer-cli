package agentd

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
	"github.com/zzjcool/homer-cli/internal/web"
)

func TestWriteTaskInvalidatesStatusFlightAndDrift(t *testing.T) {
	exec := &writeInvalidationExecutor{
		statusStarted: make(chan int, 4),
		releaseScans:  make(chan struct{}),
		pushStarted:   make(chan struct{}),
		releasePush:   make(chan struct{}),
	}
	d := New(Config{AgentID: "write-invalidation", TaskTimeout: time.Second}, exec)

	if _, err := d.statusReport(context.Background()); err != nil {
		t.Fatalf("initial statusReport: %v", err)
	}
	if got := <-exec.statusStarted; got != 1 {
		t.Fatalf("initial status scan = %d, want 1", got)
	}
	if got := d.cachedDrift(); got == nil || got.Push != 1 {
		t.Fatalf("initial drift cache = %+v, want push=1", got)
	}

	preWriteScanDone := make(chan error, 1)
	go func() {
		_, err := d.statusReport(context.Background())
		preWriteScanDone <- err
	}()
	select {
	case call := <-exec.statusStarted:
		if call != 2 {
			t.Fatalf("pre-write status scan = %d, want scan 2", call)
		}
	case <-time.After(time.Second):
		t.Fatal("pre-write status scan did not start")
	}

	agentConn, hubConn := streamtest.Pipe(streamtest.PipeOptions{})
	agentSession := stream.NewSession(agentConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	d.registerHandlers(agentSession)
	hubSession := stream.NewSession(hubConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	agentDone := make(chan error, 1)
	hubDone := make(chan error, 1)
	go func() { agentDone <- agentSession.Run(ctx) }()
	go func() { hubDone <- hubSession.Run(ctx) }()
	var releasePushOnce, releaseScansOnce sync.Once
	releasePush := func() { releasePushOnce.Do(func() { close(exec.releasePush) }) }
	releaseScans := func() { releaseScansOnce.Do(func() { close(exec.releaseScans) }) }

	t.Cleanup(func() {
		releasePush()
		releaseScans()
		cancel()
		agentSession.Close(stream.CloseNormal, "test complete")
		hubSession.Close(stream.CloseNormal, "test complete")
		waitSessionDone(t, agentDone, "agent session")
		waitSessionDone(t, hubDone, "hub session")
	})

	writeDone := make(chan error, 1)
	go func() {
		callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer callCancel()
		_, err := hubSession.Call(callCtx, string(hub.TaskKindPush), hub.TaskOptions{Confirm: true}, stream.WithBudget(2*time.Second))
		writeDone <- err
	}()
	select {
	case <-exec.pushStarted:
	case <-time.After(time.Second):
		t.Fatal("push write task did not reach its executor")
	}
	writeGenBefore, scanEpochBefore := statusFlightCounters(d)
	if writeGenBefore != 0 || scanEpochBefore != 1 {
		t.Fatalf("write-start counters = generation %d epoch %d, want generation 0 epoch 1", writeGenBefore, scanEpochBefore)
	}

	inWriteScanDone := make(chan error, 1)
	go func() {
		_, err := d.statusReport(context.Background())
		inWriteScanDone <- err
	}()
	select {
	case call := <-exec.statusStarted:
		if call != 3 {
			t.Fatalf("status scan during write = %d, want independent scan 3", call)
		}
	case <-time.After(time.Second):
		t.Fatal("status scan during write joined the still-running pre-write flight")
	}

	releasePush()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("push write task: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("push write task did not complete")
	}
	if got := d.cachedDrift(); got != nil {
		t.Fatalf("drift cache after write completion = %+v, want invalidated", got)
	}
	writeGenAfter, scanEpochAfter := statusFlightCounters(d)
	if writeGenAfter != writeGenBefore+1 {
		t.Fatalf("write generation after write = %d, want %d", writeGenAfter, writeGenBefore+1)
	}
	// beginWriteGen invalidates scans when the write starts; completion must
	// advance the epoch again so readers cannot join a still-running in-write scan.
	if scanEpochAfter != scanEpochBefore+1 {
		t.Fatalf("scan epoch after write = %d, want %d", scanEpochAfter, scanEpochBefore+1)
	}
	postWriteDone := make(chan error, 1)
	go func() {
		_, err := d.statusReport(context.Background())
		postWriteDone <- err
	}()
	select {
	case call := <-exec.statusStarted:
		if call != 4 {
			t.Fatalf("post-write status scan = %d, want fresh scan 4 instead of joining the still-running in-write flight", call)
		}
	case <-time.After(time.Second):
		t.Fatal("post-write statusReport reused the in-write flight instead of rescanning")
	}
	select {
	case err := <-postWriteDone:
		if err != nil {
			t.Fatalf("post-write statusReport: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("post-write statusReport did not finish")
	}
	if got := d.cachedDrift(); got == nil || got.Push != 4 {
		t.Fatalf("post-write drift cache = %+v, want recomputed push=4", got)
	}

	releaseScans()
	select {
	case err := <-inWriteScanDone:
		if err != nil {
			t.Fatalf("in-write statusReport: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("status scan started during the write did not finish")
	}
	if got := d.cachedDrift(); got == nil || got.Push != 4 {
		t.Fatalf("stale in-write scan replaced the fresh drift cache: %+v", got)
	}
	select {
	case err := <-preWriteScanDone:
		if err != nil {
			t.Fatalf("pre-write statusReport: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pre-write status scan did not finish")
	}
}

func statusFlightCounters(d *Daemon) (writeGen, scanEpoch uint64) {
	d.statusFlight.mu.Lock()
	defer d.statusFlight.mu.Unlock()
	return d.statusFlight.writeGen, d.statusFlight.scanEpoch
}

func waitSessionDone(t *testing.T, done <-chan error, name string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Errorf("%s did not stop", name)
	}
}

type writeInvalidationExecutor struct {
	statusCalls   atomic.Int32
	statusStarted chan int
	releaseScans  chan struct{}
	pushStarted   chan struct{}
	releasePush   chan struct{}
}

func (e *writeInvalidationExecutor) Status(ctx context.Context) (commands.StatusReport, error) {
	call := int(e.statusCalls.Add(1))
	e.statusStarted <- call
	if call == 2 || call == 3 {
		select {
		case <-e.releaseScans:
		case <-ctx.Done():
			return commands.StatusReport{}, ctx.Err()
		}
	}
	return commands.StatusReport{
		Adapters: []commands.StatusAdapterReport{{ID: "pi", Push: call}},
		Errors:   []string{},
	}, nil
}

func (*writeInvalidationExecutor) Diff(context.Context, web.DiffParams) (string, error) {
	return "", nil
}

func (e *writeInvalidationExecutor) Push(ctx context.Context, _ bool, _ []string, _, _ bool) (commands.PushReport, error) {
	close(e.pushStarted)
	select {
	case <-e.releasePush:
		return commands.PushReport{OK: true, Status: commands.PushStatusPushed}, nil
	case <-ctx.Done():
		return commands.PushReport{}, ctx.Err()
	}
}

func (*writeInvalidationExecutor) Pull(context.Context, bool, []string, bool) (commands.PullReport, error) {
	return commands.PullReport{OK: true, Status: commands.PullStatusApplied}, nil
}

var _ Executor = (*writeInvalidationExecutor)(nil)
