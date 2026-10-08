package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/toolctl"
)

func testTool(id, version string) AgentTool {
	return AgentTool{Status: toolctl.Status{ID: id, Label: id, Version: version, Upgradable: true}}
}

func toolsOf(agents []AgentInfo, agentID string) map[string]AgentTool {
	for _, agent := range agents {
		if agent.AgentID == agentID {
			out := map[string]AgentTool{}
			for _, t := range agent.Tools {
				out[t.ID] = t
			}
			return out
		}
	}
	return nil
}

func TestAnnotateToolsMeasuresAgainstTheFleet(t *testing.T) {
	agents := []AgentInfo{
		{AgentID: "old", Tools: []AgentTool{testTool("pi", "0.80.0"), testTool("herdr", "0.9.1")}},
		{AgentID: "mid", Tools: []AgentTool{testTool("pi", "0.87.1")}},
		{AgentID: "new", Tools: []AgentTool{testTool("pi", "0.90.2")}},
		{AgentID: "bare"},
	}
	annotateTools(agents)

	if got := toolsOf(agents, "old")["pi"]; got.Latest != "0.90.2" || !got.Outdated {
		t.Fatalf("old pi = %+v, want outdated against 0.90.2", got)
	}
	if got := toolsOf(agents, "mid")["pi"]; got.Latest != "0.90.2" || !got.Outdated {
		t.Fatalf("mid pi = %+v, want outdated against 0.90.2", got)
	}
	if got := toolsOf(agents, "new")["pi"]; got.Latest != "0.90.2" || got.Outdated {
		t.Fatalf("new pi = %+v, the newest machine must not be flagged", got)
	}
	// Only one machine has herdr: nothing to compare with, so no verdict.
	if got := toolsOf(agents, "old")["herdr"]; got.Outdated {
		t.Fatalf("a single-machine program was flagged: %+v", got)
	}
	if len(agents[3].Tools) != 0 {
		t.Fatalf("a machine without programs grew some: %+v", agents[3].Tools)
	}
}

func TestAnnotateToolsDoesNotGuess(t *testing.T) {
	broken := testTool("pi", "")
	broken.Error = "读不出版本：输出里没有版本号"
	agents := []AgentInfo{
		{AgentID: "unreadable", Tools: []AgentTool{broken}},
		{AgentID: "known", Tools: []AgentTool{testTool("pi", "0.87.1")}},
		{AgentID: "beta", Tools: []AgentTool{testTool("pi", "1.0.0-beta.1")}},
		{AgentID: "stable", Tools: []AgentTool{testTool("pi", "0.87.1")}},
	}
	annotateTools(agents)

	if got := toolsOf(agents, "unreadable")["pi"]; got.Outdated {
		t.Fatalf("a machine whose version could not be read was flagged: %+v", got)
	}
	// A machine on a beta must not make every stable machine look old.
	if got := toolsOf(agents, "known")["pi"]; got.Outdated || got.Latest != "0.87.1" {
		t.Fatalf("stable pi next to a beta = %+v, want latest 0.87.1 and no flag", got)
	}
	if got := toolsOf(agents, "beta")["pi"]; got.Outdated {
		t.Fatalf("the beta machine was flagged: %+v", got)
	}
}

func TestAnnotateToolsCountsStaleMachinesAsEvidence(t *testing.T) {
	// The newest release was last seen on a machine that has since gone
	// offline; it is still proof that the release exists.
	agents := []AgentInfo{
		{AgentID: "online", Tools: []AgentTool{testTool("pi", "0.80.0")}},
		{AgentID: "offline", Stale: true, Tools: []AgentTool{testTool("pi", "0.90.2")}},
	}
	annotateTools(agents)
	if got := toolsOf(agents, "online")["pi"]; got.Latest != "0.90.2" || !got.Outdated {
		t.Fatalf("online pi = %+v", got)
	}
}

func TestAgentsRouteListsToolsWithTheirVerdict(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	source := &sourceStub{list: []AgentInfo{
		{AgentID: "a", Hostname: "a", Tools: []AgentTool{testTool("pi", "0.80.0")}},
		{AgentID: "b", Hostname: "b", Tools: []AgentTool{testTool("pi", "0.90.2")}},
	}}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := request(t, server.Handler(), http.MethodGet, "/api/agents")
	if response.Code != http.StatusOK {
		t.Fatalf("agents = %d %s", response.Code, response.Body)
	}
	var body struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	first := body.Agents[0]["tools"].([]any)[0].(map[string]any)
	// The wire shape the console reads: one flat object per program.
	for key, want := range map[string]any{"id": "pi", "version": "0.80.0", "latest": "0.90.2", "outdated": true, "upgradable": true} {
		if first[key] != want {
			t.Fatalf("tools[0][%q] = %v, want %v (whole entry %v)", key, first[key], want, first)
		}
	}
	second := body.Agents[1]["tools"].([]any)[0].(map[string]any)
	if _, flagged := second["outdated"]; flagged {
		t.Fatalf("the newest machine carries an outdated flag: %v", second)
	}
}

