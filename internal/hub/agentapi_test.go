package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAgentAPIRegister(t *testing.T) {
	registry := NewRegistry()
	server := httptest.NewServer(NewAgentAPI(registry, "token"))
	defer server.Close()

	response := postAgentJSON(t, server.URL+"/agent/v1/register", "token", map[string]any{
		"agentId": "agent-a", "hostname": "box-a", "mode": "listen", "addr": "http://box-a:7761", "version": "v1",
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, want 200; body=%s", response.StatusCode, response.Body)
	}
	var registered struct {
		OK               bool `json:"ok"`
		HeartbeatSeconds int  `json:"heartbeatSeconds"`
	}
	decodeBody(t, response.Body, &registered)
	if !registered.OK || registered.HeartbeatSeconds != 60 {
		t.Fatalf("register response = %+v", registered)
	}
	info, ok := registry.Get("agent-a")
	if !ok || info.Mode != AgentModeListen || info.Addr != "http://box-a:7761" || info.LastSeen.IsZero() {
		t.Fatalf("registered info = %+v, found=%v", info, ok)
	}

	bad := postAgentJSON(t, server.URL+"/agent/v1/register", "token", map[string]any{
		"agentId": "missing-address", "hostname": "box", "mode": "listen",
	})
	if bad.StatusCode != http.StatusBadRequest || errorCode(t, bad.Body) != "bad-request" {
		t.Fatalf("invalid register = %d %s", bad.StatusCode, bad.Body)
	}
}

func TestAgentAPIPollLongWait(t *testing.T) {
	registry := NewRegistry()
	server := httptest.NewServer(NewAgentAPI(registry, ""))
	defer server.Close()

	missing := postAgentJSON(t, server.URL+"/agent/v1/poll", "", map[string]any{"agentId": "missing", "waitSeconds": 0})
	if missing.StatusCode != http.StatusNotFound || errorCode(t, missing.Body) != "agent-not-found" {
		t.Fatalf("missing poll = %d %s", missing.StatusCode, missing.Body)
	}
	if err := registry.Register(AgentInfo{AgentID: "agent-a", Mode: AgentModeConnect}); err != nil {
		t.Fatal(err)
	}

	pollResult := make(chan agentHTTPResponse, 1)
	go func() {
		pollResult <- postAgentJSON(t, server.URL+"/agent/v1/poll", "", map[string]any{"agentId": "agent-a", "waitSeconds": 1})
	}()
	time.Sleep(50 * time.Millisecond)
	task := Task{TaskID: "poll-task", Kind: TaskKindStatus, CreatedAt: time.Now()}
	if err := registry.Enqueue("agent-a", task); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-pollResult:
		if response.StatusCode != http.StatusOK {
			t.Fatalf("poll status = %d; body=%s", response.StatusCode, response.Body)
		}
		var body struct {
			OK   bool  `json:"ok"`
			Task *Task `json:"task"`
		}
		decodeBody(t, response.Body, &body)
		if !body.OK || body.Task == nil || body.Task.TaskID != task.TaskID {
			t.Fatalf("poll body = %+v", body)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("poll did not wake after enqueue")
	}

	next := postAgentJSON(t, server.URL+"/agent/v1/poll", "", map[string]any{"agentId": "agent-a", "waitSeconds": 0})
	if next.StatusCode != http.StatusOK {
		t.Fatalf("second poll status = %d", next.StatusCode)
	}
	var empty struct {
		Task *Task `json:"task"`
	}
	decodeBody(t, next.Body, &empty)
	if empty.Task != nil {
		t.Fatalf("task was delivered twice: %+v", empty.Task)
	}
}

func TestAgentAPIReport(t *testing.T) {
	registry := NewRegistry()
	api := NewAgentAPI(registry, "")
	server := httptest.NewServer(api)
	defer server.Close()
	if err := registry.Register(AgentInfo{AgentID: "agent-a", Mode: AgentModeConnect}); err != nil {
		t.Fatal(err)
	}
	task := Task{TaskID: "report-task", Kind: TaskKindStatus, CreatedAt: time.Now()}
	if err := registry.Enqueue("agent-a", task); err != nil {
		t.Fatal(err)
	}
	polled := postAgentJSON(t, server.URL+"/agent/v1/poll", "", map[string]any{"agentId": "agent-a", "waitSeconds": 0})
	if polled.StatusCode != http.StatusOK {
		t.Fatalf("poll = %d %s", polled.StatusCode, polled.Body)
	}

	waitResult := make(chan TaskResult, 1)
	go func() {
		result, err := registry.Wait(context.Background(), task.TaskID)
		if err != nil {
			t.Errorf("Wait: %v", err)
			return
		}
		waitResult <- result
	}()
	report := postAgentJSON(t, server.URL+"/agent/v1/report", "", map[string]any{
		"agentId": "agent-a", "taskId": task.TaskID, "ok": true, "report": map[string]any{"ready": true},
	})
	if report.StatusCode != http.StatusOK || !json.Valid(report.Body) {
		t.Fatalf("report = %d %s", report.StatusCode, report.Body)
	}
	select {
	case result := <-waitResult:
		if result.TaskID != task.TaskID || result.AgentID != "agent-a" || !result.OK || result.Kind != TaskKindStatus || string(result.Report) != `{"ready":true}` {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not receive report")
	}

	unknown := postAgentJSON(t, server.URL+"/agent/v1/report", "", map[string]any{
		"agentId": "agent-a", "taskId": "unknown", "ok": true,
	})
	if unknown.StatusCode != http.StatusNotFound || errorCode(t, unknown.Body) != "task-not-found" {
		t.Fatalf("unknown report = %d %s", unknown.StatusCode, unknown.Body)
	}
}

type agentHTTPResponse struct {
	StatusCode int
	Body       []byte
}

func postAgentJSON(t *testing.T, endpoint, token string, value any) agentHTTPResponse {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return agentHTTPResponse{StatusCode: response.StatusCode, Body: responseBody}
}

func decodeBody(t *testing.T, body []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeBody(t, body, &envelope)
	return envelope.Error.Code
}
