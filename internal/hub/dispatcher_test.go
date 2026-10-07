package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/keyring"
	"github.com/zzjcool/homer-cli/internal/web"
)

func TestDispatcherListenDirect(t *testing.T) {
	const token = "dispatcher-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"report":{"adapters":[{"id":"pi"}]}}`))
	}))
	defer server.Close()

	registry := NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "listen-a", Hostname: "box", Mode: AgentModeListen, Addr: server.URL}); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, token)
	got, err := dispatcher.AgentStatus(context.Background(), "listen-a")
	if err != nil {
		t.Fatalf("AgentStatus: %v", err)
	}
	if string(got) != `{"adapters":[{"id":"pi"}]}` {
		t.Fatalf("report = %s", got)
	}

	if _, err := dispatcher.AgentStatus(context.Background(), "missing"); err == nil || !hasAgentCode(err, "agent-not-found", http.StatusNotFound) {
		t.Fatalf("missing error = %v", err)
	}
}

func TestDispatcherSecretListenAndConnect(t *testing.T) {
	const token = "dispatcher-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/keys/op" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"status":"listed","keys":[]}`))
	}))
	defer server.Close()

	registry := NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "listen-secret", Mode: AgentModeListen, Addr: server.URL}); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, token)
	got, err := dispatcher.AgentKey(context.Background(), "listen-secret", keyring.Command{Action: "list"})
	if err != nil || !strings.Contains(string(got), `"listed"`) {
		t.Fatalf("listen key = %s err=%v", got, err)
	}

	if err := registry.Register(AgentInfo{AgentID: "connect-secret", Mode: AgentModeConnect}); err != nil {
		t.Fatal(err)
	}
	resultCh := make(chan json.RawMessage, 1)
	go func() {
		report, waitErr := dispatcher.AgentKey(context.Background(), "connect-secret", keyring.Command{Action: "unlock", ID: "codebuddy"})
		if waitErr != nil {
			t.Errorf("connect key: %v", waitErr)
		}
		resultCh <- report
	}()
	task, ok := registry.Poll("connect-secret", time.Second, context.Background())
	if !ok || task.Kind != TaskKindSecret || !strings.Contains(string(task.Options.SecretPayload), `"unlock"`) {
		t.Fatalf("task = %+v ok=%v", task, ok)
	}
	if err := registry.Submit(TaskResult{
		TaskID: task.TaskID, AgentID: "connect-secret", Kind: task.Kind, OK: true,
		Report: json.RawMessage(`{"ok":true,"status":"applied"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if report := <-resultCh; string(report) != `{"ok":true,"status":"applied"}` {
		t.Fatalf("connect report = %s", report)
	}
}

func TestDispatcherConnectRoundtrip(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "connect-a", Mode: AgentModeConnect}); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, "")
	resultCh := make(chan struct {
		report json.RawMessage
		err    error
	}, 1)
	go func() {
		report, err := dispatcher.AgentStatus(context.Background(), "connect-a")
		resultCh <- struct {
			report json.RawMessage
			err    error
		}{report, err}
	}()

	task, ok := registry.Poll("connect-a", time.Second, context.Background())
	if !ok || task.Kind != TaskKindStatus {
		t.Fatalf("Poll = %+v, %v", task, ok)
	}
	if err := registry.Submit(TaskResult{
		TaskID: task.TaskID, AgentID: "connect-a", Kind: task.Kind, OK: true,
		Report: json.RawMessage(`{"adapters":[]}`),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-resultCh:
		if result.err != nil || string(result.report) != `{"adapters":[]}` {
			t.Fatalf("result = %s, %v", result.report, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not receive connect result")
	}
}

func TestDispatcherConfirmPassthrough(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "connect-a", Mode: AgentModeConnect}); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, "")
	for _, confirm := range []bool{false, true} {
		resultCh := make(chan error, 1)
		go func(confirm bool) {
			_, err := dispatcher.AgentPush(context.Background(), "connect-a", confirm, web.SyncScope{})
			resultCh <- err
		}(confirm)
		task, ok := registry.Poll("connect-a", time.Second, context.Background())
		if !ok || task.Kind != TaskKindPush || task.Options.Confirm != confirm {
			t.Fatalf("task = %+v, ok=%v; confirm=%v", task, ok, confirm)
		}
		if err := registry.Submit(TaskResult{TaskID: task.TaskID, AgentID: "connect-a", Kind: task.Kind, OK: true, Report: json.RawMessage(`{"ok":true}`)}); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-resultCh:
			if err != nil {
				t.Fatalf("AgentPush(%v): %v", confirm, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("AgentPush(%v) did not finish", confirm)
		}
	}
}

