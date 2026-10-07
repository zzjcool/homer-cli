package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
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
