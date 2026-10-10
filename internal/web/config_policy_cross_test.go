package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/gens"
)

// configPolicyStub mirrors stagedContractStub for the config-policy seam: it
// reports a machine whose pi declaration lacks the files category (the exact
// drift that once deadlocked dispatches), records what it was asked to pull,
// and succeeds.
type configPolicyStub struct {
	pulls []SyncScope
}

func (configPolicyStub) ListAgents() []AgentInfo {
	return []AgentInfo{{AgentID: "box", Hostname: "box", LastSeen: time.Now()}}
}

func (configPolicyStub) AgentStatus(context.Context, string) (json.RawMessage, error) {
	// Machine outline: pi without the files category — the center (below)
	// publishes pi with it.
	return json.RawMessage(`{"adapters":[{"id":"pi","push":1,"pull":1,"conflicts":0}],"errors":[],` +
		`"configOutline":[{"id":"pi","root":"~/.pi/agent","categories":[{"name":"settings","paths":["settings.json"]}]}]}`), nil
}

func (configPolicyStub) AgentDiff(context.Context, string, DiffParams) (string, error) {
	return "d", nil
}
func (configPolicyStub) AgentPush(context.Context, string, bool, SyncScope) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true,"status":"pushed"}`), nil
}
func (configPolicyStub) AgentPull(_ context.Context, _ string, _ bool, scope SyncScope) (json.RawMessage, error) {
	stub := configPolicyPulls.current
	stub.pulls = append(stub.pulls, scope)
	return json.RawMessage(`{"ok":true,"status":"applied"}`), nil
}
func (configPolicyStub) AgentResolveRecord(_ context.Context, _ string, req ResolveRecordRequest) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true,"action":"record","entries":[],"recorded":[],"pending":0,"errors":[]}`), nil
}
func (configPolicyStub) RemoveAgent(string) bool { return false }

// configPolicyPulls hands the stub a recording slot the test can read without
// racing the HTTP handler goroutine on the stub's own field.
var configPolicyPulls struct{ current *configPolicyStub }

// TestConfigPolicyCrossCut walks the whole seam the three workers built:
// the drift the machine reports in its status outline surfaces on the
// dispatch choices row, and the configPolicy the request body carries flows
// through readSyncScope into the agent task scope.
func TestConfigPolicyCrossCut(t *testing.T) {
	fixture := authFixture(t)
	stub := &configPolicyStub{}
	configPolicyPulls.current = stub
	server, err := NewServer(ServeOptions{Addr: "127.0.0.1:0", HomerHome: fixture.home, Agents: stub})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	// Center generation: pi carries the files category the machine lacks.
	centerMeta := []byte(`{"version":1,"adapters":{"pi":{"root":"~/.pi/agent","enabled":true,` +
		`"categories":{"settings":{"paths":["settings.json"],"mode":"merge"},` +
		`"files":{"paths":["./"],"mode":"mirror"}}}}}` + "\n")
	if _, err := (gens.New(fixture.home)).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "x\n", "files/new.txt": "y\n"},
	}, centerMeta); err != nil {
		t.Fatal(err)
	}
	cookie := setupAndLogin(t, server.Handler())

	// 1. The drift surfaces on the dispatch choices row.
	request, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/sync/choices?direction=dispatch&agent=box", nil)
	request.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var choices struct {
		Adapters []struct {
			ID          string       `json:"id"`
			ConfigDrift *ConfigDrift `json:"configDrift"`
		} `json:"adapters"`
	}
	_ = json.NewDecoder(response.Body).Decode(&choices)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("dispatch choices = %d", response.StatusCode)
	}
	var drift *ConfigDrift
	for _, row := range choices.Adapters {
		if row.ID == "pi" {
			drift = row.ConfigDrift
		}
	}
	if drift == nil {
		t.Fatal("pi choices row carries no configDrift")
	}
	if len(drift.NewCategories) != 1 || drift.NewCategories[0] != "files" {
		t.Fatalf("newCategories = %v, want [files]", drift.NewCategories)
	}

	// 2. The policy the UI posts reaches the agent pull scope.
	pullBody := `{"adapters":["pi"],"configPolicy":{"pi":"keep"}}`
	request, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/agents/box/pull?confirm=true", strings.NewReader(pullBody))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	_ = json.NewDecoder(response.Body).Decode(&result)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("dispatch = %d %v", response.StatusCode, result)
	}
	if len(stub.pulls) != 1 {
		t.Fatalf("agent pulls = %d, want 1", len(stub.pulls))
	}
	pulled := stub.pulls[0]
	if !pulled.Explicit || len(pulled.Adapters) != 1 || pulled.Adapters[0] != "pi" {
		t.Fatalf("pull scope = %+v", pulled)
	}
	if pulled.ConfigPolicy["pi"] != "keep" {
		t.Fatalf("pull configPolicy = %v, want pi:keep", pulled.ConfigPolicy)
	}

	// 3. An invalid policy value is refused before any agent task.
	request, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/agents/box/pull?confirm=true",
		strings.NewReader(`{"adapters":["pi"],"configPolicy":{"pi":"maybe"}}`))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid configPolicy status = %d, want 400", response.StatusCode)
	}
}
