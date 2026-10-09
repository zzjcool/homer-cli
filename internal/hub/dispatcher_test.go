package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/keyring"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
	"github.com/zzjcool/homer-cli/internal/toolctl"
	"github.com/zzjcool/homer-cli/internal/web"
)

type dispatcherCall struct {
	method string
	params json.RawMessage
	budget time.Duration
}

type dispatcherAgent struct {
	session *stream.Session
	hub     *stream.Session
	callsMu sync.Mutex
	calls   []dispatcherCall
	once    sync.Once
}

func newDispatcherAgent(t *testing.T, registry *Registry, agentID string, caps ...string) *dispatcherAgent {
	t.Helper()
	hubConn, agentConn := streamtest.Pipe(streamtest.PipeOptions{})
	hubSession := stream.NewSession(hubConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	agentSession := stream.NewSession(agentConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	registry.Attach(AgentInfo{AgentID: agentID, Hostname: agentID, Version: "v1", caps: caps}, hubSession)
	t.Cleanup(func() {
		hubSession.Close(stream.CloseNormal, "test complete")
		agentSession.Close(stream.CloseNormal, "test complete")
	})
	go func() { _ = hubSession.Run(context.Background()) }()
	return &dispatcherAgent{session: agentSession, hub: hubSession}
}

func attachDispatcherAgent(registry *Registry, agentID string, caps ...string) (*stream.Session, stream.Conn) {
	hubConn, agentConn := streamtest.Pipe(streamtest.PipeOptions{})
	session := stream.NewSession(hubConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	registry.Attach(AgentInfo{AgentID: agentID, caps: caps}, session)
	go func() { _ = session.Run(context.Background()) }()
	return session, agentConn
}

func hasAgentCode(err error, code string, status int) bool {
	var agentErr *web.AgentError
	return errors.As(err, &agentErr) && agentErr.Code == code && agentErr.Status == status
}

func (a *dispatcherAgent) Handle(method string, handler stream.Handler) {
	a.session.Handle(method, func(ctx context.Context, req *stream.Request) (any, error) {
		a.callsMu.Lock()
		a.calls = append(a.calls, dispatcherCall{
			method: req.Method, params: append(json.RawMessage(nil), req.Params...), budget: req.Budget,
		})
		a.callsMu.Unlock()
		if handler == nil {
			return map[string]any{"ok": true}, nil
		}
		return handler(ctx, req)
	})
}

func (a *dispatcherAgent) Run() {
	a.once.Do(func() {
		go func() { _ = a.session.Run(context.Background()) }()
	})
}

func (a *dispatcherAgent) Calls() []dispatcherCall {
	a.callsMu.Lock()
	defer a.callsMu.Unlock()
	return append([]dispatcherCall(nil), a.calls...)
}

func TestDispatcherStreamMethodsAndBudgets(t *testing.T) {
	registry := NewRegistry()
	methods := []string{"status", "diff", "push", "pull", "ssh-key", "secret", "upgrade", "tool-upgrade", MethodInspect}
	agent := newDispatcherAgent(t, registry, "agent-a", methods...)
	for _, method := range methods {
		method := method
		agent.Handle(method, func(_ context.Context, req *stream.Request) (any, error) {
			if req.Budget <= 0 {
				t.Errorf("request %s has no relative deadline budget", method)
			}
			switch method {
			case "diff":
				return map[string]string{"text": "diff text"}, nil
			case MethodInspect:
				req.Progress(web.InspectEvent{Stage: "adapter", Done: 1, Total: 1})
				return web.InspectResult{Status: commands.StatusReport{Adapters: []commands.StatusAdapterReport{{ID: "pi"}}, Errors: []string{}}}, nil
			case "tool-upgrade":
				return json.RawMessage(`{"ok":true,"after":"0.90.2"}`), nil
			default:
				return map[string]any{"ok": true, "method": method}, nil
			}
		})
	}
	agent.Run()
	dispatcher := NewDispatcher(registry, "unused-shared-token")
	ctx := context.Background()
	if _, err := dispatcher.AgentStatus(ctx, "agent-a"); err != nil {
		t.Fatalf("AgentStatus: %v", err)
	}
	if got, err := dispatcher.AgentDiff(ctx, "agent-a", web.DiffParams{Adapter: "pi", Category: "settings", Path: "x.json"}); err != nil || got != "diff text" {
		t.Fatalf("AgentDiff = %q, %v", got, err)
	}
	if _, err := dispatcher.AgentPush(ctx, "agent-a", true, web.SyncScope{Explicit: true, Adapters: []string{"pi"}, AllowSecrets: true}); err != nil {
		t.Fatalf("AgentPush: %v", err)
	}
	if _, err := dispatcher.AgentPull(ctx, "agent-a", true, web.SyncScope{Overwrite: true}); err != nil {
		t.Fatalf("AgentPull: %v", err)
	}
	if _, err := dispatcher.AgentInstallSSHKeys(ctx, "agent-a", "octocat", []string{"ssh-ed25519 key"}); err != nil {
		t.Fatalf("AgentInstallSSHKeys: %v", err)
	}
	if _, err := dispatcher.AgentKey(ctx, "agent-a", keyring.Command{Action: "unlock", ID: "secret", Password: "not-in-response"}); err != nil {
		t.Fatalf("AgentKey: %v", err)
	}
	if _, err := dispatcher.AgentUpgrade(ctx, "agent-a"); err != nil {
		t.Fatalf("AgentUpgrade: %v", err)
	}
	if _, err := dispatcher.AgentToolUpgrade(ctx, "agent-a", "pi"); err != nil {
		t.Fatalf("AgentToolUpgrade: %v", err)
	}
	var events []web.InspectEvent
	inspect, err := dispatcher.AgentInspect(ctx, "agent-a", web.InspectParams{Adapters: []string{"pi"}, WantKeys: true}, func(event web.InspectEvent) {
		events = append(events, event)
	})
	if err != nil || len(inspect.Status.Adapters) != 1 || len(events) != 1 || events[0].Stage != "adapter" {
		t.Fatalf("AgentInspect = %+v events=%+v err=%v", inspect, events, err)
	}
	if got, err := dispatcher.AgentResolve(ctx, "agent-a", "local"); err != nil || !strings.Contains(string(got), `"ok":true`) {
		t.Fatalf("AgentResolve = %s, %v", got, err)
	}

	calls := agent.Calls()
	if len(calls) != 10 {
		t.Fatalf("agent received %d calls, want 10: %+v", len(calls), calls)
	}
	if got := calls[0]; got.method != "status" || got.budget <= 0 {
		t.Fatalf("status call = %+v", got)
	}
	if got := calls[1]; got.method != "diff" || string(got.params) != `{"adapter":"pi","category":"settings","path":"x.json"}` {
		t.Fatalf("diff call = %+v", got)
	}
	if got := calls[2]; got.method != "push" || !strings.Contains(string(got.params), `"confirm":true`) || !strings.Contains(string(got.params), `"allowSecrets":true`) {
		t.Fatalf("push call = %+v", got)
	}
	if got := calls[3]; got.method != "pull" || got.budget < 11*time.Minute {
		t.Fatalf("pull budget/method = %+v", got)
	}
	if got := calls[4]; got.method != "ssh-key" || !strings.Contains(string(got.params), `"githubUser":"octocat"`) {
		t.Fatalf("ssh-key call = %+v", got)
	}
	if got := calls[5]; got.method != "secret" || !strings.Contains(string(got.params), `"secretPayload"`) || !strings.Contains(string(got.params), "not-in-response") {
		t.Fatalf("secret call = %+v", got)
	}
	if got := calls[6]; got.method != "upgrade" || got.budget < 2*time.Minute {
		t.Fatalf("upgrade budget/method = %+v", got)
	}
	if got := calls[7]; got.method != "tool-upgrade" || got.budget < 6*time.Minute {
		t.Fatalf("tool-upgrade budget/method = %+v", got)
	}
	if got := calls[8]; got.method != MethodInspect || got.budget <= 0 {
		t.Fatalf("inspect call = %+v", got)
	}
	if got := calls[9]; got.method != "push" || !strings.Contains(string(got.params), `"resolve":"local"`) {
		t.Fatalf("resolve call = %+v", got)
	}
	if info, ok := registry.Get("agent-a"); !ok || len(info.Tools) != 0 {
		t.Fatalf("tool upgrade should not invent an absent tool status: %+v, found=%v", info.Tools, ok)
	}
}

func TestDispatcherRequiresOnlineForEveryCall(t *testing.T) {
	registry := NewRegistry()
	registry.Attach(AgentInfo{AgentID: "offline", LastSeen: time.Now().Add(-time.Hour)}, nil)
	dispatcher := NewDispatcher(registry, "")
	checks := []struct {
		name string
		call func() error
	}{
		{"status", func() error { _, err := dispatcher.AgentStatus(context.Background(), "offline"); return err }},
		{"diff", func() error {
			_, err := dispatcher.AgentDiff(context.Background(), "offline", web.DiffParams{})
			return err
		}},
		{"push", func() error {
			_, err := dispatcher.AgentPush(context.Background(), "offline", true, web.SyncScope{})
			return err
		}},
		{"pull", func() error {
			_, err := dispatcher.AgentPull(context.Background(), "offline", true, web.SyncScope{})
			return err
		}},
		{"secret", func() error {
			_, err := dispatcher.AgentKey(context.Background(), "offline", keyring.Command{Action: "list"})
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			started := time.Now()
			err := check.call()
			if !hasAgentCode(err, "agent-offline", http.StatusServiceUnavailable) {
				t.Fatalf("call error = %v, want agent-offline", err)
			}
			if time.Since(started) > 100*time.Millisecond {
				t.Fatalf("offline agent did not fail fast: %v", time.Since(started))
			}
		})
	}
	if _, err := dispatcher.AgentStatus(context.Background(), "missing"); !hasAgentCode(err, "agent-not-found", http.StatusNotFound) || !strings.Contains(err.Error(), "未注册") {
		t.Fatalf("unknown agent error = %v, want Chinese not-registered message", err)
	}
}

func TestDispatcherRejectsUnsupportedCapability(t *testing.T) {
	registry := NewRegistry()
	agent := newDispatcherAgent(t, registry, "agent-a", string(TaskKindStatus))
	agent.Handle(string(TaskKindStatus), nil)
	agent.Run()
	_, err := NewDispatcher(registry, "").AgentDiff(context.Background(), "agent-a", web.DiffParams{})
	if !hasAgentCode(err, "agent-unreachable", http.StatusBadGateway) || !strings.Contains(err.Error(), "does not advertise") {
		t.Fatalf("unsupported capability error = %v", err)
	}
	if len(agent.Calls()) != 0 {
		t.Fatalf("unsupported method was sent to agent: %+v", agent.Calls())
	}
}

func TestDispatcherErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		code   string
		status int
	}{
		{"offline session", &stream.SessionClosedError{Cause: &stream.CloseError{Code: stream.CloseSuperseded, Reason: "same agent ID"}}, "agent-offline", http.StatusServiceUnavailable},
		{"local deadline", context.DeadlineExceeded, "agent-timeout", http.StatusGatewayTimeout},
		{"caller cancellation", context.Canceled, "agent-timeout", http.StatusGatewayTimeout},
		{"remote timeout", &stream.Error{Code: stream.CodeTimeout, Message: "agent budget expired"}, "agent-timeout", http.StatusGatewayTimeout},
		{"bad request", &stream.Error{Code: stream.CodeBadRequest, Message: "missing adapter"}, "bad-request", http.StatusBadRequest},
		{"exec failed", &stream.Error{Code: stream.CodeExecFailed, Message: "disk error"}, "agent-unreachable", http.StatusBadGateway},
		{"internal", &stream.Error{Code: stream.CodeInternal, Message: "panic"}, "agent-unreachable", http.StatusBadGateway},
		{"unknown method", &stream.Error{Code: stream.CodeUnknownMethod, Message: "unknown"}, "agent-unreachable", http.StatusBadGateway},
		{"unsupported capability", &stream.Error{Code: "unsupported", Message: "not in caps"}, "agent-unreachable", http.StatusBadGateway},
		{"unsupported version", &stream.Error{Code: stream.CodeUnsupported, Message: "version"}, "agent-unreachable", http.StatusBadGateway},
		{"overloaded", &stream.Error{Code: stream.CodeOverloaded, Message: "busy"}, "agent-unreachable", http.StatusBadGateway},
		{"plain transport failure", errors.New("broken socket"), "agent-unreachable", http.StatusBadGateway},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := mapDispatcherError("agent-a", test.err)
			var agentErr *web.AgentError
			if !errors.As(got, &agentErr) || agentErr.Code != test.code || agentErr.Status != test.status {
				t.Fatalf("mapped error = %T %v, want %s/%d", got, got, test.code, test.status)
			}
			if test.name == "offline session" && !strings.Contains(got.Error(), "same agent ID") {
				t.Fatalf("offline message did not preserve close reason: %v", got)
			}
			if test.name == "exec failed" && !strings.Contains(got.Error(), "disk error") {
				t.Fatalf("remote error message was not passed through: %v", got)
			}
		})
	}
}