func TestDispatcherDirectFailures(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "bad", Mode: AgentModeListen, Addr: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, "")
	dispatcher.Client = &http.Client{Timeout: 20 * time.Millisecond}
	_, err := dispatcher.AgentStatus(context.Background(), "bad")
	if err == nil || !hasAgentCode(err, "agent-unreachable", http.StatusBadGateway) {
		t.Fatalf("unreachable error = %v", err)
	}
}

func TestDispatcherListAgents(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "b", Hostname: "box-b", Mode: AgentModeConnect, LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(AgentInfo{AgentID: "a", Hostname: "box-a", Mode: AgentModeListen, Addr: "http://a", Version: "v"}); err != nil {
		t.Fatal(err)
	}
	agents := NewDispatcher(registry, "").ListAgents()
	if len(agents) != 2 || agents[0].AgentID != "a" || agents[0].Mode != string(AgentModeListen) || agents[1].AgentID != "b" {
		t.Fatalf("agents = %+v", agents)
	}
}

func TestDispatcherListAgentsHost(t *testing.T) {
	registry := NewRegistry()
	usage := 22.0
	if err := registry.Register(AgentInfo{
		AgentID:  "box",
		Hostname: "box",
		Mode:     AgentModeConnect,
		Host: &HostSnapshot{
			OS:     "linux",
			Arch:   "amd64",
			Memory: &HostMemory{Total: 2048, Used: 512},
			CPU:    &HostCPU{Cores: 4, Usage: &usage},
			Nets:   []HostNet{{Name: "enp3s0", Addrs: []string{"10.0.0.8/24"}, Up: true}},
		},
	}); err != nil {
		t.Fatal(err)
	}
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

func hasAgentCode(err error, code string, status int) bool {
	var agentErr *web.AgentError
	return errors.As(err, &agentErr) && agentErr.Code == code && agentErr.Status == status
}

func TestDispatcherListenNonOKMapping(t *testing.T) {
	// A listen agent answering 5xx or non-JSON maps to agent-unreachable.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	registry := NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "err-agent", Mode: AgentModeListen, Addr: failing.URL}); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, "")
	if _, err := dispatcher.AgentStatus(context.Background(), "err-agent"); err == nil || !hasAgentCode(err, "agent-unreachable", http.StatusBadGateway) {
		t.Fatalf("5xx error = %v", err)
	}

	// 200 with a non-JSON body also maps to agent-unreachable.
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer garbage.Close()
	if err := registry.Register(AgentInfo{AgentID: "garbage", Mode: AgentModeListen, Addr: garbage.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.AgentStatus(context.Background(), "garbage"); err == nil || !hasAgentCode(err, "agent-unreachable", http.StatusBadGateway) {
		t.Fatalf("invalid JSON error = %v", err)
	}
}

func TestDispatcherForwardsAdapterScope(t *testing.T) {
	var adapters, overwrite, confirm string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adapters = r.URL.Query().Get("adapters")
		overwrite = r.URL.Query().Get("overwrite")
		confirm = r.URL.Query().Get("confirm")
		_, _ = w.Write([]byte(`{"ok":true,"status":"pushed"}`))
	}))
	defer server.Close()
	registry := NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "listen-scope", Mode: AgentModeListen, Addr: server.URL}); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, "")
	if _, err := dispatcher.AgentPush(context.Background(), "listen-scope", true, web.SyncScope{
		Explicit: true, Adapters: []string{"vscode", "pad"}, Overwrite: true,
	}); err != nil {
		t.Fatal(err)
	}
	if adapters != "vscode,pad" || overwrite != "true" || confirm != "true" {
		t.Fatalf("query adapters=%q overwrite=%q confirm=%q", adapters, overwrite, confirm)
	}

	registry = NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "connect-scope", Mode: AgentModeConnect}); err != nil {
		t.Fatal(err)
	}
	dispatcher = NewDispatcher(registry, "")
	done := make(chan error, 1)
	go func() {
		_, err := dispatcher.AgentPull(context.Background(), "connect-scope", true, web.SyncScope{
			Explicit: true, Adapters: []string{"pad"},
		})
		done <- err
	}()
	task, ok := registry.Poll("connect-scope", time.Second, context.Background())
	if !ok || task.Kind != TaskKindPull || len(task.Options.Adapters) != 1 || task.Options.Adapters[0] != "pad" || task.Options.Overwrite {
		t.Fatalf("task = %+v ok=%v", task, ok)
	}
	if err := registry.Submit(TaskResult{TaskID: task.TaskID, AgentID: "connect-scope", Kind: task.Kind, OK: true, Report: json.RawMessage(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherListenWriteStatusPassthrough(t *testing.T) {
	// Remote push/pull preserves the agent's 409 (confirm gate) and 422
	// (command failure) responses instead of remapping them to 502: the web
	// layer's two-phase confirm for agents depends on this.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/push", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"ok":false,"status":"aborted"}`))
	})
	mux.HandleFunc("/api/pull", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"ok":false,"status":"conflicts"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	registry := NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "write-a", Mode: AgentModeListen, Addr: server.URL}); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, "")
	raw, err := dispatcher.AgentPush(context.Background(), "write-a", false, web.SyncScope{})
	if err != nil || string(raw) != `{"ok":false,"status":"aborted"}` {
		t.Fatalf("push 409 passthrough = %q err=%v", raw, err)
	}
	raw, err = dispatcher.AgentPull(context.Background(), "write-a", true, web.SyncScope{})
	if err != nil || string(raw) != `{"ok":false,"status":"conflicts"}` {
		t.Fatalf("pull 422 passthrough = %q err=%v", raw, err)
	}
}

