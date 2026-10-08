package agentd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAgentCfgRoundTrip(t *testing.T) {
	home := t.TempDir()
	want := AgentConfig{
		AgentID:     "agent-a",
		HubURL:      "https://hub.example",
		DataURL:     "http://hub-internal.example",
		AgentSecret: "secret-a",
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
	data, err := os.ReadFile(filepath.Join(home, "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agentId", "hubUrl", "dataUrl", "agentSecret"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("agent.json is missing %q: %s", name, data)
		}
	}
	for _, old := range []string{"mode", "connectUrl", "listenAddr", "advertiseUrl"} {
		if _, ok := fields[old]; ok {
			t.Errorf("agent.json contains removed field %q: %s", old, data)
		}
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

func TestAgentCfgResolveConfig(t *testing.T) {
	home := t.TempDir()
	if err := SaveAgentConfig(home, AgentConfig{
		AgentID:     "saved-agent",
		HubURL:      "https://saved-hub.example",
		DataURL:     "http://saved-data.example",
		AgentSecret: "saved-secret",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveConfig(Config{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != "saved-agent" || got.HubURL != "https://saved-hub.example" ||
		got.DataURL != "http://saved-data.example" || got.AgentSecret != "saved-secret" {
		t.Fatalf("ResolveConfig() = %+v, want persisted connection state", got)
	}

	got, err = ResolveConfig(Config{
		Home:        home,
		AgentID:     "explicit-agent",
		HubURL:      "https://explicit-hub.example",
		DataURL:     "http://explicit-data.example",
		AgentSecret: "explicit-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != "explicit-agent" || got.HubURL != "https://explicit-hub.example" ||
		got.DataURL != "http://explicit-data.example" || got.AgentSecret != "explicit-secret" {
		t.Fatalf("explicit config did not win: %+v", got)
	}
}

func TestAgentCfgIgnoresLegacyFieldsButDoesNotUseThem(t *testing.T) {
	home := t.TempDir()
	legacy := `{"agentId":"legacy-agent","mode":"connect","connectUrl":"https://old-hub.example","listenAddr":"0.0.0.0:7761","advertiseUrl":"http://agent.example:7761"}`
	if err := os.WriteFile(filepath.Join(home, "agent.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadAgentConfig(home); !ok {
		t.Fatal("legacy unknown fields must not make agent.json unreadable")
	}
	if _, err := ResolveConfig(Config{Home: home}); err == nil || !strings.Contains(err.Error(), "--hub") {
		t.Fatalf("ResolveConfig() error = %v, want an explicit --hub hint", err)
	}
}

func TestAgentCfgExplicitTokenOverridesButPreservesSavedSecret(t *testing.T) {
	home := t.TempDir()
	if err := SaveAgentConfig(home, AgentConfig{AgentID: "saved", HubURL: "https://saved.example", AgentSecret: "saved-secret"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveConfig(Config{Home: home, AgentSecret: "saved-secret", Token: "explicit-token"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.AgentSecret != "" || resolved.Token != "explicit-token" {
		t.Fatalf("ResolveConfig with explicit token = %+v, want explicit token without secret override", resolved)
	}
	d := New(resolved, nil)
	if got := d.bearerCredential(); got != "explicit-token" {
		t.Fatalf("explicit token credential = %q", got)
	}
	if err := d.saveAgentConfig(); err != nil {
		t.Fatal(err)
	}
	persisted, ok := LoadAgentConfig(home)
	if !ok || persisted.AgentSecret != "saved-secret" {
		t.Fatalf("agent.json secret after explicit-token run = %+v loaded=%v", persisted, ok)
	}
}

func TestAgentCfgMissingHubURL(t *testing.T) {
	_, err := ResolveConfig(Config{Home: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "hub") || !strings.Contains(err.Error(), "--hub") {
		t.Fatalf("ResolveConfig() error = %v, want a clear missing HubURL error", err)
	}
	if got, err := ResolveConfig(Config{Home: t.TempDir(), HubURL: "https://explicit.example"}); err != nil || got.HubURL != "https://explicit.example" {
		t.Fatalf("ResolveConfig(explicit HubURL) = %+v, %v", got, err)
	}
}