// toolUpgradeStub is an AgentsSource that can upgrade programs.
type toolUpgradeStub struct {
	sourceStub
	mu    sync.Mutex
	calls [][2]string
	raw   json.RawMessage
	err   error
}

func (s *toolUpgradeStub) AgentToolUpgrade(_ context.Context, agentID, tool string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, [2]string{agentID, tool})
	return append(json.RawMessage(nil), s.raw...), s.err
}

func (s *toolUpgradeStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func TestAgentToolUpgradeRouteNeedsASourceThatCan(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	plain := newWebServer(t, fixture, "test-token", &sourceStub{}, nil)
	response := request(t, plain.Handler(), http.MethodPost, "/api/agents/box/tool-upgrade", `{"tool":"pi"}`)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("source without the capability = %d %s", response.Code, response.Body)
	}
	none := newWebServer(t, fixture, "test-token", nil, nil)
	if response := request(t, none.Handler(), http.MethodPost, "/api/agents/box/tool-upgrade", `{"tool":"pi"}`); response.Code != http.StatusNotImplemented {
		t.Fatalf("no source at all = %d %s", response.Code, response.Body)
	}
}

func TestAgentToolUpgradeRouteSuccess(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	report := `{"ok":true,"status":"upgraded","tool":"pi","before":"0.80.0","after":"0.90.2","note":"pi 0.80.0 → 0.90.2"}`
	stub := &toolUpgradeStub{raw: json.RawMessage(report)}
	server := newWebServer(t, fixture, "test-token", stub, nil)

	response := request(t, server.Handler(), http.MethodPost, "/api/agents/box/tool-upgrade", `{"tool":" pi "}`)
	if response.Code != http.StatusOK {
		t.Fatalf("upgrade = %d %s", response.Code, response.Body)
	}
	if response.Body.String() != report {
		t.Fatalf("body = %s, want the machine's report as is", response.Body)
	}
	if len(stub.calls) != 1 || stub.calls[0] != [2]string{"box", "pi"} {
		t.Fatalf("calls = %v, want one call for box/pi (tool trimmed)", stub.calls)
	}
}