func TestDispatcherConnectTimeout(t *testing.T) {
	// A connect agent that never reports back surfaces as agent-timeout (504)
	// once the caller's context deadline expires.
	registry := NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "slow", Mode: AgentModeConnect}); err != nil {
		t.Fatal(err)
	}
	// Seed the queue with a task but never Submit the result.
	if err := registry.Enqueue("slow", Task{TaskID: "t-timeout", Kind: TaskKindStatus}); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, "")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := dispatcher.AgentStatus(ctx, "slow")
	if err == nil || !hasAgentCode(err, "agent-timeout", http.StatusGatewayTimeout) {
		t.Fatalf("connect timeout error = %v", err)
	}
}

// An OFFLINE machine must fail fast with a clear offline message — not
// burn the 60s dispatcher timeout before 504. (User story: clicked
// "查看" on a disconnected machine, waited a minute for nothing.)
func TestDispatcherStatusOfflineFailsFast(t *testing.T) {
	registry := NewRegistry()
	registry.Register(AgentInfo{AgentID: "gone", Hostname: "gone", Mode: AgentModeConnect, LastSeen: time.Now().Add(-10 * time.Minute)})
	dispatcher := NewDispatcher(registry, "token")
	started := time.Now()
	_, err := dispatcher.AgentStatus(context.Background(), "gone")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("offline agent must not report success")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("offline agent must fail fast (took %v)", elapsed)
	}
	if !strings.Contains(err.Error(), "离线") {
		t.Fatalf("error must say the machine is offline, got: %v", err)
	}
}
