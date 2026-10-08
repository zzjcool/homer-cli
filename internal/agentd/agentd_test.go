package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/keyring"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
	"github.com/zzjcool/homer-cli/internal/stream/wsconn"
	"github.com/zzjcool/homer-cli/internal/toolctl"
	"github.com/zzjcool/homer-cli/internal/web"
)

func TestDefaultAgentID(t *testing.T) {
	first, second := DefaultAgentID(), DefaultAgentID()
	if first == "" || second == "" || first == second {
		t.Fatalf("DefaultAgentID() = %q, %q; want distinct non-empty IDs", first, second)
	}
	for _, id := range []string{first, second} {
		for _, r := range id {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				t.Fatalf("DefaultAgentID() = %q, contains invalid character %q", id, r)
			}
		}
	}
}

func TestDaemonStreamHelloAndStatusHandler(t *testing.T) {
	home := t.TempDir()
	logger := &captureLogger{}
	exec := &testExecutor{}
	d := New(Config{
		Home:       home,
		HubURL:     "http://hub.example/base",
		AgentID:    "agent-test",
		EnrollCode: "hr_once",
		Stream: stream.Options{
			Logger:       logger,
			PingInterval: time.Hour,
			PingTimeout:  2 * time.Hour,
		},
	}, exec)
	peerSessions := make(chan *stream.Session, 1)
	helloSeen := make(chan hub.HelloParams, 1)
	d.dialStream = func(ctx context.Context, gotURL string, options wsconn.DialOptions) (stream.Conn, *http.Response, error) {
		if gotURL != "ws://hub.example/base/agent/v1/stream" {
			t.Errorf("dial URL = %q", gotURL)
		}
		if got := options.Header.Get("Authorization"); got != "Bearer hr_once" {
			t.Errorf("Authorization = %q, want enrollment code", got)
		}
		agentConn, hubConn := streamtest.Pipe(streamtest.PipeOptions{})
		peer := stream.NewSession(hubConn, stream.Options{Logger: logger, PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
		peer.Handle(hub.MethodHello, func(_ context.Context, req *stream.Request) (any, error) {
			var hello hub.HelloParams
			if err := req.Decode(&hello); err != nil {
				return nil, err
			}
			helloSeen <- hello
			return hub.WelcomeResult{
				Proto:          stream.ProtocolVersion,
				HubVersion:     "test-hub",
				InstanceID:     "hub-1",
				AgentSecret:    "agent-secret",
				PingIntervalMs: 60_000,
				PingTimeoutMs:  120_000,
				MaxFrame:       stream.DefaultMaxFrame,
			}, nil
		})
		go func() { _ = peer.Run(ctx) }()
		peerSessions <- peer
		return agentConn, nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	peer := <-peerSessions
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("Run() = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop")
		}
		peer.Close(stream.CloseGoingAway, "test complete")
	})

	select {
	case hello := <-helloSeen:
		if hello.AgentID != "agent-test" || hello.Proto != stream.ProtocolVersion || hello.Hostname == "" {
			t.Fatalf("hello = %+v", hello)
		}
		if len(hello.Caps) == 0 || !containsString(hello.Caps, hub.MethodInspect) {
			t.Fatalf("hello capabilities = %v", hello.Caps)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not send hello")
	}
	var persisted AgentConfig
	var ok bool
	waitFor(t, time.Second, func() bool {
		persisted, ok = LoadAgentConfig(home)
		return ok && persisted.AgentSecret == "agent-secret"
	})
	if persisted.HubURL != "http://hub.example/base" {
		t.Fatalf("agent config after welcome = %+v", persisted)
	}
	if got := d.bearerCredential(); got != "agent-secret" {
		t.Fatalf("snapshot/data-plane credential = %q, want returned per-agent secret", got)
	}

	callCtx, callCancel := context.WithTimeout(context.Background(), time.Second)
	defer callCancel()
	payload, err := peer.Call(callCtx, string(hub.TaskKindStatus), hub.TaskOptions{}, stream.WithBudget(time.Second))
	if err != nil {
		t.Fatalf("status call: %v", err)
	}
	var status commands.StatusReport
	if err := json.Unmarshal(payload, &status); err != nil {
		t.Fatalf("decode status %s: %v", payload, err)
	}
	if len(status.Adapters) != 1 || status.Adapters[0].ID != "pi" {
		t.Fatalf("status = %+v", status)
	}
}

func TestDaemonContextCancellationReturnsNil(t *testing.T) {
	d := New(Config{HubURL: "http://hub.example", AgentID: "cancel-agent"}, &testExecutor{})
	var attempts atomic.Int32
	d.dialStream = func(context.Context, string, wsconn.DialOptions) (stream.Conn, *http.Response, error) {
		attempts.Add(1)
		return nil, nil, errors.New("network down")
	}
	d.retryWait = func(ctx context.Context, _ time.Duration) bool {
		<-ctx.Done()
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitFor(t, time.Second, func() bool { return attempts.Load() == 1 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() after context cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
}

func TestDaemonReconnectLoop(t *testing.T) {
	logger := &captureLogger{}
	d := New(Config{
		HubURL:  "http://hub.example",
		AgentID: "reconnect-agent",
		Backoff: stream.Backoff{Base: time.Millisecond, Max: time.Millisecond, Factor: 2},
		Stream:  stream.Options{Logger: logger, PingInterval: time.Hour, PingTimeout: 2 * time.Hour},
	}, &testExecutor{})
	var attempts atomic.Int32
	connected := make(chan *stream.Session, 1)
	d.dialStream = func(ctx context.Context, _ string, _ wsconn.DialOptions) (stream.Conn, *http.Response, error) {
		if attempts.Add(1) == 1 {
			return nil, nil, errors.New("temporary network error")
		}
		agentConn, hubConn := streamtest.Pipe(streamtest.PipeOptions{})
		peer := newTestHubSession(ctx, hubConn, func(hello hub.HelloParams) hub.WelcomeResult {
			return hub.WelcomeResult{Proto: hello.Proto, HubVersion: "hub", PingIntervalMs: 60_000, PingTimeoutMs: 120_000}
		})
		connected <- peer
		return agentConn, nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	peer := <-connected
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop after reconnect")
	}
	peer.Close(stream.CloseGoingAway, "test complete")
	if attempts.Load() < 2 {
		t.Fatalf("dial attempts = %d, want a retry", attempts.Load())
	}
	if !logger.contains("temporary network error") {
		t.Fatalf("dial retry was not logged: %v", logger.snapshot())
	}
}

func TestReconnectPolicy(t *testing.T) {
	d := New(Config{
		HubURL:  "http://hub.example",
		Backoff: stream.Backoff{Base: time.Second, Max: time.Minute, Factor: 2},
	}, &testExecutor{})
	unsupported := &stream.Error{Code: stream.CodeUnsupported, Message: "unsupported-version"}
	tests := []struct {
		name       string
		err        error
		status     int
		body       string
		wantDelay  time.Duration
		wantKey    string
		wantAlways bool
		wantText   string
	}{
		{name: "dial failure", err: errors.New("network"), wantDelay: time.Second, wantKey: "ws-dial-failed"},
		{name: "401", err: &wsconn.DialError{Status: http.StatusUnauthorized}, wantDelay: 5 * time.Minute, wantKey: "ws-unauthorized", wantText: "凭证无效/接入码已用"},
		{name: "410", err: &wsconn.DialError{Status: http.StatusGone, Body: `{"error":{"message":"protocol removed"}}`}, wantDelay: 5 * time.Minute, wantKey: "ws-protocol-removed", wantText: "protocol removed"},
		{name: "unsupported handshake", err: &wsconn.DialError{Status: http.StatusBadRequest, Body: "unsupported-version"}, wantDelay: 5 * time.Minute, wantKey: "ws-unsupported-version"},
		{name: "hub restart", err: &stream.CloseError{Code: stream.CloseGoingAway}, wantDelay: time.Second, wantKey: "ws-reconnect", wantAlways: true},
		{name: "EOF", err: io.EOF, wantDelay: time.Second, wantKey: "ws-reconnect", wantAlways: true},
		{name: "heartbeat", err: &stream.CloseError{Code: stream.CloseHeartbeat, Reason: "ping timeout"}, wantDelay: time.Second, wantKey: "ws-reconnect", wantAlways: true},
		{name: "superseded", err: &stream.CloseError{Code: stream.CloseSuperseded}, wantDelay: 30 * time.Second, wantKey: "ws-superseded", wantAlways: true, wantText: "同 id 的另一进程已接管"},
		{name: "revoked", err: &stream.CloseError{Code: stream.CloseRevoked}, wantDelay: 5 * time.Minute, wantKey: "ws-revoked", wantText: "重新接入"},
		{name: "removed", err: &stream.CloseError{Code: stream.CloseRemoved}, wantDelay: 5 * time.Minute, wantKey: "ws-removed", wantText: "重新接入"},
		{name: "hello unsupported", err: unsupported, wantDelay: 5 * time.Minute, wantKey: "ws-unsupported-version", wantText: "升级"},
		{name: "protocol close unsupported", err: &stream.CloseError{Code: stream.CloseProtocol, Reason: "unsupported-version"}, wantDelay: 5 * time.Minute, wantKey: "ws-unsupported-version", wantText: "升级"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := d.retryFor(tt.err, 0)
			if tt.status != 0 {
				plan = d.retryFor(&wsconn.DialError{Status: tt.status, Body: tt.body}, 0)
			}
			if plan.delay != tt.wantDelay || plan.key != tt.wantKey || plan.always != tt.wantAlways {
				t.Fatalf("retry plan = %+v, want delay=%v key=%q always=%v", plan, tt.wantDelay, tt.wantKey, tt.wantAlways)
			}
			if tt.wantText != "" && !strings.Contains(plan.line, tt.wantText) {
				t.Fatalf("retry line %q does not contain %q", plan.line, tt.wantText)
			}
		})
	}
	if got := d.retryFor(errors.New("network"), 2).delay; got != 4*time.Second {
		t.Fatalf("third transient retry delay = %v, want 4s", got)
	}
}

func TestAgentdWSUnauthorizedIsLogged(t *testing.T) {
	logger := &captureLogger{}
	d := New(Config{HubURL: "http://hub.example", AgentID: "unauthorized", Stream: stream.Options{Logger: logger}}, &testExecutor{})
	d.dialStream = func(context.Context, string, wsconn.DialOptions) (stream.Conn, *http.Response, error) {
		return nil, nil, &wsconn.DialError{Status: http.StatusUnauthorized}
	}
	waited := make(chan time.Duration, 1)
	d.retryWait = func(ctx context.Context, delay time.Duration) bool {
		waited <- delay
		<-ctx.Done()
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	select {
	case delay := <-waited:
		if delay != 5*time.Minute {
			t.Fatalf("401 retry delay = %v, want 5m", delay)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not enter the 401 retry path")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if !logger.contains("401") || !logger.contains("凭证无效/接入码已用") {
		t.Fatalf("401 retry is not visibly logged: %v", logger.snapshot())
	}
}

func TestTaskClass(t *testing.T) {
	readCases := []struct {
		method string
		secret string
	}{
		{method: string(hub.TaskKindStatus)},
		{method: string(hub.TaskKindDiff)},
		{method: hub.MethodInspect},
		{method: string(hub.TaskKindSecret), secret: "list"},
		{method: string(hub.TaskKindSecret), secret: "exists"},
		{method: string(hub.TaskKindSecret), secret: "status"},
	}
	for _, tc := range readCases {
		options := hub.TaskOptions{SecretAction: tc.secret}
		if got := classifyTask(tc.method, options); got != taskClassRead {
			t.Errorf("classifyTask(%q, secret=%q) = %v, want read", tc.method, tc.secret, got)
		}
	}
	for _, method := range []string{
		string(hub.TaskKindPush), string(hub.TaskKindPull), string(hub.TaskKindSSHKey),
		string(hub.TaskKindUpgrade), string(hub.TaskKindToolUpgrade), "future-method",
	} {
		if got := classifyTask(method, hub.TaskOptions{}); got != taskClassWrite {
			t.Errorf("classifyTask(%q) = %v, want write", method, got)
		}
	}
	for _, action := range []string{"create", "encrypt", "unlock", "save", "push", "pull", "unknown"} {
		if got := classifyTask(string(hub.TaskKindSecret), hub.TaskOptions{SecretAction: action}); got != taskClassWrite {
			t.Errorf("secret action %q classified as %v, want write", action, got)
		}
	}
}

func TestTaskClassWriteGateFIFOAndCancelable(t *testing.T) {
	gate := newTaskGate()
	firstRelease, queued, err := gate.acquire(context.Background(), nil)
	if err != nil || queued {
		t.Fatalf("first acquire = queued %v err %v", queued, err)
	}
	var orderMu sync.Mutex
	var order []int
	startWaiter := func(id int, ctx context.Context) <-chan error {
		done := make(chan error, 1)
		go func() {
			release, _, err := gate.acquire(ctx, func() {})
			if err == nil {
				orderMu.Lock()
				order = append(order, id)
				orderMu.Unlock()
				release()
			}
			done <- err
		}()
		return done
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	canceled := startWaiter(2, cancelCtx)
	waitFor(t, time.Second, func() bool { return gate.waiting() == 1 })
	second := startWaiter(3, context.Background())
	waitFor(t, time.Second, func() bool { return gate.waiting() == 2 })
	cancel()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v", err)
	}
	firstRelease()
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	orderMu.Lock()
	defer orderMu.Unlock()
	if fmt.Sprint(order) != "[3]" {
		t.Fatalf("gate acquisition order = %v, want surviving FIFO waiter [3]", order)
	}
}

func (s *statusFlight) waiting() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int
	for _, call := range s.flights {
		total += call.waiters
	}
	return total
}

func TestStatusFlightPanicReturnsError(t *testing.T) {
	d := New(Config{AgentID: "panic-flight"}, nil)
	started := make(chan struct{})
	scan := func(context.Context) (commands.StatusReport, error) {
		close(started)
		panic("scan exploded")
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := d.statusFlightDo(context.Background(), scan)
			results <- err
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("panic scan did not start")
	}
	for range 2 {
		select {
		case err := <-results:
			if err == nil || !strings.Contains(err.Error(), "scan exploded") {
				t.Fatalf("panic scan error = %v, want recovered scan error", err)
			}
		case <-time.After(time.Second):
			t.Fatal("status-flight waiter did not receive a panic error")
		}
	}
}

func TestStatusFlight(t *testing.T) {
	exec := &flightExecutor{started: make(chan int, 4), releases: make(chan struct{}, 4)}
	d := New(Config{AgentID: "flight-agent"}, exec)
	const callers = 12
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			_, err := d.statusReport(context.Background())
			results <- err
		}()
	}
	select {
	case n := <-exec.started:
		if n != 1 {
			t.Fatalf("first scan number = %d", n)
		}
	case <-time.After(time.Second):
		t.Fatal("status scan did not start")
	}
	waitFor(t, time.Second, func() bool { return d.statusFlight.waiting() == callers })
	select {
	case n := <-exec.started:
		t.Fatalf("singleflight started %d scans before release", n)
	default:
	}
	exec.releases <- struct{}{}
	for i := 0; i < callers; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := exec.calls.Load(); got != 1 {
		t.Fatalf("status scan calls = %d, want one", got)
	}

	mixed := &flightExecutor{started: make(chan int, 2), releases: make(chan struct{}, 2)}
	d = New(Config{AgentID: "status-inspect-flight"}, mixed)
	statusDone := make(chan error, 1)
	inspectDone := make(chan error, 1)
	go func() { _, err := d.statusReport(context.Background()); statusDone <- err }()
	go func() {
		result, err := d.runTaskCommand(context.Background(), &stream.Request{
			Method: hub.MethodInspect, Params: json.RawMessage(`{}`),
		}, hub.TaskOptions{})
		if err == nil {
			if inspect, ok := result.(web.InspectResult); !ok || len(inspect.Status.Adapters) != 1 {
				err = fmt.Errorf("unexpected inspect result %#v", result)
			}
		}
		inspectDone <- err
	}()
	waitFor(t, time.Second, func() bool { return d.statusFlight.waiting() == 2 })
	mixed.releases <- struct{}{}
	if err := <-statusDone; err != nil {
		t.Fatalf("status caller: %v", err)
	}
	if err := <-inspectDone; err != nil {
		t.Fatalf("inspect caller: %v", err)
	}
	if calls := mixed.calls.Load(); calls != 1 {
		t.Fatalf("concurrent status+inspect scans = %d, want one", calls)
	}

	// A scan that began before a completed write must not be joined by a new
	// reader or overwrite the post-write drift cache.
	blocked := &generationExecutor{
		firstStarted: make(chan struct{}), secondStarted: make(chan struct{}), releaseFirst: make(chan struct{}),
	}
	d = New(Config{AgentID: "generation-agent"}, blocked)
	first := make(chan error, 1)
	go func() { _, err := d.statusReport(context.Background()); first <- err }()
	<-blocked.firstStarted
	d.bumpWriteGen()
	second := make(chan error, 1)
	go func() { _, err := d.statusReport(context.Background()); second <- err }()
	select {
	case <-blocked.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("post-write scan joined the pre-write scan")
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	close(blocked.releaseFirst)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if got := d.cachedDrift(); got == nil || got.Push != 2 {
		t.Fatalf("cached drift after stale scan completed = %+v, want generation 2", got)
	}

	// Reads arriving after a write begins must not join the pre-write scan;
	// they share the post-write epoch and publish its current result.
	blocked = &generationExecutor{
		firstStarted: make(chan struct{}), secondStarted: make(chan struct{}), releaseFirst: make(chan struct{}),
	}
	d = New(Config{AgentID: "writing-generation"}, blocked)
	first = make(chan error, 1)
	go func() { _, err := d.statusReport(context.Background()); first <- err }()
	<-blocked.firstStarted
	release, _, err := d.writeGate.acquire(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	d.beginWriteGen()
	second = make(chan error, 1)
	go func() { _, err := d.statusReport(context.Background()); second <- err }()
	select {
	case <-blocked.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("reader arriving during a write joined the pre-write scan")
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	d.bumpWriteGen()
	release()
	close(blocked.releaseFirst)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if got := d.cachedDrift(); got == nil || got.Push != 2 {
		t.Fatalf("cached drift after in-progress write scan = %+v, want current result", got)
	}
}

func TestHandlers(t *testing.T) {
	d := New(Config{AgentID: "handler-agent", TaskTimeout: time.Second}, &testExecutor{})
	agent, hubPeer := streamtest.Pipe(streamtest.PipeOptions{})
	agentSession := stream.NewSession(agent, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	d.registerHandlers(agentSession)
	hubSession := stream.NewSession(hubPeer, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{}, 2)
	go func() { _ = agentSession.Run(ctx); runDone <- struct{}{} }()
	go func() { _ = hubSession.Run(ctx); runDone <- struct{}{} }()
	defer func() {
		agentSession.Close(stream.CloseNormal, "test done")
		hubSession.Close(stream.CloseNormal, "test done")
		for i := 0; i < 2; i++ {
			select {
			case <-runDone:
			case <-time.After(time.Second):
				t.Error("test session did not stop")
			}
		}
	}()

	callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer callCancel()
	payload, err := hubSession.Call(callCtx, string(hub.TaskKindStatus), hub.TaskOptions{}, stream.WithBudget(time.Second))
	if err != nil {
		t.Fatalf("status handler: %v", err)
	}
	var status commands.StatusReport
	if err := json.Unmarshal(payload, &status); err != nil || len(status.Adapters) != 1 {
		t.Fatalf("status payload %s: %v", payload, err)
	}

	payload, err = hubSession.Call(callCtx, hub.MethodInspect, web.InspectParams{}, stream.WithBudget(time.Second))
	if err != nil {
		t.Fatalf("inspect placeholder: %v", err)
	}
	var inspect web.InspectResult
	if err := json.Unmarshal(payload, &inspect); err != nil || len(inspect.Status.Adapters) != 1 {
		t.Fatalf("inspect payload %s: %v", payload, err)
	}

	d.cfg.HomerHome = t.TempDir()
	command, _ := json.Marshal(keyring.Command{Action: "list"})
	payload, err = hubSession.Call(callCtx, string(hub.TaskKindSecret), hub.TaskOptions{SecretAction: "list", SecretPayload: command}, stream.WithBudget(time.Second))
	if err != nil {
		t.Fatalf("secret handler: %v", err)
	}
	var secret keyring.Result
	if err := json.Unmarshal(payload, &secret); err != nil || !secret.OK {
		t.Fatalf("secret payload %s: %v", payload, err)
	}
}

func TestHandlersUpgradeFlushBeforeReexec(t *testing.T) {
	agentConn, peerConn := streamtest.Pipe(streamtest.PipeOptions{})
	d := New(Config{AgentID: "reexec-handler"}, &testExecutor{})
	var hubSession *stream.Session
	var agentSession *stream.Session
	reexecCalled := make(chan struct{}, 2)
	d.reexec = func() {
		if agentSession.Stats().FramesOut != 1 {
			t.Errorf("reexec ran before the upgrade response write completed: framesOut=%d", agentSession.Stats().FramesOut)
		}
		reexecCalled <- struct{}{}
	}
	agentSession = stream.NewSession(&streamtest.FaultConn{Conn: agentConn, WriteDelay: 50 * time.Millisecond}, stream.Options{
		PingInterval: time.Hour,
		PingTimeout:  2 * time.Hour,
	})
	agentSession.Handle("test.upgrade", func(_ context.Context, req *stream.Request) (any, error) {
		d.requestReexec(req.Session)
		d.requestReexec(req.Session) // repeated triggers must still reexec only once
		return map[string]string{"result": "upgraded"}, nil
	})
	hubSession = stream.NewSession(peerConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = agentSession.Run(ctx) }()
	go func() { _ = hubSession.Run(ctx) }()
	callCtx, callCancel := context.WithTimeout(context.Background(), time.Second)
	defer callCancel()
	payload, err := hubSession.Call(callCtx, "test.upgrade", struct{}{}, stream.WithBudget(time.Second))
	if err != nil {
		t.Fatalf("upgrade request: %v", err)
	}
	if string(payload) != `{"result":"upgraded"}` {
		t.Fatalf("upgrade response = %s", payload)
	}
	select {
	case <-reexecCalled:
	case <-time.After(time.Second):
		t.Fatal("reexec was not called after Flush")
	}
	select {
	case <-reexecCalled:
		t.Fatal("duplicate upgrade event triggered multiple reexecs")
	default:
	}
	agentSession.Close(stream.CloseNormal, "test done")
	hubSession.Close(stream.CloseNormal, "test done")
}

func TestHandlersCanceledWriteRetainsFIFO(t *testing.T) {
	exec := &testExecutor{pushStarted: make(chan string, 2), pushRelease: make(chan struct{}, 2)}
	d := New(Config{AgentID: "canceled-write", TaskTimeout: time.Second}, exec)
	agentConn, peerConn := streamtest.Pipe(streamtest.PipeOptions{})
	agentSession := stream.NewSession(agentConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	d.registerHandlers(agentSession)
	hubSession := stream.NewSession(peerConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = agentSession.Run(ctx) }()
	go func() { _ = hubSession.Run(ctx) }()
	defer func() {
		agentSession.Close(stream.CloseNormal, "test done")
		hubSession.Close(stream.CloseNormal, "test done")
	}()

	firstCtx, firstCancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer firstCancel()
	start := time.Now()
	_, err := hubSession.Call(firstCtx, string(hub.TaskKindPush), hub.TaskOptions{Adapters: []string{"cancel-first"}}, stream.WithBudget(35*time.Millisecond))
	if err == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("canceled write returned after %v with %v", time.Since(start), err)
	}
	if got := <-exec.pushStarted; got != "cancel-first" {
		t.Fatalf("first write = %q", got)
	}

	queued := make(chan string, 2)
	secondDone := make(chan error, 1)
	go func() {
		callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer callCancel()
		_, err := hubSession.Call(callCtx, string(hub.TaskKindPush), hub.TaskOptions{Adapters: []string{"second"}},
			stream.WithBudget(2*time.Second), stream.WithProgress(func(raw json.RawMessage) {
				var progress struct {
					Stage string `json:"stage"`
				}
				if json.Unmarshal(raw, &progress) == nil {
					queued <- progress.Stage
				}
			}),
		)
		secondDone <- err
	}()
	select {
	case stage := <-queued:
		if stage != "queued" {
			t.Fatalf("second write progress = %q, want queued", stage)
		}
	case <-time.After(time.Second):
		t.Fatal("second write did not report queued")
	}
	select {
	case got := <-exec.pushStarted:
		t.Fatalf("second write %q began while canceled write still ran", got)
	case <-time.After(50 * time.Millisecond):
	}

	// The command is not context-aware and completes naturally after the
	// caller is gone. Only then may the FIFO write lock pass to the next task.
	exec.pushRelease <- struct{}{}
	if got := <-exec.pushStarted; got != "second" {
		t.Fatalf("second write = %q, want second", got)
	}
	exec.pushRelease <- struct{}{}
	if err := <-secondDone; err != nil {
		t.Fatalf("second write: %v", err)
	}
	if got := hubSession.Stats().Pending; got != 0 {
		t.Fatalf("pending calls after write completion = %d, want no duplicate canceled response", got)
	}
}

func TestHandlersCancellationReturnsPromptly(t *testing.T) {
	exec := &testExecutor{blockDiff: make(chan struct{})}
	d := New(Config{AgentID: "cancel-handler", TaskTimeout: time.Second}, exec)
	agent, peer := streamtest.Pipe(streamtest.PipeOptions{})
	agentSession := stream.NewSession(agent, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	d.registerHandlers(agentSession)
	hubSession := stream.NewSession(peer, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = agentSession.Run(ctx) }()
	go func() { _ = hubSession.Run(ctx) }()
	callCtx, callCancel := context.WithTimeout(context.Background(), time.Second)
	defer callCancel()
	start := time.Now()
	_, err := hubSession.Call(callCtx, string(hub.TaskKindDiff), hub.TaskOptions{}, stream.WithBudget(25*time.Millisecond))
	if err == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("canceled non-context-aware operation returned after %v with %v", time.Since(start), err)
	}
	close(exec.blockDiff)
	hubSession.Close(stream.CloseNormal, "test done")
	agentSession.Close(stream.CloseNormal, "test done")
}

func TestHandlersReadLimitAndQueuedWriteFIFO(t *testing.T) {
	exec := &testExecutor{
		diffStarted: make(chan struct{}, 5),
		diffRelease: make(chan struct{}),
		pushStarted: make(chan string, 2),
		pushRelease: make(chan struct{}, 2),
	}
	d := New(Config{AgentID: "task-scheduling"}, exec)
	agentConn, peerConn := streamtest.Pipe(streamtest.PipeOptions{})
	agentSession := stream.NewSession(agentConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	d.registerHandlers(agentSession)
	hubSession := stream.NewSession(peerConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = agentSession.Run(ctx) }()
	go func() { _ = hubSession.Run(ctx) }()
	defer func() {
		agentSession.Close(stream.CloseNormal, "test done")
		hubSession.Close(stream.CloseNormal, "test done")
	}()

	call := func(method string, params any, progress func(json.RawMessage)) <-chan error {
		done := make(chan error, 1)
		go func() {
			callCtx, callCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer callCancel()
			_, err := hubSession.Call(callCtx, method, params, stream.WithBudget(3*time.Second), stream.WithProgress(progress))
			done <- err
		}()
		return done
	}

	var reads []<-chan error
	for i := 0; i < 4; i++ {
		reads = append(reads, call(string(hub.TaskKindDiff), hub.TaskOptions{Adapter: fmt.Sprintf("read-%d", i)}, nil))
	}
	for i := 0; i < 4; i++ {
		select {
		case <-exec.diffStarted:
		case <-time.After(time.Second):
			t.Fatalf("only %d read handlers acquired readSem", i)
		}
	}
	fifthRead := call(string(hub.TaskKindDiff), hub.TaskOptions{Adapter: "read-4"}, nil)
	select {
	case <-exec.diffStarted:
		t.Fatal("fifth read exceeded readSem=4")
	case <-time.After(50 * time.Millisecond):
	}
	close(exec.diffRelease)
	select {
	case <-exec.diffStarted:
	case <-time.After(time.Second):
		t.Fatal("queued fifth read never acquired readSem")
	}
	for _, done := range reads {
		if err := <-done; err != nil {
			t.Fatalf("read handler: %v", err)
		}
	}
	if err := <-fifthRead; err != nil {
		t.Fatalf("fifth read handler: %v", err)
	}

	firstWrite := call(string(hub.TaskKindPush), hub.TaskOptions{Adapters: []string{"first"}}, nil)
	if got := <-exec.pushStarted; got != "first" {
		t.Fatalf("first write = %q", got)
	}
	queuedProgress := make(chan string, 4)
	secondWrite := call(string(hub.TaskKindPush), hub.TaskOptions{Adapters: []string{"second"}}, func(raw json.RawMessage) {
		var progress struct {
			Stage string `json:"stage"`
		}
		if json.Unmarshal(raw, &progress) == nil {
			queuedProgress <- progress.Stage
		}
	})
	select {
	case stage := <-queuedProgress:
		if stage != "queued" {
			t.Fatalf("write progress stage = %q, want queued", stage)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting write did not report queued progress")
	}
	select {
	case got := <-exec.pushStarted:
		t.Fatalf("second write %q began before the first completed", got)
	case <-time.After(50 * time.Millisecond):
	}
	exec.pushRelease <- struct{}{}
	if got := <-exec.pushStarted; got != "second" {
		t.Fatalf("second write = %q, want FIFO order", got)
	}
	exec.pushRelease <- struct{}{}
	if err := <-firstWrite; err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := <-secondWrite; err != nil {
		t.Fatalf("second write: %v", err)
	}
}

func TestHeartbeat(t *testing.T) {
	d := New(Config{AgentID: "heartbeat-agent"}, &testExecutor{})
	d.heartbeatEvery = time.Hour
	d.setDrift(&hub.AgentDrift{Push: 3, Error: "fresh"})
	d.setHost(&hub.HostSnapshot{OS: "test"})
	d.tools.mu.Lock()
	d.tools.statuses = []toolctl.Status{{ID: "pi", Version: "1.0"}}
	d.tools.known = true
	d.tools.mu.Unlock()

	agent, hubConn := streamtest.Pipe(streamtest.PipeOptions{})
	agentSession := stream.NewSession(agent, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	hubSession := stream.NewSession(hubConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	events := make(chan hub.HeartbeatParams, 2)
	hubSession.OnEvent(hub.MethodHeartbeat, func(_ context.Context, _ string, raw json.RawMessage) {
		var event hub.HeartbeatParams
		if json.Unmarshal(raw, &event) == nil {
			events <- event
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = agentSession.Run(ctx) }()
	go func() { _ = hubSession.Run(ctx) }()
	loopDone := make(chan struct{})
	go func() { d.heartbeatLoop(ctx, agentSession); close(loopDone) }()
	d.signalHeartbeat()
	select {
	case event := <-events:
		if event.Version != web.Version || event.Drift == nil || event.Drift.Push != 3 || event.Host == nil || event.Host.OS != "test" || event.Tools == nil || len(*event.Tools) != 1 {
			t.Fatalf("heartbeat event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("background heartbeat wake was not delivered")
	}
	cancel()
	agentSession.Close(stream.CloseNormal, "test done")
	hubSession.Close(stream.CloseNormal, "test done")
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("heartbeat loop did not stop")
	}
}

func TestHeartbeatStatusScanCompletionTriggersUpdate(t *testing.T) {
	exec := &flightExecutor{started: make(chan int, 1), releases: make(chan struct{}, 1)}
	d := New(Config{AgentID: "status-heartbeat"}, exec)
	d.heartbeatEvery = time.Hour
	agentConn, hubConn := streamtest.Pipe(streamtest.PipeOptions{})
	agentSession := stream.NewSession(agentConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	hubSession := stream.NewSession(hubConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	events := make(chan hub.HeartbeatParams, 2)
	hubSession.OnEvent(hub.MethodHeartbeat, func(_ context.Context, _ string, raw json.RawMessage) {
		var event hub.HeartbeatParams
		if json.Unmarshal(raw, &event) == nil {
			events <- event
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = agentSession.Run(ctx) }()
	go func() { _ = hubSession.Run(ctx) }()
	loopDone := make(chan struct{})
	go func() { d.heartbeatLoop(ctx, agentSession); close(loopDone) }()
	scanDone := make(chan error, 1)
	go func() { _, err := d.statusReport(ctx); scanDone <- err }()
	select {
	case <-exec.started:
	case <-time.After(time.Second):
		t.Fatal("status scan did not start")
	}
	select {
	case <-events:
		t.Fatal("heartbeat waited for or preceded the status scan completion")
	case <-time.After(50 * time.Millisecond):
	}
	exec.releases <- struct{}{}
	if err := <-scanDone; err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Drift == nil || event.Drift.Push != 1 {
			t.Fatalf("status completion heartbeat = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("status scan completion did not trigger a heartbeat")
	}
	cancel()
	agentSession.Close(stream.CloseNormal, "test done")
	hubSession.Close(stream.CloseNormal, "test done")
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("heartbeat loop did not stop")
	}
}

func TestHeartbeatToolProbeCompletionTriggersUpdate(t *testing.T) {
	d := New(Config{AgentID: "tool-heartbeat"}, &testExecutor{})
	d.SetToolkit(Toolkit{Probe: func(context.Context) []toolctl.Status { return []toolctl.Status{{ID: "pi", Version: "2.0"}} }})
	agent, hubConn := streamtest.Pipe(streamtest.PipeOptions{})
	agentSession := stream.NewSession(agent, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	hubSession := stream.NewSession(hubConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	events := make(chan hub.HeartbeatParams, 4)
	hubSession.OnEvent(hub.MethodHeartbeat, func(_ context.Context, _ string, raw json.RawMessage) {
		var event hub.HeartbeatParams
		if json.Unmarshal(raw, &event) == nil {
			events <- event
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = agentSession.Run(ctx) }()
	go func() { _ = hubSession.Run(ctx) }()
	loopDone := make(chan struct{})
	go func() { d.heartbeatLoop(ctx, agentSession); close(loopDone) }()
	_ = d.reportedTools(ctx, 0)
	found := false
	deadline := time.After(time.Second)
	for !found {
		select {
		case event := <-events:
			found = event.Tools != nil && len(*event.Tools) == 1 && (*event.Tools)[0].Version == "2.0"
		case <-deadline:
			t.Fatal("tool probe completion did not trigger a heartbeat update")
		}
	}
	cancel()
	agentSession.Close(stream.CloseNormal, "test done")
	hubSession.Close(stream.CloseNormal, "test done")
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("heartbeat loop did not stop")
	}
}

func TestDaemonUpgradeUsesDataURLAndAgentSecret(t *testing.T) {
	var requestPath, authorization string
	dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		authorization = r.Header.Get("Authorization")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer dataPlane.Close()
	d := New(Config{
		HubURL:      "http://control.example",
		DataURL:     dataPlane.URL,
		AgentSecret: "upgrade-secret",
	}, &testExecutor{})
	report := d.runUpgrade(context.Background())
	if report.OK || report.Status != "error" || !strings.Contains(report.Note, "401") {
		t.Fatalf("upgrade report = %+v, want the test server's 401", report)
	}
	if requestPath != "/dl/homer" {
		t.Fatalf("upgrade path = %q, want /dl/homer", requestPath)
	}
	if authorization != "Bearer upgrade-secret" {
		t.Fatalf("upgrade Authorization = %q, want per-agent secret", authorization)
	}
}

func TestDaemonTaskBudgetUsesMinimumOfRequestAndKind(t *testing.T) {
	tests := []struct {
		method     string
		configured time.Duration
		request    time.Duration
		want       time.Duration
	}{
		{method: string(hub.TaskKindStatus), configured: 50 * time.Second, request: time.Minute, want: 50 * time.Second},
		{method: string(hub.TaskKindStatus), configured: 50 * time.Second, request: 12 * time.Second, want: 12 * time.Second},
		{method: string(hub.TaskKindUpgrade), configured: 50 * time.Second, request: 4 * time.Minute, want: 3 * time.Minute},
		{method: string(hub.TaskKindUpgrade), configured: 50 * time.Second, request: 2 * time.Minute, want: 2 * time.Minute},
		{method: string(hub.TaskKindPull), configured: 50 * time.Second, request: 14 * time.Minute, want: 12 * time.Minute},
		{method: string(hub.TaskKindPull), configured: 50 * time.Second, request: 8 * time.Minute, want: 8 * time.Minute},
		{method: string(hub.TaskKindToolUpgrade), configured: 50 * time.Second, request: 0, want: ToolUpgradeBudget},
		{method: string(hub.TaskKindToolUpgrade), configured: 50 * time.Second, request: 3 * time.Minute, want: 3 * time.Minute},
	}
	for _, tc := range tests {
		if got := taskLimit(tc.configured, tc.method, tc.request); got != tc.want {
			t.Errorf("taskLimit(%s, %v, %v) = %v, want %v", tc.method, tc.configured, tc.request, got, tc.want)
		}
	}
}

func TestDaemonDriftSummaryPreservesFreshMachineMarkerAndForget(t *testing.T) {
	d := New(Config{AgentID: "fresh-machine"}, NewLocalExecutor(t.TempDir()))
	first := d.driftSummary(context.Background())
	if first == nil || !strings.Contains(first.Error, "未找到 homer 配置") {
		t.Fatalf("fresh-machine drift = %+v, want the missing homer config marker", first)
	}
	d.setDrift(&hub.AgentDrift{Error: "cached test marker"})
	if cached := d.driftSummary(context.Background()); cached == nil || cached.Error != "cached test marker" {
		t.Fatalf("drift cache = %+v, want previously cached marker", cached)
	}
	d.forgetDrift()
	afterForget := d.driftSummary(context.Background())
	if afterForget == nil || !strings.Contains(afterForget.Error, "未找到 homer 配置") {
		t.Fatalf("drift after forget = %+v, want fresh scan marker", afterForget)
	}
}

func TestDaemonConfigSelectsDataPlaneURLAndCredential(t *testing.T) {
	exec := NewLocalExecutorWithHub(t.TempDir(), "http://constructor.example", "constructor-secret").(*localExecutor)
	_ = New(Config{
		AgentID:     "data-plane",
		HubURL:      "https://control.example",
		DataURL:     "http://data.example",
		AgentSecret: "per-agent-secret",
	}, exec)
	if exec.hubURL != "http://data.example" || exec.credential != "per-agent-secret" {
		t.Fatalf("executor transport = (%q, %q), want configured DataURL and secret", exec.hubURL, exec.credential)
	}

	// An executor constructed with an explicit data-plane URL keeps it when
	// Config.DataURL is empty, while the daemon still defaults blank DataURL
	// to HubURL if the executor had no URL of its own.
	custom := NewLocalExecutorWithHub(t.TempDir(), "http://custom-data.example", "custom-token").(*localExecutor)
	New(Config{AgentID: "custom-data", HubURL: "https://control.example", Token: "custom-token"}, custom)
	if custom.hubURL != "https://control.example" || custom.credential != "custom-token" {
		t.Fatalf("blank DataURL did not default to HubURL: (%q, %q)", custom.hubURL, custom.credential)
	}
	overridden := NewLocalExecutorWithHub(t.TempDir(), "http://constructor.example", "constructor-token").(*localExecutor)
	New(Config{AgentID: "override-data", HubURL: "https://control.example", DataURL: "http://explicit-data.example", Token: "hub-token"}, overridden)
	if overridden.hubURL != "http://explicit-data.example" || overridden.credential != "hub-token" {
		t.Fatalf("explicit DataURL transport = (%q, %q)", overridden.hubURL, overridden.credential)
	}
	fallback := New(Config{AgentID: "fallback-data", HubURL: "https://control.example", Token: "hub-token"}, nil).exec.(*localExecutor)
	if fallback.hubURL != "https://control.example" || fallback.credential != "hub-token" {
		t.Fatalf("default data plane = (%q, %q), want HubURL fallback", fallback.hubURL, fallback.credential)
	}
}

func newTestHubSession(ctx context.Context, conn stream.Conn, welcome func(hub.HelloParams) hub.WelcomeResult) *stream.Session {
	session := stream.NewSession(conn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	session.Handle(hub.MethodHello, func(_ context.Context, req *stream.Request) (any, error) {
		var hello hub.HelloParams
		if err := req.Decode(&hello); err != nil {
			return nil, err
		}
		return welcome(hello), nil
	})
	go func() { _ = session.Run(ctx) }()
	return session
}

type testExecutor struct {
	blockDiff   chan struct{}
	statusErr   error
	diffStarted chan struct{}
	diffRelease chan struct{}
	pushStarted chan string
	pushRelease chan struct{}
}

func (e *testExecutor) Status(ctx context.Context) (commands.StatusReport, error) {
	if err := ctx.Err(); err != nil {
		return commands.StatusReport{}, err
	}
	if e.statusErr != nil {
		return commands.StatusReport{}, e.statusErr
	}
	return commands.StatusReport{Adapters: []commands.StatusAdapterReport{{ID: "pi", Categories: []commands.StatusCategoryReport{}}}, Errors: []string{}}, nil
}

func (e *testExecutor) Diff(ctx context.Context, params web.DiffParams) (string, error) {
	if e.diffStarted != nil {
		e.diffStarted <- struct{}{}
		select {
		case <-e.diffRelease:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if e.blockDiff != nil {
		<-e.blockDiff
	}
	return "diff:" + params.Adapter, ctx.Err()
}

func (e *testExecutor) Push(ctx context.Context, _ bool, adapters []string, _ bool, _ bool) (commands.PushReport, error) {
	if e.pushStarted != nil {
		id := strings.Join(adapters, ",")
		e.pushStarted <- id
		if id == "cancel-first" {
			<-e.pushRelease
		} else {
			select {
			case <-e.pushRelease:
			case <-ctx.Done():
				return commands.PushReport{}, ctx.Err()
			}
		}
	}
	return commands.PushReport{OK: true, Status: commands.PushStatusPushed}, ctx.Err()
}

func (e *testExecutor) Pull(ctx context.Context, _ bool, _ []string, _ bool) (commands.PullReport, error) {
	return commands.PullReport{OK: true, Status: commands.PullStatusApplied}, ctx.Err()
}

type flightExecutor struct {
	calls    atomic.Int32
	started  chan int
	releases chan struct{}
}

func (e *flightExecutor) Status(context.Context) (commands.StatusReport, error) {
	n := int(e.calls.Add(1))
	e.started <- n
	<-e.releases
	return commands.StatusReport{Adapters: []commands.StatusAdapterReport{{ID: "pi", Push: n}}, Errors: []string{}}, nil
}
func (*flightExecutor) Diff(context.Context, web.DiffParams) (string, error) { return "", nil }
func (*flightExecutor) Push(context.Context, bool, []string, bool, bool) (commands.PushReport, error) {
	return commands.PushReport{OK: true}, nil
}
func (*flightExecutor) Pull(context.Context, bool, []string, bool) (commands.PullReport, error) {
	return commands.PullReport{OK: true}, nil
}

type generationExecutor struct {
	calls         atomic.Int32
	firstStarted  chan struct{}
	secondStarted chan struct{}
	releaseFirst  chan struct{}
}

func (e *generationExecutor) Status(context.Context) (commands.StatusReport, error) {
	n := e.calls.Add(1)
	if n == 1 {
		close(e.firstStarted)
		<-e.releaseFirst
	} else if n == 2 {
		close(e.secondStarted)
	}
	return commands.StatusReport{Adapters: []commands.StatusAdapterReport{{ID: "pi", Push: int(n)}}, Errors: []string{}}, nil
}
func (*generationExecutor) Diff(context.Context, web.DiffParams) (string, error) { return "", nil }
func (*generationExecutor) Push(context.Context, bool, []string, bool, bool) (commands.PushReport, error) {
	return commands.PushReport{OK: true}, nil
}
func (*generationExecutor) Pull(context.Context, bool, []string, bool) (commands.PullReport, error) {
	return commands.PullReport{OK: true}, nil
}

type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}
func (l *captureLogger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}
func (l *captureLogger) contains(value string) bool {
	for _, line := range l.snapshot() {
		if strings.Contains(line, value) {
			return true
		}
	}
	return false
}

func containsString(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