func TestDispatcherCallConcurrencyLimit(t *testing.T) {
	registry := NewRegistry()
	const calls = agentCallConcurrency + 1
	agent := newDispatcherAgent(t, registry, "agent-a", string(TaskKindStatus))
	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, calls)
	release := make(chan struct{})
	var releaseOnce sync.Once
	stopHandlers := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(stopHandlers)
	agent.Handle(string(TaskKindStatus), func(ctx context.Context, _ *stream.Request) (any, error) {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			active.Add(-1)
			return map[string]any{"ok": true}, nil
		case <-ctx.Done():
			active.Add(-1)
			return nil, ctx.Err()
		}
	})
	agent.Run()
	dispatcher := NewDispatcher(registry, "")
	ctxs := make([]context.Context, calls)
	cancels := make([]context.CancelFunc, calls)
	for i := range ctxs {
		ctxs[i], cancels[i] = context.WithTimeout(context.Background(), time.Hour)
		defer cancels[i]()
	}
	type callOutcome struct {
		index int
		err   error
	}
	outcomes := make(chan callOutcome, calls)
	var wg sync.WaitGroup
	launch := func(i int) {
		index := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := dispatcher.AgentStatus(ctxs[index], "agent-a")
			outcomes <- callOutcome{index: index, err: err}
		}()
	}
	for i := 0; i < agentCallConcurrency; i++ {
		launch(i)
	}
	for i := 0; i < agentCallConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			stopHandlers()
			wg.Wait()
			var summary []string
			for len(outcomes) > 0 {
				outcome := <-outcomes
				summary = append(summary, fmt.Sprintf("call[%d]=%v", outcome.index, outcome.err))
			}
			t.Fatalf("only %d handlers started; outcomes=%v calls=%+v hubErr=%v hubStats=%+v agentErr=%v agentStats=%+v", i, summary, agent.Calls(), agent.hub.Err(), agent.hub.Stats(), agent.session.Err(), agent.session.Stats())
		}
	}
	launch(calls - 1)
	waitForDispatcherQueue(t, dispatcher, "agent-a", 1)
	if got := activeAgentCalls(dispatcher, "agent-a"); got != agentCallConcurrency {
		stopHandlers()
		t.Fatalf("active call tokens = %d, want %d", got, agentCallConcurrency)
	}
	cancels[calls-1]()
	stopHandlers()
	wg.Wait()
	errs := make([]error, calls)
	for len(outcomes) > 0 {
		outcome := <-outcomes
		errs[outcome.index] = outcome.err
	}
	if maximum.Load() != agentCallConcurrency {
		t.Fatalf("maximum concurrent calls = %d, want %d", maximum.Load(), agentCallConcurrency)
	}
	if !hasAgentCode(errs[calls-1], "agent-timeout", http.StatusGatewayTimeout) {
		t.Fatalf("queued call cancellation = %v, want agent-timeout", errs[calls-1])
	}
	for i := 0; i < calls-1; i++ {
		if errs[i] != nil {
			t.Errorf("call %d: %v", i, errs[i])
		}
	}
}

