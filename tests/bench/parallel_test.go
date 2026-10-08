package bench

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/agentd"
	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/web"
)

// BenchmarkFourParallel measures four real agentd daemons connected to an
// AgentHub over loopback WebSockets. The injected Executor intentionally delays
// every status call; the batch should take about one task delay, not four.
// Each benchmark iteration also enforces the P6 ≤1.3× serial-call bound.
func BenchmarkFourParallel(b *testing.B) {
	const delay = 120 * time.Millisecond
	fixture := newParallelFixture(b, delay)
	defer fixture.close(b)

	slowestSingle := time.Duration(0)
	for _, id := range fixture.agentIDs {
		started := time.Now()
		if _, err := fixture.dispatcher.AgentStatus(context.Background(), id); err != nil {
			b.Fatalf("serial status call to %s: %v", id, err)
		}
		if elapsed := time.Since(started); elapsed > slowestSingle {
			slowestSingle = elapsed
		}
	}
	if slowestSingle <= 0 {
		b.Fatal("single status call did not run")
	}

	b.ReportMetric(float64(slowestSingle.Microseconds()), "slowest-single-us")
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		started := time.Now()
		errorsFound := parallelStatusCalls(fixture.dispatcher, fixture.agentIDs)
		elapsed := time.Since(started)
		if len(errorsFound) != 0 {
			b.Fatalf("parallel iteration %d returned %d error(s): %v", iteration, len(errorsFound), errorsFound)
		}
		if elapsed > slowestSingle*13/10 {
			b.Fatalf("four parallel tasks took %s; slowest single task took %s (limit %s)", elapsed, slowestSingle, slowestSingle*13/10)
		}
	}
}

func parallelStatusCalls(dispatcher *hub.Dispatcher, agentIDs []string) []error {
	results := make(chan error, len(agentIDs))
	var workers sync.WaitGroup
	workers.Add(len(agentIDs))
	for _, id := range agentIDs {
		id := id
		go func() {
			defer workers.Done()
			_, err := dispatcher.AgentStatus(context.Background(), id)
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	var failures []error
	for err := range results {
		if err != nil {
			failures = append(failures, err)
		}
	}
	return failures
}

type delayedExecutor struct{ delay time.Duration }

func (e delayedExecutor) wait(ctx context.Context) error {
	timer := time.NewTimer(e.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e delayedExecutor) Status(ctx context.Context) (commands.StatusReport, error) {
	if err := e.wait(ctx); err != nil {
		return commands.StatusReport{}, err
	}
	return commands.StatusReport{
		Adapters: []commands.StatusAdapterReport{{ID: "benchmark", Categories: []commands.StatusCategoryReport{}}},
		Errors:   []string{},
	}, nil
}

func (e delayedExecutor) Diff(ctx context.Context, _ web.DiffParams) (string, error) {
	if err := e.wait(ctx); err != nil {
		return "", err
	}
	return "benchmark diff", nil
}

func (e delayedExecutor) Push(ctx context.Context, _ bool, _ []string, _, _ bool) (commands.PushReport, error) {
	if err := e.wait(ctx); err != nil {
		return commands.PushReport{}, err
	}
	return commands.PushReport{OK: true, Status: commands.PushStatusNoDrift, Errors: []string{}}, nil
}

func (e delayedExecutor) Pull(ctx context.Context, _ bool, _ []string, _ bool) (commands.PullReport, error) {
	if err := e.wait(ctx); err != nil {
		return commands.PullReport{}, err
	}
	return commands.PullReport{OK: true, Status: commands.PullStatusNoDrift}, nil
}

type parallelFixture struct {
	server     *http.Server
	listener   net.Listener
	registry   *hub.Registry
	dispatcher *hub.Dispatcher
	agentHub   *hub.AgentHub
	serverDone chan error
	stops      []context.CancelFunc
	runs       []chan error
	agentIDs   []string
}

func newParallelFixture(b *testing.B, delay time.Duration) *parallelFixture {
	b.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	logger := log.New(io.Discard, "", 0)
	registry := hub.NewRegistry()
	enrollment := hub.NewEnrollmentManager()
	const token = "parallel-benchmark-token"
	auth := &hub.Authenticator{Token: token, Enrollment: enrollment}
	agentHub := hub.NewAgentHub(registry, auth, enrollment, hub.HubOptions{Logger: logger})
	server := &http.Server{Handler: agentHub, ReadHeaderTimeout: time.Second}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	fixture := &parallelFixture{
		server: server, listener: listener, registry: registry,
		dispatcher: hub.NewDispatcher(registry, token), agentHub: agentHub,
		serverDone: serverDone,
		agentIDs:   []string{"bench-0", "bench-1", "bench-2", "bench-3"},
	}
	baseURL := "http://" + listener.Addr().String()

	for _, id := range fixture.agentIDs {
		home := b.TempDir()
		daemon := agentd.New(agentd.Config{
			Home: home, HomerHome: filepath.Join(home, "homer"), HubURL: baseURL,
			AgentID: id, Token: token,
			Stream: stream.Options{Logger: logger},
		}, delayedExecutor{delay: delay})
		runCtx, stop := context.WithCancel(context.Background())
		fixture.stops = append(fixture.stops, stop)
		runDone := make(chan error, 1)
		fixture.runs = append(fixture.runs, runDone)
		go func() { runDone <- daemon.Run(runCtx) }()
	}

	deadline := time.Now().Add(10 * time.Second)
	for len(fixture.dispatcher.ListAgents()) < len(fixture.agentIDs) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(fixture.dispatcher.ListAgents()) != len(fixture.agentIDs) {
		fixture.close(b)
		b.Fatalf("only %d of %d benchmark agents registered", len(fixture.dispatcher.ListAgents()), len(fixture.agentIDs))
	}
	// Hello schedules a cached drift scan in each daemon. A concurrent status
	// warm-up joins/drains those scans before the serial baseline is measured.
	if failures := parallelStatusCalls(fixture.dispatcher, fixture.agentIDs); len(failures) != 0 {
		fixture.close(b)
		b.Fatalf("benchmark warm-up status: %v", failures)
	}
	return fixture
}

func (f *parallelFixture) close(b *testing.B) {
	if f == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if f.agentHub != nil {
		if err := f.agentHub.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
			b.Errorf("shut down benchmark AgentHub: %v", err)
		}
	}
	if f.server != nil {
		_ = f.server.Shutdown(ctx)
	}
	if f.listener != nil {
		_ = f.listener.Close()
	}
	for _, stop := range f.stops {
		stop()
	}
	for _, done := range f.runs {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			b.Errorf("benchmark agent daemon did not stop")
		}
	}
	if f.serverDone != nil {
		select {
		case err := <-f.serverDone:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				b.Errorf("benchmark HTTP server: %v", err)
			}
		case <-time.After(2 * time.Second):
			b.Errorf("benchmark HTTP server did not stop")
		}
	}
}
