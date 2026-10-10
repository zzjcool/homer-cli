package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/gens"
)

// stagedContractStub mirrors tests/e2e's stagedUIStub through the web layer.
type stagedContractStub struct{}

func (stagedContractStub) ListAgents() []AgentInfo {
	return []AgentInfo{{AgentID: "box", Hostname: "box", LastSeen: time.Now(),
		Drift: &AgentDrift{Conflicts: 2, Resolutions: 1}}}
}

func (stagedContractStub) AgentStatus(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{"adapters":[{"id":"pi","conflicts":2}],"errors":[],` +
		`"resolutions":[{"adapter":"pi","choice":"center","recordedAt":"2026-10-09T00:00:00Z","generationAtRecord":1}]}`), nil
}

func (stagedContractStub) AgentDiff(context.Context, string, DiffParams) (string, error) {
	return "d", nil
}
func (stagedContractStub) AgentPush(context.Context, string, bool, SyncScope) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true,"status":"pushed"}`), nil
}
func (stagedContractStub) AgentPull(context.Context, string, bool, SyncScope) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true,"status":"applied"}`), nil
}
func (stagedContractStub) AgentResolveRecord(_ context.Context, _ string, req ResolveRecordRequest) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true,"action":"record","entries":[],"recorded":[{"adapter":"pi","choice":"center","recordedAt":"2026-10-09T00:00:00Z","generationAtRecord":1}],"pending":1,"errors":[]}`), nil
}
func (stagedContractStub) RemoveAgent(string) bool { return false }

func TestStagedDispatchChoicesContract(t *testing.T) {
	fixture := authFixture(t)
	server, err := NewServer(ServeOptions{Addr: "127.0.0.1:0", HomerHome: fixture.home, Agents: stagedContractStub{}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	// Seed a generation so the center has adapters to dispatch.
	if _, err := (gens.New(fixture.home)).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "x\n"},
	}, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	cookie := setupAndLogin(t, server.Handler())
	request, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/sync/choices?direction=dispatch&agent=box", nil)
	request.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(response.Body).Decode(&body)
	if response.StatusCode != 200 {
		t.Fatalf("dispatch choices = %d %v", response.StatusCode, body)
	}
	adapters, _ := body["adapters"].([]any)
	t.Logf("choices=%v", body)
	if len(adapters) == 0 {
		t.Fatal("no adapters in choices")
	}
	found := false
	for _, a := range adapters {
		m := a.(map[string]any)
		if m["id"] == "pi" && m["resolution"] != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("pi choice lacks resolution view")
	}
}
