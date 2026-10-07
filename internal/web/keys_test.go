package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/keyring"
)

func TestKeyConsoleRoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	homer := filepath.Join(root, ".homer")
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return homer
		}
		return ""
	})
	if err := core.SaveConfig(paths, core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{}}); err != nil {
		t.Fatal(err)
	}
	// The HTTP handler uses the production scrypt work factor. Exercise the
	// keyring here with a test factor, and only check the list route over HTTP
	// so the console test stays fast.
	destination := filepath.Join(root, "providers.json")
	if err := os.WriteFile(destination, []byte("plain-marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := keyring.Apply(homer, keyring.Command{Action: "create", ID: "codebuddy", Name: "CodeBuddy", Password: "long-password", WorkFactor: 14})
	if !created.OK {
		t.Fatalf("create = %#v", created)
	}
	server := newWebServer(t, webFixture{home: homer, paths: paths}, "test-token", nil, nil)
	listed := request(t, server.Handler(), http.MethodGet, "/api/keys")
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), "long-password") || strings.Contains(listed.Body.String(), "AGE-SECRET-KEY-") {
		t.Fatalf("list = %d %s", listed.Code, listed.Body)
	}
	if !strings.Contains(listed.Body.String(), "codebuddy") {
		t.Fatalf("list missing key: %s", listed.Body)
	}
	page := string(staticIndex)
	for _, want := range []string{`id="sec-keys"`, "新建密钥", "加密文件", "解开"} {
		if !strings.Contains(page, want) {
			t.Fatalf("console missing %q", want)
		}
	}
}

func TestDispatchRequiresUnlockBeforeSend(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	homer := filepath.Join(root, ".homer")
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return homer
		}
		return ""
	})
	if err := core.SaveConfig(paths, core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{}}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "models.json")
	if err := os.WriteFile(destination, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := keyring.Apply(homer, keyring.Command{Action: "create", ID: "pi", Name: "pi", Password: "long-password", WorkFactor: 14})
	if !created.OK {
		t.Fatalf("create = %#v", created)
	}
	encrypted := keyring.Apply(homer, keyring.Command{Action: "encrypt", ID: "pi", Path: destination, Adapter: "pi", Password: "long-password", WorkFactor: 14})
	if !encrypted.OK {
		t.Fatalf("encrypt = %#v", encrypted)
	}
	if _, err := gens.New(homer).Publish(map[string]map[string]string{
		"pi":      {"settings/settings.json": "x"},
		"keyring": {"marker.txt": "x"},
	}, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	stub := &recordingKeySource{raw: []byte(`{"ok":true,"status":"unlocked"}`)}
	stub.pullRaw = []byte(`{"ok":true,"status":"applied"}`)
	server := newWebServer(t, webFixture{home: homer, paths: paths}, "test-token", stub, nil)
	pull := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		return request(t, server.Handler(), http.MethodPost, "/api/agents/box/pull?confirm=true", body)
	}
	missing := pull(`{"adapters":["pi","keyring"]}`)
	if missing.Code != http.StatusUnprocessableEntity || len(stub.pullValues) != 0 {
		t.Fatalf("missing password = %d pulls=%d body=%s", missing.Code, len(stub.pullValues), missing.Body)
	}
	if !strings.Contains(missing.Body.String(), "口令") || strings.Contains(missing.Body.String(), "long-password") {
		t.Fatalf("missing body = %s", missing.Body)
	}
	wrong := pull(`{"adapters":["pi","keyring"],"unlocks":[{"id":"pi","password":"wrong-password"}]}`)
	if wrong.Code != http.StatusUnprocessableEntity || len(stub.pullValues) != 0 {
		t.Fatalf("wrong password = %d pulls=%d body=%s", wrong.Code, len(stub.pullValues), wrong.Body)
	}
	if strings.Contains(wrong.Body.String(), "wrong-password") {
		t.Fatal("response echoed the password")
	}
	sent := pull(`{"adapters":["pi","keyring"],"unlocks":[{"id":"pi","password":"long-password"}]}`)
	if sent.Code != http.StatusOK || len(stub.pullValues) != 1 {
		t.Fatalf("dispatch = %d pulls=%d body=%s", sent.Code, len(stub.pullValues), sent.Body)
	}
	if stub.cmd.Action != "unlock" || stub.cmd.ID != "pi" || stub.cmd.Password != "long-password" {
		t.Fatalf("unlock = %+v", stub.cmd)
	}
	if strings.Contains(sent.Body.String(), "long-password") {
		t.Fatal("success response echoed the password")
	}
}

func TestAgentKeyRoute(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	plain := newWebServer(t, fixture, "test-token", &sourceStub{}, nil)
	missing := request(t, plain.Handler(), http.MethodPost, "/api/agents/box/keys", `{"action":"list"}`)
	if missing.Code != http.StatusNotImplemented {
		t.Fatalf("stub = %d %s", missing.Code, missing.Body)
	}
	stub := &recordingKeySource{raw: []byte(`{"ok":true,"status":"listed","keys":[]}`)}
	server := newWebServer(t, fixture, "test-token", stub, nil)
	ok := request(t, server.Handler(), http.MethodPost, "/api/agents/box/keys", `{"action":"unlock","id":"codebuddy","password":"secretpw"}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("proxied = %d %s", ok.Code, ok.Body)
	}
	if stub.agentID != "box" || stub.cmd.Action != "unlock" || stub.cmd.Password != "secretpw" {
		t.Fatalf("forwarded %+v %s", stub.cmd, stub.agentID)
	}
	if strings.Contains(ok.Body.String(), "secretpw") {
		t.Fatal("response echoed the password")
	}
}

type recordingKeySource struct {
	sourceStub
	agentID string
	cmd     keyring.Command
	raw     []byte
}

func (s *recordingKeySource) AgentKey(_ context.Context, agentID string, cmd keyring.Command) (json.RawMessage, error) {
	s.agentID = agentID
	s.cmd = cmd
	return append(json.RawMessage(nil), s.raw...), nil
}