func waitForDispatcherQueue(t *testing.T, dispatcher *Dispatcher, agentID string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		dispatcher.limitsMu.Lock()
		limit := dispatcher.limits[agentID]
		queued := 0
		if limit != nil {
			queued = limit.refs - len(limit.active)
		}
		dispatcher.limitsMu.Unlock()
		if queued == want {
			return
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("agent %q queued calls = %d, want %d", agentID, dispatcherQueue(dispatcher, agentID), want)
}

func dispatcherQueue(dispatcher *Dispatcher, agentID string) int {
	dispatcher.limitsMu.Lock()
	defer dispatcher.limitsMu.Unlock()
	if limit := dispatcher.limits[agentID]; limit != nil {
		return limit.refs - len(limit.active)
	}
	return 0
}

func activeAgentCalls(dispatcher *Dispatcher, agentID string) int {
	dispatcher.limitsMu.Lock()
	defer dispatcher.limitsMu.Unlock()
	if limit := dispatcher.limits[agentID]; limit != nil {
		return len(limit.active)
	}
	return 0
}

func TestDispatcherRemoveKicksAgent(t *testing.T) {
	registry := NewRegistry()
	session, peer := attachDispatcherAgent(registry, "agent-a", string(TaskKindStatus))
	defer peer.CloseNow()
	dispatcher := NewDispatcher(registry, "")
	dispatcher.Hub = &AgentHub{registry: registry}
	if !dispatcher.RemoveAgent("agent-a") {
		t.Fatal("RemoveAgent() returned false")
	}
	select {
	case <-session.Done():
	case <-time.After(time.Second):
		t.Fatal("RemoveAgent did not kick the connected session")
	}
	if _, ok := registry.Get("agent-a"); ok {
		t.Fatal("RemoveAgent left the agent in the registry")
	}
}

func TestDispatcherListAgents(t *testing.T) {
	registry := NewRegistry()
	registry.Attach(AgentInfo{AgentID: "b", Hostname: "box-b", Version: "v1", Drift: &AgentDrift{Conflicts: 1, Resolutions: 2}}, nil)
	registry.Attach(AgentInfo{AgentID: "a", Hostname: "box-a"}, nil)
	agents := NewDispatcher(registry, "").ListAgents()
	if len(agents) != 2 || agents[0].AgentID != "a" || agents[0].Hostname != "box-a" || agents[1].AgentID != "b" {
		t.Fatalf("agents = %+v", agents)
	}
	if agents[1].Drift == nil || agents[1].Drift.Resolutions != 2 {
		t.Fatalf("ListAgents did not copy resolution count: %+v", agents[1].Drift)
	}
}

func TestListAgentsCopiesResolutions(t *testing.T) {
	registry := NewRegistry()
	registry.Attach(AgentInfo{AgentID: "box", Drift: &AgentDrift{Conflicts: 1, Resolutions: 4}}, nil)
	listed := NewDispatcher(registry, "").ListAgents()
	if len(listed) != 1 || listed[0].Drift == nil || listed[0].Drift.Resolutions != 4 {
		t.Fatalf("listed agents = %+v", listed)
	}
	listed[0].Drift.Resolutions = 9
	info, _ := registry.Get("box")
	if info.Drift == nil || info.Drift.Resolutions != 4 {
		t.Fatalf("web drift aliased registry state: %+v", info.Drift)
	}
}

func TestDispatcherListAgentsHost(t *testing.T) {
	registry := NewRegistry()
	usage := 22.0
	registry.Attach(AgentInfo{AgentID: "box", Host: &HostSnapshot{
		OS: "linux", Arch: "amd64", Memory: &HostMemory{Total: 2048, Used: 512},
		CPU: &HostCPU{Cores: 4, Usage: &usage}, Nets: []HostNet{{Name: "enp3s0", Addrs: []string{"10.0.0.8/24"}, Up: true}},
	}}, nil)
	agents := NewDispatcher(registry, "").ListAgents()
	if len(agents) != 1 || agents[0].Host == nil {
		t.Fatalf("agents = %+v", agents)
	}
	host := agents[0].Host
	if host.OS != "linux" || host.Memory == nil || host.Memory.Used != 512 || host.CPU == nil || host.CPU.Cores != 4 || host.CPU.Usage == nil || *host.CPU.Usage != 22 {
		t.Fatalf("host = %+v", host)
	}
	if len(host.Nets) != 1 || host.Nets[0].Addrs[0] != "10.0.0.8/24" {
		t.Fatalf("nets = %+v", host.Nets)
	}
}

func TestDispatcherToolListClone(t *testing.T) {
	registry := NewRegistry()
	registry.Attach(AgentInfo{AgentID: "box", Tools: []toolctl.Status{{ID: "pi", Version: "1.0"}}}, nil)
	listed := NewDispatcher(registry, "").ListAgents()
	listed[0].Tools[0].Version = "mutated"
	info, _ := registry.Get("box")
	if info.Tools[0].Version != "1.0" {
		t.Fatalf("web list aliased registry tools: %+v", info.Tools)
	}
}

func TestAgentResolveRecordSendsTaskAndNotesPending(t *testing.T) {
	registry := NewRegistry()
	registry.Attach(AgentInfo{AgentID: "box", Drift: &AgentDrift{Conflicts: 1, Resolutions: 2}}, nil)
	agent := newDispatcherAgent(t, registry, "box", MethodResolveRecord)
	agent.Handle(MethodResolveRecord, func(_ context.Context, _ *stream.Request) (any, error) {
		return json.RawMessage(`{"ok":true,"action":"record","pending":3,"entries":[],"errors":[]}`), nil
	})
	agent.Run()

	raw, err := NewDispatcher(registry, "").AgentResolveRecord(context.Background(), "box", web.ResolveRecordRequest{
		Action: "record", Choice: "local", Adapters: []string{"pi"}, CenterGeneration: 7,
	})
	if err != nil || !strings.Contains(string(raw), `"pending":3`) {
		t.Fatalf("AgentResolveRecord = %s, %v", raw, err)
	}
	calls := agent.Calls()
	if len(calls) != 1 || calls[0].method != MethodResolveRecord {
		t.Fatalf("calls = %+v", calls)
	}
	var options TaskOptions
	if err := json.Unmarshal(calls[0].params, &options); err != nil {
		t.Fatal(err)
	}
	if options.ResolutionAction != "record" || options.ResolutionChoice != "local" ||
		!reflect.DeepEqual(options.Adapters, []string{"pi"}) || options.CenterGeneration != 7 {
		t.Fatalf("task options = %+v", options)
	}
	info, _ := registry.Get("box")
	if info.Drift == nil || info.Drift.Resolutions != 3 {
		t.Fatalf("pending count was not noted: %+v", info.Drift)
	}
}

func TestAgentResolveRecordOutdatedAgent(t *testing.T) {
	registry := NewRegistry()
	agent := newDispatcherAgent(t, registry, "old", string(TaskKindStatus))
	dispatcher := NewDispatcher(registry, "")
	_, err := dispatcher.AgentResolveRecord(context.Background(), "old", web.ResolveRecordRequest{Action: "record"})
	if !hasAgentCode(err, "agent-outdated", http.StatusConflict) {
		t.Fatalf("error = %v, want agent-outdated conflict", err)
	}
	if calls := agent.Calls(); len(calls) != 0 {
		t.Fatalf("outdated agent received stream calls: %+v", calls)
	}
}

func TestAgentResolveRecordOfflineAgentIs503First(t *testing.T) {
	registry := NewRegistry()
	registry.Attach(AgentInfo{AgentID: "offline"}, nil)
	_, err := NewDispatcher(registry, "").AgentResolveRecord(context.Background(), "offline", web.ResolveRecordRequest{Action: "record"})
	if !hasAgentCode(err, "agent-offline", http.StatusServiceUnavailable) {
		t.Fatalf("error = %v, want agent-offline before capability check", err)
	}
}

func TestTaskOptionsForScopeCopiesResolutionFlags(t *testing.T) {
	adapters := []string{"pi", "herdr"}
	options := taskOptionsForScope(true, web.SyncScope{
		Explicit: true, Adapters: adapters, Overwrite: true, AllowSecrets: true,
		ApplyResolutions: true, ClearResolutions: true, CenterGeneration: 12,
	})
	adapters[0] = "mutated"
	if !options.Confirm || !options.Overwrite || !options.AllowSecrets ||
		!options.ApplyResolutions || !options.ClearResolutions || options.CenterGeneration != 12 ||
		!reflect.DeepEqual(options.Adapters, []string{"pi", "herdr"}) {
		t.Fatalf("task options = %+v", options)
	}
}
