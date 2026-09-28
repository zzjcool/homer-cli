package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/gitx"
)

// Planner-frozen test names (MVP step 2). The sync API collapses
// push + fan-out pull into one manual action with human sentences only.

func syncPost(t *testing.T, handler http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/sync?"+query, nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestSyncRequiresConfirm(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	setGitIdentity(t, fixture.home)
	source := &sourceStub{}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := syncPost(t, server.Handler(), "direction=to-others")
	if response.Code != http.StatusConflict {
		t.Fatalf("sync without confirm = %d body=%s", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), `"aborted"`) {
		t.Fatalf("body = %s", response.Body)
	}
	if gitx.HeadCommit(fixture.home) != "" {
		t.Fatal("sync without confirm must not commit anything")
	}
	if len(source.pullValues) != 0 {
		t.Fatal("sync without confirm must not pull agents")
	}
}

func TestSyncToOthersPushesThenPullsOnlineAgents(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	setGitIdentity(t, fixture.home)
	source := &sourceStub{
		list: []AgentInfo{
			{AgentID: "online-1", Hostname: "box-online", Mode: "listen"},
			{AgentID: "offline-1", Hostname: "box-offline", Mode: "listen", Stale: true},
		},
		pullRaw: json.RawMessage(`{"ok":true}`),
	}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := syncPost(t, server.Handler(), "direction=to-others&confirm=true")
	if response.Code != http.StatusOK {
		t.Fatalf("sync = %d body=%s", response.Code, response.Body)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"status":"synced"`) || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("body = %s", body)
	}
	if len(source.pullValues) != 1 || !source.pullValues[0] {
		t.Fatalf("AgentPull confirm values = %v (want exactly one true)", source.pullValues)
	}
	if len(source.pulledAgent) != 1 || source.pulledAgent[0] != "online-1" {
		t.Fatalf("pulled agents = %v (stale machine must be skipped)", source.pulledAgent)
	}
	if len(source.pushValues) != 0 {
		t.Fatalf("AgentPush must not be called by sync, values = %v", source.pushValues)
	}
	if !strings.Contains(body, `"skipped":true`) {
		t.Fatalf("stale agent must be reported skipped: %s", body)
	}
}

func TestSyncToOthersDoesNotPullWhenPushRejected(t *testing.T) {
	fixture := makeFixture(t, "base\n", "sk-ant-abcdefghijklmnopqrstuvwxyz1234567890\n")
	setGitIdentity(t, fixture.home)
	source := &sourceStub{list: []AgentInfo{{AgentID: "online-1", Hostname: "box", Mode: "listen"}}}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := syncPost(t, server.Handler(), "direction=to-others&confirm=true")
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("secrets sync = %d body=%s", response.Code, response.Body)
	}
	body := response.Body.String()
	if !strings.Contains(body, "secrets-rejected") {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(body, "检测到疑似密钥，已停止") {
		t.Fatalf("human sentence missing: %s", body)
	}
	if len(source.pullValues) != 0 {
		t.Fatal("agents must not be pulled when the push is rejected")
	}
	for _, gitWord := range []string{"commit", "merge", "git "} {
		if strings.Contains(body, gitWord) {
			t.Fatalf("git word %q leaked into sync response: %s", gitWord, body)
		}
	}
}

func TestSyncFromCenterDoesNotFanout(t *testing.T) {
	fixture := pullFixture(t, "remote\n")
	source := &sourceStub{list: []AgentInfo{{AgentID: "online-1", Hostname: "box", Mode: "listen"}}}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := syncPost(t, server.Handler(), "direction=from-center&confirm=true")
	if response.Code != http.StatusOK {
		t.Fatalf("from-center = %d body=%s", response.Code, response.Body)
	}
	tool, err := os.ReadFile(filepath.Join(fixture.tool, "settings.json"))
	if err != nil || string(tool) != "remote\n" {
		t.Fatalf("tool file = %q err=%v (want remote content applied)", tool, err)
	}
	if len(source.pullValues) != 0 {
		t.Fatal("from-center must not fan out to agents")
	}
}

func TestSyncRejectsBadDirection(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	for _, query := range []string{"", "direction=sideways&confirm=true", "confirm=true"} {
		response := syncPost(t, server.Handler(), query)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("query %q = %d body=%s", query, response.Code, response.Body)
		}
		if !strings.Contains(response.Body.String(), "direction 必须是 to-others 或 from-center") {
			t.Fatalf("query %q body = %s", query, response.Body)
		}
	}
}
