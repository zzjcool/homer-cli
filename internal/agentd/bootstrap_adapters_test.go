package agentd

import (
	"os"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter/keys"
	"github.com/zzjcool/homer-cli/internal/core"
)

func bootstrapPaths(t *testing.T, home string) core.HomerPaths {
	t.Helper()
	return core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return os.Getenv(key)
	})
}

// A machine configured before the center grew a keyring adapter must pick it
// up on the next dispatch; otherwise it answers "没有适配器 keyring" forever.
func TestBootstrapAddsAdaptersTheCenterHasAndTheMachineLacks(t *testing.T) {
	home := t.TempDir()
	paths := bootstrapPaths(t, home)
	piRoot := "~/custom/pi"
	local := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: piRoot, Categories: map[string]core.CategoryConfig{"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror}}},
	}}
	if err := core.SaveConfig(paths, local); err != nil {
		t.Fatal(err)
	}
	center := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi":           {Root: "~/.pi/agent", Categories: map[string]core.CategoryConfig{"settings": {Paths: []string{"other.json"}, Mode: core.SyncModeMirror}}},
		keys.AdapterID: keys.DefaultAdapter,
	}}
	meta := centerMeta(t, home, center)

	exec := &localExecutor{homerHome: home}
	if err := exec.bootstrapFromGeneration(nil, meta); err != nil {
		t.Fatal(err)
	}
	got, err := core.LoadConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Adapters[keys.AdapterID]; !ok {
		t.Fatalf("keyring adapter not added: %v", got.Adapters)
	}
	if got.Adapters["pi"].Root != piRoot || got.Adapters["pi"].Categories["settings"].Paths[0] != "settings.json" {
		t.Fatalf("the machine's own adapter must be left as the user set it: %+v", got.Adapters["pi"])
	}
}

func TestBootstrapKeepsConfigWhenNothingIsMissing(t *testing.T) {
	home := t.TempDir()
	paths := bootstrapPaths(t, home)
	local := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{keys.AdapterID: keys.DefaultAdapter}}
	if err := core.SaveConfig(paths, local); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	exec := &localExecutor{homerHome: home}
	if err := exec.bootstrapFromGeneration(nil, centerMeta(t, home, local)); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(paths.ConfigFile)
	if string(before) != string(after) {
		t.Fatal("config must not be rewritten when the machine already has every adapter")
	}
	// An unreadable center config must never block a pull.
	if err := exec.bootstrapFromGeneration(nil, []byte("not json")); err != nil {
		t.Fatalf("bad center meta = %v", err)
	}
}

// centerMeta serializes a config the way the hub stores homer.json.
func centerMeta(t *testing.T, home string, config core.HomerConfig) []byte {
	t.Helper()
	other := bootstrapPaths(t, t.TempDir())
	if err := core.SaveConfig(other, config); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(other.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
