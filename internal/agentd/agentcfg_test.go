package agentd

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/hub"
)

func TestAgentConfigRoundTrip(t *testing.T) {
	home := t.TempDir()
	want := AgentConfig{
		AgentID:      "agent-a",
		Mode:         "listen",
		HubURL:       "https://hub.example",
		ListenAddr:   "192.168.1.5:7761",
		AdvertiseURL: "http://192.168.1.5:7761",
	}
	if err := SaveAgentConfig(home, want); err != nil {
		t.Fatal(err)
	}
	got, ok := LoadAgentConfig(home)
	if !ok {
		t.Fatal("LoadAgentConfig() returned false")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadAgentConfig() = %+v, want %+v", got, want)
	}
	info, err := os.Stat(filepath.Join(home, "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("agent.json mode = %o, want 600", mode)
	}

	if err := os.WriteFile(filepath.Join(home, "agent.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadAgentConfig(home); ok {
		t.Fatal("LoadAgentConfig() returned true for damaged JSON")
	}
	if err := os.WriteFile(filepath.Join(home, "agent.json"), []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadAgentConfig(home); ok {
		t.Fatal("LoadAgentConfig() returned true for null config")
	}
}

func TestResolveConfigMergesPersisted(t *testing.T) {
	home := t.TempDir()
	if err := SaveAgentConfig(home, AgentConfig{
		AgentID:    "saved-agent",
		Mode:       "connect",
		ConnectURL: "http://saved-hub",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveConfig(Config{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != "saved-agent" || got.ConnectURL != "http://saved-hub" {
		t.Fatalf("ResolveConfig() = %+v, want persisted agent ID and URL", got)
	}

	got, err = ResolveConfig(Config{Home: home, ConnectURL: "http://explicit-hub"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ConnectURL != "http://explicit-hub" {
		t.Fatalf("ResolveConfig() ConnectURL = %q, want explicit URL", got.ConnectURL)
	}
	if got.AgentID != "saved-agent" {
		t.Fatalf("ResolveConfig() AgentID = %q, want persisted ID", got.AgentID)
	}
}

func TestResolveConfigNoModeError(t *testing.T) {
	_, err := ResolveConfig(Config{Home: t.TempDir()})
	if err == nil {
		t.Fatal("ResolveConfig() returned nil error for missing mode")
	}
	if !strings.Contains(err.Error(), "homer agent --connect <url>") {
		t.Fatalf("ResolveConfig() error = %q, want bootstrap hint", err)
	}
}

func TestResolveConfigExplicitFieldsWin(t *testing.T) {
	home := t.TempDir()
	if err := SaveAgentConfig(home, AgentConfig{
		AgentID:      "saved-agent",
		Mode:         "listen",
		HubURL:       "http://saved-hub",
		ListenAddr:   "0.0.0.0:7761",
		AdvertiseURL: "http://saved-agent:7761",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveConfig(Config{
		Home:         home,
		AgentID:      "explicit-agent",
		ListenAddr:   "127.0.0.1:7761",
		AdvertiseURL: "http://explicit-agent:7761",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != "explicit-agent" || got.ListenAddr != "127.0.0.1:7761" || got.AdvertiseURL != "http://explicit-agent:7761" {
		t.Fatalf("ResolveConfig() = %+v, explicit fields were not preserved", got)
	}
	if got.HubURL != "http://saved-hub" {
		t.Fatalf("ResolveConfig() HubURL = %q, want persisted completion", got.HubURL)
	}
}

func TestAgentdPersistsAfterRegister(t *testing.T) {
	home := t.TempDir()
	registry := hub.NewRegistry()
	hubServer := httptest.NewServer(hub.NewAgentAPI(registry, "token"))
	defer hubServer.Close()

	cfg := Config{
		Home:         home,
		ConnectURL:   hubServer.URL,
		Token:        "token",
		AgentID:      "persist-agent",
		PollWait:     10 * time.Millisecond,
		PollInterval: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- New(cfg, newRecordingExecutor()).Run(ctx) }()
	waitFor(t, time.Second, func() bool {
		_, ok := registry.Get(cfg.AgentID)
		return ok
	})
	waitFor(t, time.Second, func() bool {
		persisted, ok := LoadAgentConfig(home)
		return ok && persisted.AgentID == cfg.AgentID && persisted.ConnectURL == cfg.ConnectURL
	})

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("connect Run() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connect daemon did not stop")
	}
	body, err := os.ReadFile(filepath.Join(home, "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), cfg.Token) {
		t.Fatalf("agent.json contains token: %s", body)
	}
}

func TestFailedRegisterDoesNotPersist(t *testing.T) {
	home := t.TempDir()
	d := New(Config{
		Home:       home,
		ConnectURL: "http://127.0.0.1:1",
		AgentID:    "failed-agent",
	}, newRecordingExecutor())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.register(ctx, hub.AgentModeConnect, "host"); err == nil {
		t.Fatal("register() returned nil for an unreachable hub")
	}
	if _, ok := LoadAgentConfig(home); ok {
		t.Fatal("failed register persisted agent config")
	}
}

func TestAgentdAdvertiseFallbackConcreteIP(t *testing.T) {
	registry := hub.NewRegistry()
	hubServer := httptest.NewServer(hub.NewAgentAPI(registry, ""))
	defer hubServer.Close()
	d := New(Config{
		Home:       t.TempDir(),
		HubURL:     hubServer.URL,
		ListenAddr: "192.168.1.5:7761",
		AgentID:    "listen-fallback-agent",
	}, newRecordingExecutor())
	if err := d.register(context.Background(), hub.AgentModeListen, "host"); err != nil {
		t.Fatal(err)
	}
	info, ok := registry.Get(d.cfg.AgentID)
	if !ok {
		t.Fatal("agent was not registered")
	}
	if info.Addr != "http://192.168.1.5:7761" {
		t.Fatalf("registered addr = %q, want http://192.168.1.5:7761", info.Addr)
	}
}

func TestAdvertiseFallbackConcreteIP(t *testing.T) {
	got, err := DeriveAdvertiseURL("192.168.1.5:7761")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://192.168.1.5:7761" {
		t.Fatalf("DeriveAdvertiseURL() = %q, want concrete IP URL", got)
	}
}

func TestDeriveAdvertiseURL(t *testing.T) {
	tests := []struct {
		name      string
		listen    string
		want      string
		wantError string
	}{
		{name: "ipv6 wildcard", listen: "[::]:7761", wantError: "--advertise <url>"},
		{name: "invalid", listen: "not-an-address", wantError: "--advertise <url>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DeriveAdvertiseURL(tt.listen)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("DeriveAdvertiseURL() = %q, %v; want error containing %q", got, err, tt.wantError)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("DeriveAdvertiseURL() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestDeriveAdvertiseURLWildcard(t *testing.T) {
	original := discoverLanIPv4
	t.Cleanup(func() { discoverLanIPv4 = original })

	discoverLanIPv4 = func() (net.IP, error) {
		return net.ParseIP("192.168.1.5"), nil
	}
	got, err := DeriveAdvertiseURL("0.0.0.0:7761")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://192.168.1.5:7761" {
		t.Fatalf("DeriveAdvertiseURL() = %q, want wildcard fallback", got)
	}

	discoverLanIPv4 = func() (net.IP, error) {
		return nil, errors.New("本机有多个网卡；请用 --advertise <url>")
	}
	if _, err := DeriveAdvertiseURL("0.0.0.0:7761"); err == nil || !strings.Contains(err.Error(), "--advertise <url>") {
		t.Fatalf("wildcard ambiguity error = %v, want advertise hint", err)
	}
}
