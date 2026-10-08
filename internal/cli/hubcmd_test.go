package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPSTalksToTheHubFromHOMERHOST(t *testing.T) {
	var gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("Authorization")
		if r.URL.Path != "/api/agents" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"agents":[{"agentId":"box-1","hostname":"box","version":"v1.2.0","stale":false,"lastSeen":"2026-10-08T00:35:21Z"}]}`))
	}))
	defer server.Close()
	t.Setenv("HOMER_HOST", server.URL)
	t.Setenv("HOMER_HUB_TOKEN", "cli-token")
	var out, errOut bytes.Buffer
	if code := runWithIO([]string{"ps"}, &out, &errOut); code != 0 {
		t.Fatalf("ps exit %d stderr %s", code, errOut.String())
	}
	if gotToken != "Bearer cli-token" {
		t.Fatalf("token = %q", gotToken)
	}
	text := out.String()
	if !strings.Contains(text, "box") || !strings.Contains(text, "在线") || !strings.Contains(text, "v1.2.0") {
		t.Fatalf("ps output = %q", text)
	}
}

// ps lists each machine's programs with their versions and says which ones
// have fallen behind the rest of the fleet.
func TestPSListsApplicationVersions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"agents":[
			{"agentId":"old-1","hostname":"old","version":"v1.2.0","tools":[
				{"id":"pi","label":"pi","version":"0.80.0","latest":"0.90.2","outdated":true},
				{"id":"herdr","label":"herdr","version":"0.9.1","latest":"0.9.1"},
				{"id":"opencode","label":"opencode","error":"读不出版本：输出里没有版本号"}]},
			{"agentId":"new-1","hostname":"new","version":"v1.2.0","tools":[
				{"id":"vscode","version":"1.100.0"}]},
			{"agentId":"bare-1","hostname":"bare","version":"v1.2.0"}]}`))
	}))
	defer server.Close()
	t.Setenv("HOMER_HOST", server.URL)
	t.Setenv("HOMER_HUB_TOKEN", "cli-token")
	var out, errOut bytes.Buffer
	if code := runWithIO([]string{"ps"}, &out, &errOut); code != 0 {
		t.Fatalf("ps exit %d stderr %s", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("ps printed %d lines, want a header and three machines:\n%s", len(lines), out.String())
	}
	if cells := strings.Split(lines[0], "\t"); len(cells) != 5 || cells[0] != "机器" || cells[4] != "应用" {
		t.Fatalf("header = %q", lines[0])
	}
	want := map[string]string{
		"old":  "pi 0.80.0（落后，最新 0.90.2）, herdr 0.9.1, opencode 版本未知",
		"new":  "vscode 1.100.0",
		"bare": "—",
	}
	for _, line := range lines[1:] {
		cells := strings.Split(line, "\t")
		if len(cells) != 5 {
			t.Fatalf("row %q has %d cells, want 5", line, len(cells))
		}
		if cells[4] != want[cells[0]] {
			t.Errorf("%s applications = %q, want %q", cells[0], cells[4], want[cells[0]])
		}
	}
}

func TestStatusUsesLocalAgentIDAgainstTheHub(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "agent.json"), []byte(`{"agentId":"box-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents/box-1/status" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"report":{"adapters":[{"id":"pi","push":1,"pull":2,"conflicts":0}]}}`))
	}))
	defer server.Close()
	t.Setenv("HOMER_HOST", "")
	var out, errOut bytes.Buffer
	if code := runWithIO([]string{"status", "--host", server.URL, "--home", home, "--token", "t"}, &out, &errOut); code != 0 {
		t.Fatalf("status exit %d stderr %s", code, errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, "box-1") || !strings.Contains(text, "1 项未收取") || !strings.Contains(text, "2 项待下发") {
		t.Fatalf("status = %q", text)
	}
}

