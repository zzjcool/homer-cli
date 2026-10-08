package hubclient

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEndpointDefaultsAndDockerStyleTCP(t *testing.T) {
	t.Setenv("HOMER_HOST", "")
	if got := Endpoint(""); got != "http://127.0.0.1:7760" {
		t.Fatalf("default = %q", got)
	}
	t.Setenv("HOMER_HOST", "tcp://10.0.0.8:7760")
	if got := Endpoint(""); got != "http://10.0.0.8:7760" {
		t.Fatalf("env = %q", got)
	}
	if got := Endpoint("https://hub.example"); got != "https://hub.example" {
		t.Fatalf("flag = %q", got)
	}
	if got := Endpoint("hub.example:7760"); got != "http://hub.example:7760" {
		t.Fatalf("bare = %q", got)
	}
}

func TestTokenFileThenEnv(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "keys", "hub-token"), []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOMER_HUB_TOKEN", "")
	if got := Token("", home); got != "from-file" {
		t.Fatalf("file token = %q", got)
	}
	t.Setenv("HOMER_HUB_TOKEN", "from-env")
	if got := Token("", home); got != "from-env" {
		t.Fatalf("env token = %q", got)
	}
	if got := Token("from-flag", home); got != "from-flag" {
		t.Fatalf("flag token = %q", got)
	}
}

func TestDoSendsBearerAndReportsHubDown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"agents":[]}`))
	}))
	defer server.Close()
	client := New(server.URL, "secret")
	body, code, err := client.Do(http.MethodGet, "/api/agents", nil, 0)
	if err != nil || code != 200 || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("do = %d %v %s", code, err, body)
	}
	down := New("http://127.0.0.1:1", "secret")
	if _, _, err := down.Do(http.MethodGet, "/api/agents", nil, 200*time.Millisecond); err == nil || !strings.Contains(err.Error(), "无法连接中心") {
		t.Fatalf("down = %v", err)
	}
}
