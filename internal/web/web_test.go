package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/gitx"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

type webFixture struct {
	home  string
	paths core.HomerPaths
	tool  string
}

func makeFixture(t *testing.T, base, local string) webFixture {
	t.Helper()
	home := t.TempDir()
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return os.Getenv(key)
	})
	tool := filepath.Join(home, "tool")
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"pi": {
				Root: tool,
				Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
				},
			},
		},
	}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool, "settings.json"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot := core.AdapterSnapshot{
		AdapterID: "pi",
		Categories: []core.CategorySnapshot{{
			AdapterID: "pi",
			Category:  "settings",
			Mode:      core.SyncModeMirror,
			Files:     core.SnapshotFiles{"settings.json": {Kind: "file", Content: base}},
		}},
	}
	if err := core.WriteSnapshotToStore(paths, snapshot); err != nil {
		t.Fatal(err)
	}
	return webFixture{home: home, paths: paths, tool: tool}
}

func newWebServer(t *testing.T, fixture webFixture, token string, source AgentsSource, identity *AgentIdentity) *Server {
	t.Helper()
	server, err := NewServer(ServeOptions{
		Addr:      "127.0.0.1:0",
		HomerHome: fixture.home,
		Token:     token,
		Agents:    source,
		Identity:  identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestAgentListMarksOlderVersion(t *testing.T) {
	old := Version
	Version = "v1.4.0"
	t.Cleanup(func() { Version = old })
	fixture := makeFixture(t, "base\n", "base\n")
	source := &sourceStub{list: []AgentInfo{
		{AgentID: "old", Hostname: "old", Version: "v1.3.0"},
		{AgentID: "same", Hostname: "same", Version: "v1.4.0"},
		{AgentID: "newer", Hostname: "newer", Version: "v1.5.0"},
		{AgentID: "unknown", Hostname: "unknown"},
	}}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := request(t, server.Handler(), http.MethodGet, "/api/agents")
	if response.Code != http.StatusOK {
		t.Fatalf("list = %d %s", response.Code, response.Body)
	}
	var body struct {
		Agents []AgentInfo `json:"agents"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, agent := range body.Agents {
		got[agent.AgentID] = agent.Outdated
	}
	if !got["old"] || got["same"] || got["newer"] || got["unknown"] {
		t.Fatalf("outdated = %#v", got)
	}
}

func TestAgentUpgradeRoute(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	plain := newWebServer(t, fixture, "test-token", &sourceStub{}, nil)
	missing := request(t, plain.Handler(), http.MethodPost, "/api/agents/box/upgrade", `{}`)
	if missing.Code != http.StatusNotImplemented {
		t.Fatalf("missing upgrader = %d %s", missing.Code, missing.Body)
	}
	stub := &upgradeRouteStub{raw: []byte(`{"ok":true,"status":"upgraded"}`)}
	server := newWebServer(t, fixture, "test-token", stub, nil)
	ok := request(t, server.Handler(), http.MethodPost, "/api/agents/box/upgrade", `{}`)
	if ok.Code != http.StatusOK || !stub.called {
		t.Fatalf("upgrade = %d called=%v body=%s", ok.Code, stub.called, ok.Body)
	}
	stub.raw = []byte(`{"ok":false,"status":"error","note":"下载失败"}`)
	stub.called = false
	failed := request(t, server.Handler(), http.MethodPost, "/api/agents/box/upgrade", `{}`)
	if failed.Code != http.StatusUnprocessableEntity || !strings.Contains(failed.Body.String(), "下载失败") {
		t.Fatalf("failed upgrade = %d %s", failed.Code, failed.Body)
	}
}

type upgradeRouteStub struct {
	sourceStub
	called bool
	raw    json.RawMessage
}

func (s *upgradeRouteStub) AgentUpgrade(context.Context, string) (json.RawMessage, error) {
	s.called = true
	return append(json.RawMessage(nil), s.raw...), nil
}

func request(t *testing.T, handler http.Handler, method, path string, body ...string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if len(body) > 0 {
		reader = strings.NewReader(body[0])
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer test-token") // rule 6: no implicit loopback trust
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func requestWithToken(t *testing.T, handler http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("invalid JSON response: %v: %s", err, recorder.Body.String())
	}
	return value
}

func setGitIdentity(t *testing.T, home string) {
	t.Helper()
	if err := gitx.EnsureGitRepo(home); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "user.email", "web-test@example.invalid"}, {"config", "user.name", "web-test"}} {
		if result := gitx.Exec(home, args, 0); !result.OK {
			t.Fatalf("git %v: %s", args, result.Stderr)
		}
	}
}

func gitCommand(t *testing.T, home string, args ...string) string {
	t.Helper()
	result := gitx.Exec(home, args, 0)
	if !result.OK {
		t.Fatalf("git %v: %s", args, result.Stderr)
	}
	return strings.TrimSpace(result.Stdout)
}

func pullFixture(t *testing.T, content string) webFixture {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	home := filepath.Join(root, "home")
	remote := filepath.Join(root, "remote")
	if result := gitx.Exec(root, []string{"init", "--bare", "-b", "master", origin}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	fixture := makeFixtureAtHome(t, home, "base\n")
	setGitIdentity(t, home)
	gitCommand(t, home, "add", "homer.json", ".gitignore", "store")
	gitCommand(t, home, "commit", "-m", "base")
	gitCommand(t, home, "remote", "add", "origin", origin)
	gitCommand(t, home, "push", "-u", "origin", "master")
	gitCommand(t, root, "clone", origin, remote)
	for _, args := range [][]string{{"config", "user.email", "web-test@example.invalid"}, {"config", "user.name", "web-test"}} {
		if result := gitx.Exec(remote, args, 0); !result.OK {
			t.Fatal(result.Stderr)
		}
	}
	if err := os.WriteFile(filepath.Join(remote, "store", "pi", "settings", "settings.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, remote, "add", "store")
	gitCommand(t, remote, "commit", "-m", "remote update")
	gitCommand(t, remote, "push")
	return fixture
}

func makeFixtureAtHome(t *testing.T, home, base string) webFixture {
	t.Helper()
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return os.Getenv(key)
	})
	tool := filepath.Join(home, "tool")
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"pi": {
				Root: tool,
				Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
				},
			},
		},
	}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool, "settings.json"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := core.WriteSnapshotToStore(paths, core.AdapterSnapshot{
		AdapterID: "pi",
		Categories: []core.CategorySnapshot{{
			AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror,
			Files: core.SnapshotFiles{"settings.json": {Kind: "file", Content: base}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	return webFixture{home: home, paths: paths, tool: tool}
}

func TestHealthRoute(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "secret", nil, nil)
	response := request(t, server.Handler(), http.MethodGet, "/api/health")
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d, body=%s", response.Code, response.Body)
	}
	body := decodeBody(t, response)
	if body["ok"] != true || body["status"] != "healthy" || body["version"] == "" {
		t.Fatalf("health body = %#v", body)
	}
}

func TestStatusRoute(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	response := request(t, server.Handler(), http.MethodGet, "/api/status")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body)
	}
	body := decodeBody(t, response)
	report, ok := body["report"].(map[string]any)
	if !ok || len(report["adapters"].([]any)) != 1 {
		t.Fatalf("status report = %#v", body["report"])
	}
}

func TestStatusNotInitialized(t *testing.T) {
	home := t.TempDir()
	server := newWebServer(t, webFixture{home: home}, "test-token", nil, nil)
	// A fresh machine (no homer.json) answers with a LEGAL report: the
	// built-in adapters fall back to defaults and scan what is actually
	// installed, plus an errors marker line. The old 409
	// not-initialized surfaced as a misleading 502 "agent unreachable"
	// in the console's status drawer.
	response := request(t, server.Handler(), http.MethodGet, "/api/status")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body)
	}
	body := decodeBody(t, response)
	report := body["report"].(map[string]any)
	marked := false
	if errorsList, ok := report["errors"].([]any); ok {
		for _, item := range errorsList {
			if message, ok := item.(string); ok && strings.Contains(message, "未找到 homer 配置") {
				marked = true
			}
		}
	}
	if !marked {
		t.Fatalf("fresh machine report must carry the marker in errors: %#v", report["errors"])
	}
}

func TestStatusInvalidConfig(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return os.Getenv(key)
	})
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := newWebServer(t, webFixture{home: home}, "test-token", nil, nil)
	response := request(t, server.Handler(), http.MethodGet, "/api/status")
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body)
	}
	if code := decodeBody(t, response)["error"].(map[string]any)["code"]; code != "invalid-config" {
		t.Fatalf("error code = %v", code)
	}
}

func TestDiffRoute(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	response := request(t, server.Handler(), http.MethodGet, "/api/diff?adapter=pi")
	if response.Code != http.StatusOK {
		t.Fatalf("diff = %d, body=%s", response.Code, response.Body)
	}
	body := decodeBody(t, response)
	if body["empty"] != false || body["text"] == "" {
		t.Fatalf("diff body = %#v", body)
	}
	if err := os.WriteFile(filepath.Join(fixture.tool, "settings.json"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	response = request(t, server.Handler(), http.MethodGet, "/api/diff?adapter=pi&category=settings")
	body = decodeBody(t, response)
	if body["empty"] != true || body["text"] != "" {
		t.Fatalf("clean diff body = %#v", body)
	}
}

func TestPushRequiresConfirm(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	setGitIdentity(t, fixture.home)
	server := newWebServer(t, fixture, "test-token", nil, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/push")
	if response.Code != http.StatusConflict {
		t.Fatalf("push = %d, body=%s", response.Code, response.Body)
	}
	body := decodeBody(t, response)
	if body["status"] != "aborted" || body["ok"] != false {
		t.Fatalf("push body = %#v", body)
	}
	if gitx.HeadCommit(fixture.home) != "" {
		t.Fatal("aborted push created a commit")
	}
}

func TestPushConfirmed(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	setGitIdentity(t, fixture.home)
	server := newWebServer(t, fixture, "test-token", nil, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/push?confirm=true")
	if response.Code != http.StatusOK {
		t.Fatalf("push = %d, body=%s", response.Code, response.Body)
	}
	body := decodeBody(t, response)
	if body["ok"] != true || body["status"] != "pushed" || gitx.HeadCommit(fixture.home) == "" {
		t.Fatalf("push body = %#v", body)
	}
}

func TestPushSecretsRejected(t *testing.T) {
	fixture := makeFixture(t, "base\n", "sk-ant-abcdefghijklmnopqrstuvwxyz1234567890\n")
	setGitIdentity(t, fixture.home)
	server := newWebServer(t, fixture, "test-token", nil, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/push?confirm=true")
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("push = %d, body=%s", response.Code, response.Body)
	}
	body := decodeBody(t, response)
	if body["status"] != "secrets-rejected" || body["ok"] != false {
		t.Fatalf("push body = %#v", body)
	}
}

func TestPullRequiresConfirm(t *testing.T) {
	fixture := pullFixture(t, "remote\n")
	// The no-git data plane's center is a published hub generation.
	if _, err := gens.New(fixture.home).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "remote\n"},
	}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	server := newWebServer(t, fixture, "test-token", nil, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/pull")
	if response.Code != http.StatusConflict {
		t.Fatalf("pull = %d, body=%s", response.Code, response.Body)
	}
	body := decodeBody(t, response)
	if body["status"] != "aborted" || body["ok"] != false {
		t.Fatalf("pull body = %#v", body)
	}
	content, err := os.ReadFile(filepath.Join(fixture.tool, "settings.json"))
	if err != nil || string(content) != "base\n" {
		t.Fatalf("pull changed tool: %q, %v", content, err)
	}
}

func TestPullConfirmed(t *testing.T) {
	fixture := pullFixture(t, "remote\n")
	if _, err := gens.New(fixture.home).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "remote\n"},
	}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	server := newWebServer(t, fixture, "test-token", nil, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/pull?confirm=true")
	if response.Code != http.StatusOK {
		t.Fatalf("pull = %d, body=%s", response.Code, response.Body)
	}
	body := decodeBody(t, response)
	if body["ok"] != true || body["status"] != "applied" {
		t.Fatalf("pull body = %#v", body)
	}
	content, err := os.ReadFile(filepath.Join(fixture.tool, "settings.json"))
	if err != nil || string(content) != "remote\n" {
		t.Fatalf("pull did not apply: %q, %v", content, err)
	}
}

func TestConfigRoute(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	response := request(t, server.Handler(), http.MethodGet, "/api/config")
	if response.Code != http.StatusOK {
		t.Fatalf("config = %d, body=%s", response.Code, response.Body)
	}
	body := decodeBody(t, response)
	config, ok := body["config"].(map[string]any)
	if !ok || config["adapters"] == nil {
		t.Fatalf("config body = %#v", body)
	}
	value, err := orderedjson.Parse(response.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	object := value.(*orderedjson.Object)
	if len(object.Keys) != 2 || object.Keys[0] != "ok" || object.Keys[1] != "config" {
		t.Fatalf("response key order = %#v", object.Keys)
	}
	configBytes, err := os.ReadFile(fixture.paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configBytes), `"version"`) || !strings.Contains(response.Body.String(), `"version"`) {
		t.Fatal("config response omitted source keys")
	}
	missing := newWebServer(t, webFixture{home: t.TempDir()}, "test-token", nil, nil)
	response = request(t, missing.Handler(), http.MethodGet, "/api/config")
	if response.Code != http.StatusConflict {
		t.Fatalf("uninitialized config = %d, body=%s", response.Code, response.Body)
	}
	if code := decodeBody(t, response)["error"].(map[string]any)["code"]; code != "not-initialized" {
		t.Fatalf("uninitialized config error = %v", code)
	}
}

func TestAgentsRouteP1(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	response := request(t, server.Handler(), http.MethodGet, "/api/agents")
	if response.Code != http.StatusOK || decodeBody(t, response)["agents"] == nil {
		t.Fatalf("agents = %d, body=%s", response.Code, response.Body)
	}
	response = request(t, server.Handler(), http.MethodPost, "/api/agents/x/status")
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("agent status = %d, body=%s", response.Code, response.Body)
	}
	if code := decodeBody(t, response)["error"].(map[string]any)["code"]; code != "agents-disabled" {
		t.Fatalf("agent error = %v", code)
	}
}

type sourceStub struct {
	list        []AgentInfo
	removed     []string
	statusRaw   json.RawMessage
	diffText    string
	pushRaw     json.RawMessage
	pullRaw     json.RawMessage
	statusErr   error
	diffErr     error
	pushErr     error
	pullErr     error
	pushValues  []bool
	pullValues  []bool
	pushScopes  []SyncScope
	pullScopes  []SyncScope
	pulledAgent []string
	// onPush runs inside AgentPush — tests simulate a machine whose own
	// executor uploads into the hub storage here.
	onPush func()
	mu     sync.Mutex
}

func (s *sourceStub) ListAgents() []AgentInfo { return append([]AgentInfo(nil), s.list...) }

func (s *sourceStub) RemoveAgent(agentID string) bool {
	found := false
	kept := s.list[:0]
	for _, info := range s.list {
		if info.AgentID == agentID {
			found = true
			continue
		}
		kept = append(kept, info)
	}
	s.list = kept
	if found {
		s.removed = append(s.removed, agentID)
	}
	return found
}
func (s *sourceStub) AgentStatus(_ context.Context, _ string) (json.RawMessage, error) {
	return s.statusRaw, s.statusErr
}
func (s *sourceStub) AgentDiff(_ context.Context, _ string, _ DiffParams) (string, error) {
	return s.diffText, s.diffErr
}
func (s *sourceStub) AgentPush(_ context.Context, _ string, confirm bool, scope SyncScope) (json.RawMessage, error) {
	s.mu.Lock()
	s.pushValues = append(s.pushValues, confirm)
	s.pushScopes = append(s.pushScopes, scope)
	hook := s.onPush
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return s.pushRaw, s.pushErr
}
func (s *sourceStub) AgentPull(_ context.Context, agentID string, confirm bool, scope SyncScope) (json.RawMessage, error) {
	s.mu.Lock()
	s.pullValues = append(s.pullValues, confirm)
	s.pullScopes = append(s.pullScopes, scope)
	s.pulledAgent = append(s.pulledAgent, agentID)
	s.mu.Unlock()
	return s.pullRaw, s.pullErr
}

func TestAgentsRouteWithSource(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	source := &sourceStub{
		list:      []AgentInfo{{AgentID: "a", Hostname: "box", Mode: "listen"}},
		statusRaw: json.RawMessage(`{"adapters":[]}`),
		diffText:  "diff",
		pushRaw:   json.RawMessage(`{"ok":true,"status":"pushed"}`),
		pullRaw:   json.RawMessage(`{"ok":false,"status":"aborted"}`),
	}
	server := newWebServer(t, fixture, "test-token", source, nil)
	if response := request(t, server.Handler(), http.MethodGet, "/api/agents"); response.Code != http.StatusOK {
		t.Fatalf("list = %d", response.Code)
	}
	if response := request(t, server.Handler(), http.MethodPost, "/api/agents/a/status"); response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body)
	}
	if response := request(t, server.Handler(), http.MethodPost, "/api/agents/a/diff?adapter=pi&category=settings"); response.Code != http.StatusOK {
		t.Fatalf("diff = %d, body=%s", response.Code, response.Body)
	}
	if _, err := gens.New(fixture.home).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "base\n"},
	}, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	joined := &sourceStub{diffText: `{"local":"machine line\n","localOk":true}`}
	joinedServer := newWebServer(t, fixture, "test-token", joined, nil)
	response := request(t, joinedServer.Handler(), http.MethodPost, "/api/agents/a/diff?adapter=pi&category=settings&path=settings.json:theme")
	if response.Code != http.StatusOK {
		t.Fatalf("file diff = %d, body=%s", response.Code, response.Body)
	}
	var sides map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &sides); err != nil {
		t.Fatal(err)
	}
	if sides["local"] != "machine line\n" || sides["localOk"] != true || sides["remote"] != "base\n" || sides["remoteOk"] != true {
		t.Fatalf("sides = %#v", sides)
	}
	if response := request(t, server.Handler(), http.MethodPost, "/api/agents/a/push"); response.Code != http.StatusOK {
		t.Fatalf("push = %d, body=%s", response.Code, response.Body)
	}
	if response := request(t, server.Handler(), http.MethodPost, "/api/agents/a/pull?confirm=true"); response.Code != http.StatusConflict {
		t.Fatalf("pull = %d, body=%s", response.Code, response.Body)
	}
	if len(source.pushValues) != 1 || source.pushValues[0] || len(source.pullValues) != 1 || !source.pullValues[0] {
		t.Fatalf("confirm values push=%v pull=%v", source.pushValues, source.pullValues)
	}
	for _, testCase := range []struct {
		path string
		err  error
		code int
	}{
		{path: "/api/agents/a/status", err: &AgentError{Code: "agent-not-found", Status: http.StatusNotFound, Err: errors.New("gone")}, code: http.StatusNotFound},
		{path: "/api/agents/a/diff", err: &AgentError{Code: "agent-unreachable", Status: http.StatusBadGateway, Err: errors.New("down")}, code: http.StatusBadGateway},
		{path: "/api/agents/a/push", err: &AgentError{Code: "agent-timeout", Status: http.StatusGatewayTimeout, Err: errors.New("slow")}, code: http.StatusGatewayTimeout},
	} {
		switch {
		case strings.HasSuffix(testCase.path, "/status"):
			source.statusErr = testCase.err
		case strings.HasSuffix(testCase.path, "/diff"):
			source.diffErr = testCase.err
		case strings.HasSuffix(testCase.path, "/push"):
			source.pushErr = testCase.err
		}
		response := request(t, server.Handler(), http.MethodPost, testCase.path)
		if response.Code != testCase.code {
			t.Fatalf("%s = %d, body=%s", testCase.path, response.Code, response.Body)
		}
		source.statusErr, source.diffErr, source.pushErr = nil, nil, nil
	}
}

func TestAuthLoopbackNoToken(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	if response := request(t, server.Handler(), http.MethodGet, "/api/status"); response.Code != http.StatusOK {
		t.Fatalf("loopback status = %d, body=%s", response.Code, response.Body)
	}
}

func TestAuthTokenRequired(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "secret", nil, nil)
	if response := request(t, server.Handler(), http.MethodGet, "/api/status"); response.Code != http.StatusUnauthorized {
		t.Fatalf("missing token = %d", response.Code)
	}
	if response := requestWithToken(t, server.Handler(), http.MethodGet, "/api/status", "wrong"); response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d", response.Code)
	}
	if response := requestWithToken(t, server.Handler(), http.MethodGet, "/api/status", "secret"); response.Code != http.StatusOK {
		t.Fatalf("valid token = %d, body=%s", response.Code, response.Body)
	}
	if response := request(t, server.Handler(), http.MethodGet, "/api/health"); response.Code != http.StatusOK {
		t.Fatalf("health auth = %d", response.Code)
	}
	if response := request(t, server.Handler(), http.MethodGet, "/"); response.Code != http.StatusOK {
		t.Fatalf("index auth = %d", response.Code)
	}
}

func TestNonLoopbackRequiresToken(t *testing.T) {
	t.Setenv("HOMER_HOME", t.TempDir())
	if _, err := NewServer(ServeOptions{Addr: "0.0.0.0:7760"}); err == nil {
		t.Fatal("non-loopback server without token was accepted")
	}
}

func TestWriteMutexSerializes(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	setGitIdentity(t, fixture.home)
	server := newWebServer(t, fixture, "test-token", nil, nil)
	// Channel handoff: each goroutine reports through the channel; the
	// main goroutine alone touches the collected slice.
	var wait sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- request(t, server.Handler(), http.MethodPost, "/api/push?confirm=true")
		}()
	}
	wait.Wait()
	close(results)
	responses := make([]*httptest.ResponseRecorder, 0, 2)
	for response := range results {
		responses = append(responses, response)
	}
	for i, response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("push %d = %d, body=%s", i, response.Code, response.Body)
		}
	}
	if gitx.HeadCommit(fixture.home) == "" {
		t.Fatal("serialized push did not commit")
	}
}

func TestMethodNotAllowedAndNotFound(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	response := request(t, server.Handler(), http.MethodPut, "/api/status")
	if response.Code != http.StatusMethodNotAllowed || decodeBody(t, response)["error"].(map[string]any)["code"] != "method-not-allowed" {
		t.Fatalf("method response = %d, body=%s", response.Code, response.Body)
	}
	response = request(t, server.Handler(), http.MethodGet, "/api/nope")
	if response.Code != http.StatusNotFound || decodeBody(t, response)["error"].(map[string]any)["code"] != "not-found" {
		t.Fatalf("not found response = %d, body=%s", response.Code, response.Body)
	}
}

func TestStaticIndexServed(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "secret", nil, nil)
	response := request(t, server.Handler(), http.MethodGet, "/")
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("index = %d content-type=%q", response.Code, response.Header().Get("Content-Type"))
	}
	for _, id := range []string{"gate-setup", "gate-login", "hero", "agent-list"} {
		if !strings.Contains(response.Body.String(), `id="`+id+`"`) {
			t.Fatalf("index missing %s", id)
		}
	}
	response = request(t, server.Handler(), http.MethodGet, "/static/../homer.json")
	if response.Code != http.StatusNotFound {
		t.Fatalf("traversal = %d, body=%s", response.Code, response.Body)
	}
}

func TestAgentInfoRoute(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	identity := &AgentIdentity{AgentID: "agent-a", Hostname: "box-a", Version: "v1"}
	server := newWebServer(t, fixture, "secret", nil, identity)
	response := requestWithToken(t, server.Handler(), http.MethodGet, "/agent/v1/info", "secret")
	if response.Code != http.StatusOK {
		t.Fatalf("info = %d, body=%s", response.Code, response.Body)
	}
	body := decodeBody(t, response)
	got := body["identity"].(map[string]any)
	if got["agentId"] != identity.AgentID || got["hostname"] != identity.Hostname || got["version"] != identity.Version {
		t.Fatalf("identity = %#v", got)
	}
}