func TestTokenCreatesThenReuses(t *testing.T) {
	home := t.TempDir()
	var out, errOut bytes.Buffer
	if code := runWithIO([]string{"token", "--home", home}, &out, &errOut); code != 0 {
		t.Fatalf("create exit %d %s", code, errOut.String())
	}
	first := strings.TrimSpace(out.String())
	if len(first) != 64 {
		t.Fatalf("token length = %d", len(first))
	}
	if !strings.Contains(errOut.String(), "已写入") {
		t.Fatalf("stderr = %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := runWithIO([]string{"token", "--home", home}, &out, &errOut); code != 0 {
		t.Fatalf("show exit %d %s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != first {
		t.Fatal("second token changed without --force")
	}
}

func TestJoinPrintsHubCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/join" || r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"command":"curl install | sh -s -- --token once"}`))
	}))
	defer server.Close()
	var out, errOut bytes.Buffer
	if code := runWithIO([]string{"join", "--host", server.URL, "--token", "secret"}, &out, &errOut); code != 0 {
		t.Fatalf("join exit %d %s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "curl install | sh -s -- --token once" {
		t.Fatalf("join = %q", out.String())
	}
}

func TestPullResolveAndUpgradeGoThroughTheHub(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "agent.json"), []byte(`{"agentId":"box-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var pulls, resolves, upgrades int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/agents/box-1/pull":
			if r.URL.Query().Get("confirm") != "true" {
				t.Errorf("pull confirm = %q", r.URL.Query().Get("confirm"))
			}
			pulls++
			_, _ = w.Write([]byte(`{"ok":true,"status":"applied"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/resolve":
			if r.URL.Query().Get("choice") != "local" || r.URL.Query().Get("agent") != "box-1" || r.URL.Query().Get("confirm") != "true" {
				t.Errorf("resolve query = %s", r.URL.RawQuery)
			}
			resolves++
			_, _ = w.Write([]byte(`{"ok":true,"status":"resolved"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/agents/box-1/upgrade":
			upgrades++
			_, _ = w.Write([]byte(`{"ok":true,"status":"upgraded"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	args := []string{"--host", server.URL, "--home", home, "--token", "secret", "--yes"}
	var out, errOut bytes.Buffer
	if code := runWithIO(append([]string{"pull"}, args...), &out, &errOut); code != 0 {
		t.Fatalf("pull exit %d %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "下发") {
		t.Fatalf("pull output = %q", out.String())
	}
	out.Reset()
	if code := runWithIO(append([]string{"resolve", "--accept-local"}, args...), &out, &errOut); code != 0 {
		t.Fatalf("resolve exit %d %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "以这台机器为准") {
		t.Fatalf("resolve output = %q", out.String())
	}
	out.Reset()
	if code := runWithIO([]string{"upgrade", "--host", server.URL, "--token", "secret", "--id", "box-1"}, &out, &errOut); code != 0 {
		t.Fatalf("upgrade exit %d %s", code, errOut.String())
	}
	if pulls != 1 || resolves != 1 || upgrades != 1 {
		t.Fatalf("calls pull=%d resolve=%d upgrade=%d", pulls, resolves, upgrades)
	}
}

func TestTokenForceReplacesThePreviousToken(t *testing.T) {
	home := t.TempDir()
	var out, errOut bytes.Buffer
	if code := runWithIO([]string{"token", "--home", home}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	first := strings.TrimSpace(out.String())
	out.Reset()
	if code := runWithIO([]string{"token", "--home", home, "--force"}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	second := strings.TrimSpace(out.String())
	if second == first || len(second) != 64 {
		t.Fatalf("rotated token = %q", second)
	}
}

func TestPushRefusesWithoutYes(t *testing.T) {
	t.Setenv("HOMER_HOST", "http://127.0.0.1:1")
	var out, errOut bytes.Buffer
	if code := runWithIO([]string{"push"}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "--yes") {
		t.Fatalf("push exit %d stderr %s", code, errOut.String())
	}
}