func TestAgentToolUpgradeRouteFailureKeepsTheDetail(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	failure := `{"ok":false,"status":"failed","tool":"pi","label":"pi","before":"0.80.0","note":"pi 升级没有成功：exit status 1","output":"npm ERR! code EACCES\n\nnpm ERR! permission denied\n","manual":"curl -fsSL https://pi.dev/install.sh | sh"}`
	stub := &toolUpgradeStub{raw: json.RawMessage(failure)}
	server := newWebServer(t, fixture, "test-token", stub, nil)

	response := request(t, server.Handler(), http.MethodPost, "/api/agents/box/tool-upgrade", `{"tool":"pi"}`)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("failed upgrade = %d %s", response.Code, response.Body)
	}
	var body struct {
		Error struct {
			Code    string   `json:"code"`
			Message string   `json:"message"`
			Details []string `json:"details"`
		} `json:"error"`
		Report struct {
			OK     bool   `json:"ok"`
			Status string `json:"status"`
			Manual string `json:"manual"`
		} `json:"report"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v: %s", err, response.Body)
	}
	if body.Error.Code != "tool-upgrade-failed" || body.Error.Message != "pi 升级没有成功：exit status 1" {
		t.Fatalf("error = %+v", body.Error)
	}
	if len(body.Error.Details) != 2 || body.Error.Details[0] != "npm ERR! code EACCES" || body.Error.Details[1] != "npm ERR! permission denied" {
		t.Fatalf("details = %q, want the output lines without blanks", body.Error.Details)
	}
	if body.Report.OK || body.Report.Status != "failed" || body.Report.Manual != "curl -fsSL https://pi.dev/install.sh | sh" {
		t.Fatalf("report = %+v, want the whole report so the console can offer the installer", body.Report)
	}
}

func TestAgentToolUpgradeRouteFailureWithoutANote(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	stub := &toolUpgradeStub{raw: json.RawMessage(`{"ok":false,"status":"failed"}`)}
	server := newWebServer(t, fixture, "test-token", stub, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/agents/box/tool-upgrade", `{"tool":"pi"}`)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "升级没有完成") {
		t.Fatalf("noteless failure = %d %s", response.Code, response.Body)
	}
}

func TestAgentToolUpgradeRouteRejectsBadRequests(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	stub := &toolUpgradeStub{raw: json.RawMessage(`{"ok":true}`)}
	server := newWebServer(t, fixture, "test-token", stub, nil)

	for name, body := range map[string]string{
		"empty body":         ``,
		"not json":           `pi`,
		"no tool":            `{}`,
		"blank tool":         `{"tool":"   "}`,
		"path":               `{"tool":"../../bin/sh"}`,
		"shell metacharters": `{"tool":"pi; rm -rf /"}`,
		"command line":       `{"tool":"pi update --self"}`,
		"upper case":         `{"tool":"Pi"}`,
		"leading dash":       `{"tool":"-pi"}`,
		"too long":           `{"tool":"` + strings.Repeat("a", 41) + `"}`,
		"a list":             `{"tool":["pi"]}`,
	} {
		response := request(t, server.Handler(), http.MethodPost, "/api/agents/box/tool-upgrade", body)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", name, response.Code, response.Body)
		}
	}
	if stub.callCount() != 0 {
		t.Fatalf("a rejected request still reached the machine: %v", stub.calls)
	}
	if response := request(t, server.Handler(), http.MethodGet, "/api/agents/box/tool-upgrade"); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", response.Code)
	}
}

func TestAgentToolUpgradeRouteAcceptsEveryRegisteredToolID(t *testing.T) {
	// The shape check must never reject a tool the adapters really register.
	fixture := makeFixture(t, "base\n", "base\n")
	stub := &toolUpgradeStub{raw: json.RawMessage(`{"ok":true}`)}
	server := newWebServer(t, fixture, "test-token", stub, nil)
	ids := []string{"tool-2", "a.b_c"}
	for _, registered := range adapter.Tools() {
		ids = append(ids, registered.ID)
	}
	for _, id := range ids {
		response := request(t, server.Handler(), http.MethodPost, "/api/agents/box/tool-upgrade", `{"tool":"`+id+`"}`)
		if response.Code != http.StatusOK {
			t.Errorf("%q: status = %d %s", id, response.Code, response.Body)
		}
	}
}

func TestAgentToolUpgradeRouteMapsMachineErrors(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{&AgentError{Code: "agent-offline", Status: http.StatusServiceUnavailable, Err: errors.New("机器 box 离线")}, http.StatusServiceUnavailable, "agent-offline"},
		{&AgentError{Code: "agent-timeout", Status: http.StatusGatewayTimeout, Err: errors.New("slow")}, http.StatusGatewayTimeout, "agent-timeout"},
		{&AgentError{Code: "agent-not-found", Status: http.StatusNotFound, Err: errors.New("gone")}, http.StatusNotFound, "agent-not-found"},
	} {
		stub := &toolUpgradeStub{err: c.err}
		server := newWebServer(t, fixture, "test-token", stub, nil)
		response := request(t, server.Handler(), http.MethodPost, "/api/agents/box/tool-upgrade", `{"tool":"pi"}`)
		if response.Code != c.status {
			t.Errorf("%s: status = %d, want %d (%s)", c.code, response.Code, c.status, response.Body)
		}
		if got := decodeBody(t, response)["error"].(map[string]any)["code"]; got != c.code {
			t.Errorf("error code = %v, want %s", got, c.code)
		}
	}
}

func TestAgentToolUpgradeRouteRefusesAnEmptyReport(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	for _, raw := range []string{``, `not json`} {
		stub := &toolUpgradeStub{raw: json.RawMessage(raw)}
		server := newWebServer(t, fixture, "test-token", stub, nil)
		response := request(t, server.Handler(), http.MethodPost, "/api/agents/box/tool-upgrade", `{"tool":"pi"}`)
		if response.Code != http.StatusInternalServerError {
			t.Errorf("report %q: status = %d, want 500 (%s)", raw, response.Code, response.Body)
		}
	}
}

func TestAgentWebRoutesAreRemoved(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	for _, path := range []struct {
		name   string
		method string
	}{{"/api/ssh-key", http.MethodPost}, {"/api/upgrade", http.MethodPost}, {"/api/tools/upgrade", http.MethodPost}} {
		t.Run(path.name, func(t *testing.T) {
			response := request(t, server.Handler(), path.method, path.name)
			if response.Code != http.StatusNotFound {
				t.Fatalf("removed route = %d %s", response.Code, response.Body)
			}
		})
	}
}
