package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	keyMu   sync.Mutex
	agentID string
	cmd     keyring.Command
	raw     []byte
}

func (s *recordingKeySource) AgentKey(_ context.Context, agentID string, cmd keyring.Command) (json.RawMessage, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	s.agentID = agentID
	s.cmd = cmd
	return append(json.RawMessage(nil), s.raw...), nil
}

// A dispatch of an adapter that has a key bound to it must carry that key and
// open it, or not happen at all. Writing the adapter's plain files while its
// credential file stays behind leaves a machine that looks synced but cannot
// authenticate, and nothing on it says why.
func dispatchFixtureWithBoundKey(t *testing.T) (*Server, *recordingKeySource) {
	t.Helper()
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
	destination := filepath.Join(root, "auth.json")
	if err := os.WriteFile(destination, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := keyring.Apply(homer, keyring.Command{Action: "create", ID: "pi", Name: "pi", Password: "long-password", WorkFactor: 14}); !r.OK {
		t.Fatalf("create = %#v", r)
	}
	if r := keyring.Apply(homer, keyring.Command{Action: "encrypt", ID: "pi", Path: destination, Adapter: "pi", Password: "long-password", WorkFactor: 14}); !r.OK {
		t.Fatalf("encrypt = %#v", r)
	}
	if _, err := gens.New(homer).Publish(map[string]map[string]string{
		"pi":      {"settings/settings.json": "x"},
		"herdr":   {"config/config.toml": "x"},
		"keyring": {"marker.txt": "x"},
	}, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	stub := &recordingKeySource{raw: []byte(`{"ok":true,"status":"unlocked"}`)}
	stub.pullRaw = []byte(`{"ok":true,"status":"applied"}`)
	return newWebServer(t, webFixture{home: homer, paths: paths}, "test-token", stub, nil), stub
}

func TestDispatchRefusesAdapterWithBoundKeyWhenKeyringNotCarried(t *testing.T) {
	server, stub := dispatchFixtureWithBoundKey(t)
	pull := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		return request(t, server.Handler(), http.MethodPost, "/api/agents/box/pull?confirm=true", body)
	}
	// pi has a key bound to it, but the request does not carry the keyring.
	refused := pull(`{"adapters":["pi"]}`)
	if refused.Code != http.StatusUnprocessableEntity || len(stub.pullValues) != 0 {
		t.Fatalf("pi without keyring = %d pulls=%d body=%s (want refused before anything is written)", refused.Code, len(stub.pullValues), refused.Body)
	}
	for _, want := range []string{"pi", "密钥"} {
		if !strings.Contains(refused.Body.String(), want) {
			t.Fatalf("refusal should name the adapter and say a key is needed (%q): %s", want, refused.Body)
		}
	}
	// An adapter with no key bound is unaffected.
	other := pull(`{"adapters":["herdr"]}`)
	if other.Code != http.StatusOK || len(stub.pullValues) != 1 {
		t.Fatalf("herdr has no bound key, dispatch = %d pulls=%d body=%s", other.Code, len(stub.pullValues), other.Body)
	}
	// Mixed selection: the bound adapter still forces the key.
	mixed := pull(`{"adapters":["herdr","pi"]}`)
	if mixed.Code != http.StatusUnprocessableEntity || len(stub.pullValues) != 1 {
		t.Fatalf("herdr+pi without keyring = %d pulls=%d body=%s", mixed.Code, len(stub.pullValues), mixed.Body)
	}
	// With the keyring and a correct password it goes through.
	ok := pull(`{"adapters":["pi","keyring"],"unlocks":[{"id":"pi","password":"long-password"}]}`)
	if ok.Code != http.StatusOK || len(stub.pullValues) != 2 {
		t.Fatalf("pi+keyring+password = %d pulls=%d body=%s", ok.Code, len(stub.pullValues), ok.Body)
	}
}

// A dry run (no confirm) only previews, so it must not be refused: the
// console builds its preview before asking for the password.
func TestDispatchPreviewIsNotRefusedForBoundKey(t *testing.T) {
	server, stub := dispatchFixtureWithBoundKey(t)
	preview := request(t, server.Handler(), http.MethodPost, "/api/agents/box/pull", `{"adapters":["pi"]}`)
	if preview.Code != http.StatusOK || len(stub.pullValues) != 1 {
		t.Fatalf("preview = %d pulls=%d body=%s", preview.Code, len(stub.pullValues), preview.Body)
	}
}

// Leaving the adapter list out means "everything in the center", which
// includes every adapter that has a key bound. It must be refused too: the
// gate cannot depend on the caller remembering to list adapters.
func TestDispatchUnrestrictedRefusedWhenAnyAdapterHasBoundKey(t *testing.T) {
	server, stub := dispatchFixtureWithBoundKey(t)
	refused := request(t, server.Handler(), http.MethodPost, "/api/agents/box/pull?confirm=true", ``)
	if refused.Code != http.StatusUnprocessableEntity || len(stub.pullValues) != 0 {
		t.Fatalf("unrestricted dispatch = %d pulls=%d body=%s", refused.Code, len(stub.pullValues), refused.Body)
	}
	if !strings.Contains(refused.Body.String(), "pi") {
		t.Fatalf("refusal should name pi: %s", refused.Body)
	}
}

// Every path that writes a machine from the center shares the one gate. The
// fan-out and the resolve-to-center paths have no per-machine password step,
// so they must refuse rather than write an adapter and strand its key.
func TestOtherDispatchPathsRefuseBoundKeyGap(t *testing.T) {
	cases := []struct {
		name, url, body string
	}{
		{"fanout dispatch, explicit adapter", "/api/sync?direction=dispatch&confirm=true", `{"adapters":["pi"]}`},
		{"fanout dispatch, unrestricted", "/api/sync?direction=dispatch&confirm=true", ``},
		{"resolve center", "/api/resolve?choice=center&agent=box&confirm=true", `{"adapters":["pi"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, stub := dispatchFixtureWithBoundKey(t)
			stub.list = []AgentInfo{{AgentID: "box", Hostname: "box"}}
			refused := request(t, server.Handler(), http.MethodPost, tc.url, tc.body)
			if refused.Code != http.StatusUnprocessableEntity || len(stub.pullValues) != 0 {
				t.Fatalf("%s = %d pulls=%d body=%s (want refused, nothing written)", tc.name, refused.Code, len(stub.pullValues), refused.Body)
			}
			if !strings.Contains(refused.Body.String(), "pi") || !strings.Contains(refused.Body.String(), "密钥") {
				t.Fatalf("%s: refusal should name the adapter and the key: %s", tc.name, refused.Body)
			}
		})
	}
}

// An adapter with no key bound still dispatches through the fan-out.
func TestFanoutDispatchStillWorksForAdapterWithoutBoundKey(t *testing.T) {
	server, stub := dispatchFixtureWithBoundKey(t)
	stub.list = []AgentInfo{{AgentID: "box", Hostname: "box"}}
	ok := request(t, server.Handler(), http.MethodPost, "/api/sync?direction=dispatch&confirm=true", `{"adapters":["herdr"]}`)
	if ok.Code != http.StatusOK || len(stub.pullValues) != 1 {
		t.Fatalf("fanout herdr = %d pulls=%d body=%s", ok.Code, len(stub.pullValues), ok.Body)
	}
}
