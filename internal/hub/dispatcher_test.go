package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
			_, err := dispatcher.AgentPush(context.Background(), "connect-a", confirm)
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

func hasAgentCode(err error, code string, status int) bool {
	var agentErr *web.AgentError
	return errors.As(err, &agentErr) && agentErr.Code == code && agentErr.Status == status
}
