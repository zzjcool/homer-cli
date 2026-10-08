package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

// A collect must tell the user which credential files will NOT travel in
// plaintext, so they can encrypt them instead of silently losing them.
func TestCollectChoicesReportCredentialFiles(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	stub := &recordingKeySource{raw: []byte(`{"ok":true,"status":"checked","exists":{"~/.pi/agent/auth.json":true,"~/.pi/agent/mcp-auth.json":false,"~/.local/share/opencode/auth.json":false}}`)}
	stub.statusRaw = []byte(`{"adapters":[{"id":"pi","push":1},{"id":"herdr"},{"id":"opencode"}],"errors":[]}`)
	server := newWebServer(t, fixture, "test-token", stub, nil)

	response := request(t, server.Handler(), http.MethodGet, "/api/sync/precheck?agent=box")
	if response.Code != http.StatusOK {
		t.Fatalf("precheck = %d %s", response.Code, response.Body)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"credentials"`) || !strings.Contains(body, `"destination":"~/.pi/agent/auth.json"`) {
		t.Fatalf("pi credentials missing: %s", body)
	}
	if !strings.Contains(body, `"destination":"~/.local/share/opencode/auth.json"`) {
		t.Fatalf("opencode credentials missing: %s", body)
	}
	if strings.Contains(body, `"herdr"`) {
		t.Fatalf("herdr has no credential file: %s", body)
	}
}

func TestProbeCredentials(t *testing.T) {
	paths := []string{"~/.pi/agent/auth.json", "~/.pi/agent/mcp-auth.json"}
	ok := &recordingKeySource{raw: []byte(`{"ok":true,"status":"checked","exists":{"~/.pi/agent/auth.json":true,"~/.pi/agent/mcp-auth.json":false}}`)}
	got := probeCredentials(context.Background(), ok, "box", paths)
	if !got["~/.pi/agent/auth.json"] || got["~/.pi/agent/mcp-auth.json"] {
		t.Fatalf("exists = %v", got)
	}
	if ok.cmd.Action != "exists" || len(ok.cmd.Paths) != 2 {
		t.Fatalf("one batched request expected, got %+v", ok.cmd)
	}
	if got := probeCredentials(context.Background(), nil, "box", paths); got != nil {
		t.Fatalf("no actor = %v", got)
	}
	broken := &recordingKeySource{raw: []byte(`{"ok":false}`)}
	if got := probeCredentials(context.Background(), broken, "box", paths); got != nil {
		t.Fatalf("failed probe = %v", got)
	}
}

// The files a machine's secret scanner would refuse must be known before the
// user presses 收取, with the path on the machine so they can encrypt it.
func TestCollectChoicesListSecretHitsBeforeCollecting(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	stub := &recordingKeySource{raw: []byte(`{"ok":true,"status":"checked","exists":{}}`)}
	stub.statusRaw = []byte(`{"adapters":[{"id":"pi","push":1}],"errors":[]}`)
	stub.pushRaw = []byte(`{"ok":false,"status":"secrets-rejected","secrets":[{"path":"pi/settings/settings.json","description":"api key","line":7}]}`)
	server := newWebServer(t, fixture, "test-token", stub, nil)

	response := request(t, server.Handler(), http.MethodGet, "/api/sync/precheck?agent=box")
	if response.Code != http.StatusOK {
		t.Fatalf("precheck = %d %s", response.Code, response.Body)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"secretHits"`) || !strings.Contains(body, `"path":"pi/settings/settings.json"`) {
		t.Fatalf("secret hit missing: %s", body)
	}
	if len(stub.pushValues) != 1 || stub.pushValues[0] {
		t.Fatalf("preflight must not confirm the collect: %v", stub.pushValues)
	}
}

func TestSecretDestinationMapsStorePathToMachinePath(t *testing.T) {
	config := &core.HomerConfig{Adapters: map[string]core.AdapterConfig{
		"pi": {Root: "~/.pi/agent", Categories: map[string]core.CategoryConfig{
			"files": {Paths: []string{"./"}, Mode: core.SyncModeMirror},
		}},
	}}
	adapterID, destination := secretDestination(config, "pi/files/web-search.json")
	if adapterID != "pi" || destination != "~/.pi/agent/web-search.json" {
		t.Fatalf("got %q %q", adapterID, destination)
	}
	if id, dest := secretDestination(config, "pi/nope/x"); id != "pi" || dest != "" {
		t.Fatalf("unknown category = %q %q", id, dest)
	}
	if id, dest := secretDestination(nil, "pi/files/x"); id != "pi" || dest != "" {
		t.Fatalf("no config = %q %q", id, dest)
	}
}

// The adapter list must not wait on the machine-side checks: they are queued
// tasks, each a polling round trip, and live on their own endpoint.
func TestCollectChoicesDoNotWaitOnPrecheck(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	stub := &recordingKeySource{raw: []byte(`{"ok":true,"status":"checked","exists":{}}`)}
	stub.statusRaw = []byte(`{"adapters":[{"id":"pi","push":1}],"errors":[]}`)
	stub.pushRaw = []byte(`{"ok":false,"status":"secrets-rejected","secrets":[{"path":"pi/x/y.json"}]}`)
	server := newWebServer(t, fixture, "test-token", stub, nil)
	response := request(t, server.Handler(), http.MethodGet, "/api/sync/choices?direction=collect&agent=box")
	if response.Code != http.StatusOK {
		t.Fatalf("choices = %d %s", response.Code, response.Body)
	}
	if strings.Contains(response.Body.String(), "secretHits") || strings.Contains(response.Body.String(), "credentials") {
		t.Fatalf("choices must come back without the slow checks: %s", response.Body)
	}
	if len(stub.pushValues) != 0 {
		t.Fatalf("choices must not run the preflight: %v", stub.pushValues)
	}
}
