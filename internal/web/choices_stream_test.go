package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/keyring"
)

func inspectCredentialPaths() []string {
	var paths []string
	for _, rule := range credentialRules() {
		paths = append(paths, rule.Destination)
	}
	return paths
}

func TestChoicesStream(t *testing.T) {
	t.Run("inspect events and final reconciliation", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "base\n")
		piCredential := credentialRulesFor("pi")[0]
		openCodeCredential := credentialRulesFor("opencode")[0]
		key := keyring.Summary{ID: "pi", Name: "Pi", Files: []keyring.FileInfo{}}
		status := commands.StatusReport{
			Adapters: []commands.StatusAdapterReport{
				{ID: "pi", Push: 2},
				{ID: "opencode", Pull: 1},
			},
			Errors: []string{},
		}
		present := map[string]bool{
			piCredential.Destination:       true,
			openCodeCredential.Destination: false,
		}
		secrets := []InspectSecret{{
			Path:        "pi/settings/settings.json",
			Description: "api token",
			Line:        7,
		}}
		keys := &keyring.Result{OK: true, Status: "listed", Keys: []keyring.Summary{key}}
		source := &sourceStub{
			statusRaw: json.RawMessage(`{"adapters":[],"errors":[]}`),
			inspect:   &InspectResult{Status: status, Present: present, Secrets: secrets, Keys: keys},
			inspectEvents: []InspectEvent{
				{Stage: "adapter", Done: 1, Total: 2, Adapter: &status.Adapters[0]},
				{Stage: "credentials", Done: 1, Total: 1, Present: present},
				{Stage: "secrets", Done: 1, Total: 1, Secrets: secrets},
				{Stage: "keys", Keys: keys},
			},
		}
		server := newWebServer(t, fixture, "test-token", source, nil)
		response := request(t, server.Handler(), http.MethodGet, "/api/sync/choices?direction=collect&agent=box&stream=1")
		if response.Code != http.StatusOK {
			t.Fatalf("stream = %d %s", response.Code, response.Body)
		}
		if got := response.Header().Get("Content-Type"); got != "application/x-ndjson" {
			t.Fatalf("Content-Type = %q", got)
		}
		lines := decodeChoicesNDJSON(t, response.Body.String())
		if len(lines) < 6 {
			t.Fatalf("expected progress, reconciliation, and done events; got %d: %s", len(lines), response.Body)
		}
		wantPrefix := []string{"adapter", "precheck", "precheck", "keys"}
		for i, want := range wantPrefix {
			if got := lines[i]["type"]; got != want {
				t.Fatalf("event[%d].type = %v, want %q; events=%#v", i, got, want, lines)
			}
		}
		if got := lines[len(lines)-1]["type"]; got != "done" {
			t.Fatalf("last event = %v, want done", got)
		}
		// ResponseRecorder gathers all writes into one body, matching a proxy
		// that delivers every NDJSON row in one network chunk.
		if count := strings.Count(response.Body.String(), "\n"); count != len(lines) {
			t.Fatalf("combined body has %d lines; parsed %d: %s", count, len(lines), response.Body)
		}
		var done struct {
			Type     string          `json:"type"`
			Hint     string          `json:"hint"`
			Adapters []AdapterChoice `json:"adapters"`
		}
		if err := json.Unmarshal(mustJSON(t, lines[len(lines)-1]), &done); err != nil {
			t.Fatal(err)
		}
		if done.Hint == "" || len(done.Adapters) != 2 || done.Adapters[0].ID != "opencode" || done.Adapters[1].ID != "pi" {
			t.Fatalf("done = %+v", done)
		}
		choices := map[string]AdapterChoice{}
		for _, choice := range done.Adapters {
			choices[choice.ID] = choice
		}
		pi := choices["pi"]
		if len(pi.Credentials) != len(credentialRulesFor("pi")) || pi.Credentials[0].Exists != credPresent {
			t.Fatalf("pi credentials = %#v", pi.Credentials)
		}
		if len(pi.SecretHits) != 1 || pi.SecretHits[0].Reason != "api token" || pi.SecretHits[0].Destination != fixture.tool+"/settings.json" {
			t.Fatalf("pi secret hits = %#v", pi.SecretHits)
		}
		if source.inspectCalls != 1 || source.inspectAgentID != "box" {
			t.Fatalf("inspect calls=%d agent=%q", source.inspectCalls, source.inspectAgentID)
		}
		if !source.inspectParams.WantKeys || !reflect.DeepEqual(source.inspectParams.Credentials, inspectCredentialPaths()) {
			t.Fatalf("inspect params = %+v", source.inspectParams)
		}
	})

	t.Run("errors are NDJSON events", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "base\n")
		source := &sourceStub{inspectErr: &AgentError{Code: "agent-timeout", Status: http.StatusGatewayTimeout, Err: errors.New("late")}}
		server := newWebServer(t, fixture, "test-token", source, nil)
		response := request(t, server.Handler(), http.MethodGet, "/api/sync/choices?direction=collect&agent=box&stream=1")
		lines := decodeChoicesNDJSON(t, response.Body.String())
		if response.Code != http.StatusOK || len(lines) != 1 || lines[0]["type"] != "error" || lines[0]["code"] != "agent-timeout" || lines[0]["message"] == "" {
			t.Fatalf("error stream = %d %#v", response.Code, lines)
		}
	})

	t.Run("request cancellation reaches inspect source", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "base\n")
		started := make(chan struct{})
		canceled := make(chan struct{})
		source := &sourceStub{
			inspectBlock:    true,
			inspectStarted:  started,
			inspectCanceled: canceled,
			inspectEvents: []InspectEvent{{
				Stage: "adapter", Done: 1, Total: 1,
				Adapter: &commands.StatusAdapterReport{ID: "pi", Push: 1},
			}},
		}
		server := newWebServer(t, fixture, "test-token", source, nil)
		httpServer := httptest.NewServer(server.Handler())
		defer httpServer.Close()
		client := &http.Client{Timeout: 5 * time.Second}
		request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/sync/choices?direction=collect&agent=box&stream=1", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer test-token")
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("stream request: %v", err)
		}
		line, err := bufio.NewReader(response.Body).ReadString('\n')
		if err != nil || !strings.Contains(line, `"type":"adapter"`) {
			_ = response.Body.Close()
			t.Fatalf("first progress line = %q, err=%v", line, err)
		}
		_ = response.Body.Close()
		select {
		case <-canceled:
		case <-time.After(2 * time.Second):
			t.Fatal("closing the client response did not cancel AgentInspect")
		}
	})

	t.Run("non-stream collect uses InspectResult", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "base\n")
		rule := credentialRulesFor("pi")[0]
		source := &sourceStub{inspect: &InspectResult{
			Status:  commands.StatusReport{Adapters: []commands.StatusAdapterReport{{ID: "pi", Push: 1}}},
			Present: map[string]bool{rule.Destination: true},
			Secrets: []InspectSecret{{Path: "pi/settings/settings.json", Description: "token"}},
		}}
		server := newWebServer(t, fixture, "test-token", source, nil)
		response := request(t, server.Handler(), http.MethodGet, "/api/sync/choices?direction=collect&agent=box")
		var body struct {
			OK        bool            `json:"ok"`
			Direction string          `json:"direction"`
			AgentID   string          `json:"agentId"`
			Adapters  []AdapterChoice `json:"adapters"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || !body.OK || body.Direction != "collect" || body.AgentID != "box" || len(body.Adapters) != 1 {
			t.Fatalf("choices = %d %+v body=%s", response.Code, body, response.Body)
		}
		if body.Adapters[0].Credentials[0].Exists != credPresent || len(body.Adapters[0].SecretHits) != 1 {
			t.Fatalf("collect enrichment = %+v", body.Adapters[0])
		}
	})

	t.Run("status fallback and non-collect remain on AgentStatus", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "base\n")
		fallback := &statusOnlySource{raw: json.RawMessage(`{"adapters":[{"id":"pi","push":1}],"errors":[]}`)}
		server := newWebServer(t, fixture, "test-token", fallback, nil)
		response := request(t, server.Handler(), http.MethodGet, "/api/sync/choices?direction=collect&agent=box")
		if response.Code != http.StatusOK || fallback.statusCalls != 1 || !strings.Contains(response.Body.String(), `"id":"pi"`) {
			t.Fatalf("fallback = %d calls=%d body=%s", response.Code, fallback.statusCalls, response.Body)
		}
		stream := request(t, server.Handler(), http.MethodGet, "/api/sync/choices?direction=collect&agent=box&stream=1")
		streamEvents := decodeChoicesNDJSON(t, stream.Body.String())
		if stream.Code != http.StatusOK || fallback.statusCalls != 2 || len(streamEvents) != 3 || streamEvents[0]["type"] != "adapter" || streamEvents[1]["type"] != "keys" || streamEvents[2]["type"] != "done" {
			t.Fatalf("stream fallback = %d calls=%d events=%#v body=%s", stream.Code, fallback.statusCalls, streamEvents, stream.Body)
		}

		source := &sourceStub{
			statusRaw: json.RawMessage(`{"adapters":[{"id":"pi","pull":2}],"errors":[]}`),
			inspect:   &InspectResult{Status: commands.StatusReport{Adapters: []commands.StatusAdapterReport{{ID: "wrong"}}}},
		}
		if _, err := gens.New(fixture.home).Publish(map[string]map[string]string{"pi": {"settings/settings.json": "center"}}, []byte("{}")); err != nil {
			t.Fatal(err)
		}
		server = newWebServer(t, fixture, "test-token", source, nil)
		response = request(t, server.Handler(), http.MethodGet, "/api/sync/choices?direction=dispatch&agent=box")
		if response.Code != http.StatusOK || source.inspectCalls != 0 || !strings.Contains(response.Body.String(), `"id":"pi"`) {
			t.Fatalf("dispatch = %d inspect=%d body=%s", response.Code, source.inspectCalls, response.Body)
		}
	})
}

func decodeChoicesNDJSON(t *testing.T, body string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	out := make([]map[string]any, 0, len(lines))
	for i, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("NDJSON line %d: %v: %q", i+1, err, line)
		}
		out = append(out, event)
	}
	return out
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type statusOnlySource struct {
	list        []AgentInfo
	raw         json.RawMessage
	statusCalls int
}

func (s *statusOnlySource) ListAgents() []AgentInfo { return append([]AgentInfo(nil), s.list...) }
func (s *statusOnlySource) AgentStatus(context.Context, string) (json.RawMessage, error) {
	s.statusCalls++
	return append(json.RawMessage(nil), s.raw...), nil
}
func (*statusOnlySource) AgentDiff(context.Context, string, DiffParams) (string, error) {
	return "", nil
}
func (*statusOnlySource) AgentPush(context.Context, string, bool, SyncScope) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true}`), nil
}
func (*statusOnlySource) AgentPull(context.Context, string, bool, SyncScope) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true}`), nil
}
func (*statusOnlySource) RemoveAgent(string) bool { return false }

func TestSnapshotETag(t *testing.T) {
	fixture := webFixture{home: t.TempDir()}
	server := newWebServer(t, fixture, "test-token", nil, nil)
	handler := server.Handler()

	requestWithETag := func(tag string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
		request.Header.Set("Authorization", "Bearer test-token")
		if tag != "" {
			request.Header.Set("If-None-Match", tag)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := requestWithETag(`"g1"`); response.Code != http.StatusConflict || response.Header().Get("ETag") != "" {
		t.Fatalf("missing snapshot = %d etag=%q body=%s", response.Code, response.Header().Get("ETag"), response.Body)
	}
	if response := request(t, handler, http.MethodPost, "/api/snapshot", `{"homerJson":"{}","store":{"pi":{"settings/settings.json":"one"}}}`); response.Code != http.StatusOK {
		t.Fatalf("publish = %d %s", response.Code, response.Body)
	}
	first := requestWithETag("")
	firstETag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || !regexp.MustCompile(`^"g1-[0-9a-f]{16}"$`).MatchString(firstETag) || first.Body.Len() == 0 {
		t.Fatalf("first download = %d etag=%q body=%s", first.Code, firstETag, first.Body)
	}
	for _, tag := range []string{firstETag, "W/" + firstETag, `"other", ` + firstETag, `*`} {
		response := requestWithETag(tag)
		if response.Code != http.StatusNotModified || response.Body.Len() != 0 || response.Header().Get("ETag") != firstETag {
			t.Errorf("If-None-Match %q = %d etag=%q body=%q", tag, response.Code, response.Header().Get("ETag"), response.Body.String())
		}
	}
	// A legacy generation-only validator cannot identify the content and must
	// be treated as stale, even when its generation number matches.
	if response := requestWithETag(`"g1"`); response.Code != http.StatusOK || response.Body.Len() == 0 {
		t.Fatalf("legacy generation-only ETag = %d %s, want full body", response.Code, response.Body)
	}
	if response := requestWithETag(`"g0"`); response.Code != http.StatusOK || response.Body.Len() == 0 {
		t.Fatalf("stale tag = %d %s", response.Code, response.Body)
	}
	if response := request(t, handler, http.MethodPost, "/api/snapshot", `{"homerJson":"{}","store":{"pi":{"settings/settings.json":"two"}}}`); response.Code != http.StatusOK {
		t.Fatalf("second publish = %d %s", response.Code, response.Body)
	}
	second := requestWithETag("")
	if second.Code != http.StatusOK || !regexp.MustCompile(`^"g2-[0-9a-f]{16}"$`).MatchString(second.Header().Get("ETag")) {
		t.Fatalf("second download = %d etag=%q", second.Code, second.Header().Get("ETag"))
	}
}

func TestFanoutBounded(t *testing.T) {
	const agentsCount = 24
	infos := make([]AgentInfo, agentsCount)
	for i := range infos {
		infos[i] = AgentInfo{AgentID: fmt.Sprintf("agent-%02d", i), Hostname: fmt.Sprintf("host-%02d", i), Stale: i%7 == 0}
	}
	started := make(chan string, agentsCount)
	release := make(chan struct{})
	source := &fanoutBoundSource{
		sourceStub: sourceStub{list: infos},
		started:    started,
		release:    release,
	}
	done := make(chan []agentApplyResult, 1)
	go func() { done <- fanoutPullOnlineAgents(context.Background(), source, SyncScope{}) }()

	for i := 0; i < 8; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatalf("only %d fanout tasks started; want the limit of 8", i)
		}
	}
	select {
	case id := <-started:
		close(release)
		t.Fatalf("fanout started a ninth concurrent pull (%s)", id)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)

	var results []agentApplyResult
	select {
	case results = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("fanout did not finish after workers were released")
	}
	if len(results) != len(infos) {
		t.Fatalf("got %d results, want %d", len(results), len(infos))
	}
	for i, result := range results {
		if result.AgentID != infos[i].AgentID || result.Hostname != infos[i].Hostname {
			t.Fatalf("result[%d] = %+v, want list order %s/%s", i, result, infos[i].AgentID, infos[i].Hostname)
		}
		if result.Skipped != infos[i].Stale {
			t.Errorf("result[%d] skipped=%v, stale=%v", i, result.Skipped, infos[i].Stale)
		}
	}
	source.mu.Lock()
	maxActive := source.maxActive
	source.mu.Unlock()
	if maxActive != 8 {
		t.Fatalf("max concurrent AgentPull = %d, want exactly 8", maxActive)
	}
}

type fanoutBoundSource struct {
	sourceStub
	started   chan<- string
	release   <-chan struct{}
	mu        sync.Mutex
	active    int
	maxActive int
}

func (s *fanoutBoundSource) AgentPull(ctx context.Context, agentID string, _ bool, _ SyncScope) (json.RawMessage, error) {
	s.mu.Lock()
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.mu.Unlock()
	s.started <- agentID
	select {
	case <-s.release:
	case <-ctx.Done():
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
		return nil, ctx.Err()
	}
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return json.RawMessage(`{"ok":true,"status":"applied"}`), nil
}

func TestCollectChoicesPrecheckRemoved(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	source := &sourceStub{statusRaw: json.RawMessage(`{"adapters":[],"errors":[]}`)}
	server := newWebServer(t, fixture, "test-token", source, nil)
	for _, path := range []string{
		"/api/sync/precheck?agent=box",
		"/api/ssh-key",
		"/api/upgrade",
		"/api/tools/upgrade",
		"/agent/v1/info",
	} {
		method := http.MethodGet
		if path == "/api/ssh-key" || path == "/api/upgrade" || path == "/api/tools/upgrade" {
			method = http.MethodPost
		}
		response := request(t, server.Handler(), method, path)
		if response.Code != http.StatusNotFound {
			t.Errorf("removed endpoint %s = %d %s", path, response.Code, response.Body)
		}
	}
	if source.inspectCalls != 0 {
		t.Fatalf("removed precheck triggered inspect %d times", source.inspectCalls)
	}
}
