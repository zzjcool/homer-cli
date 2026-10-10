package pluginruntime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/pluginregistry"
)

func TestStatePersistsOfficialAndCustomPluginsInInstallOrder(t *testing.T) {
	home := t.TempDir()
	state := New(home)
	if got := state.List(); len(got) != 0 {
		t.Fatalf("new state List() = %#v, want empty", got)
	}

	for _, plugin := range []pluginregistry.Plugin{builtin(t, "herdr"), customPlugin("custom-editor"), builtin(t, "pi")} {
		if err := state.Install(plugin); err != nil {
			t.Fatalf("Install(%q): %v", plugin.ID, err)
		}
	}
	wantIDs := []string{"herdr", "custom-editor", "pi"}
	assertPluginIDs(t, state.List(), wantIDs)

	path := filepath.Join(home, "plugins.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		SchemaVersion int                     `json:"schemaVersion"`
		Installed     []string                `json:"installed"`
		Custom        []pluginregistry.Plugin `json:"custom"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("decode plugins.json: %v", err)
	}
	if saved.SchemaVersion != 1 || !reflect.DeepEqual(saved.Installed, wantIDs) || len(saved.Custom) != 1 || saved.Custom[0].ID != "custom-editor" {
		t.Fatalf("persisted state = %+v", saved)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("plugins.json mode = %o, want 600", info.Mode().Perm())
	}

	reloaded := New(home)
	assertPluginIDs(t, reloaded.List(), wantIDs)
	if got := reloaded.List()[1].Adapter.Categories["files"].Paths; !reflect.DeepEqual(got, []string{"settings.json"}) {
		t.Fatalf("restored custom adapter paths = %#v", got)
	}

	// List returns detached plugin values; mutating one cannot rewrite state.
	listed := reloaded.List()
	listed[1].Adapter.Categories["files"] = core.CategoryConfig{}
	if got := reloaded.List()[1].Adapter.Categories["files"].Paths; !reflect.DeepEqual(got, []string{"settings.json"}) {
		t.Fatalf("mutating List() changed stored custom plugin: %#v", got)
	}
}

func TestStateUninstallPersistsAndRejectsDuplicates(t *testing.T) {
	home := t.TempDir()
	state := New(home)
	if err := state.Install(builtin(t, "ssh-key")); err != nil {
		t.Fatal(err)
	}
	if err := state.Install(builtin(t, "ssh-key")); !errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("duplicate Install error = %v, want ErrAlreadyInstalled", err)
	}
	if !state.IsInstalled("ssh-key") {
		t.Fatal("ssh-key is not reported installed")
	}
	if err := state.Uninstall("ssh-key"); err != nil {
		t.Fatal(err)
	}
	if state.IsInstalled("ssh-key") {
		t.Fatal("ssh-key is still installed after Uninstall")
	}
	if err := state.Uninstall("ssh-key"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("missing Uninstall error = %v, want ErrNotInstalled", err)
	}
	if got := New(home).List(); len(got) != 0 {
		t.Fatalf("reloaded List() = %#v after uninstall", got)
	}
}

func TestStateValidatesCustomAdapterManifest(t *testing.T) {
	state := New(t.TempDir())
	officialCollision := customPlugin("pi")
	if err := state.Install(officialCollision); !errors.Is(err, ErrInvalidPlugin) {
		t.Fatalf("official ID collision error = %v, want ErrInvalidPlugin", err)
	}
	invalidID := customPlugin("Invalid_Id")
	if err := state.Install(invalidID); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("invalid ID error = %v, want ErrInvalidID", err)
	}
	invalidRole := customPlugin("custom-role")
	invalidRole.Role = pluginregistry.RoleCarrier
	if err := state.Install(invalidRole); !errors.Is(err, ErrInvalidPlugin) {
		t.Fatalf("invalid role error = %v, want ErrInvalidPlugin", err)
	}
	invalidCategories := customPlugin("custom-categories")
	category := invalidCategories.Adapter.Categories["files"]
	category.Mode = core.SyncMode("replace")
	invalidCategories.Adapter.Categories["files"] = category
	if err := state.Install(invalidCategories); !errors.Is(err, ErrInvalidPlugin) {
		t.Fatalf("invalid categories error = %v, want ErrInvalidPlugin", err)
	}
	if got := state.List(); len(got) != 0 {
		t.Fatalf("invalid plugin changed state: %#v", got)
	}
}

func TestStateConcurrentInstallIsSerialized(t *testing.T) {
	state := New(t.TempDir())
	plugin := builtin(t, "pi")
	const callers = 12
	var wg sync.WaitGroup
	results := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- state.Install(plugin)
		}()
	}
	wg.Wait()
	close(results)
	installed, duplicate := 0, 0
	for err := range results {
		switch {
		case err == nil:
			installed++
		case errors.Is(err, ErrAlreadyInstalled):
			duplicate++
		default:
			t.Fatalf("concurrent Install error = %v", err)
		}
	}
	if installed != 1 || duplicate != callers-1 {
		t.Fatalf("successes=%d duplicates=%d, want 1 and %d", installed, duplicate, callers-1)
	}
	assertPluginIDs(t, New(statePathHome(state)).List(), []string{"pi"})
}

func builtin(t *testing.T, id string) pluginregistry.Plugin {
	t.Helper()
	plugin, ok := pluginregistry.Builtin(id)
	if !ok {
		t.Fatalf("Builtin(%q) not found", id)
	}
	return plugin
}

func customPlugin(id string) pluginregistry.Plugin {
	return pluginregistry.Plugin{
		ID:   id,
		Role: pluginregistry.RoleAdapter,
		Name: "Custom editor",
		Desc: "A test custom adapter",
		Adapter: &core.AdapterConfig{
			Root: "~/.custom-editor",
			Categories: map[string]core.CategoryConfig{
				"files": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
			},
		},
	}
}

func assertPluginIDs(t *testing.T, plugins []pluginregistry.Plugin, want []string) {
	t.Helper()
	got := make([]string, len(plugins))
	for i, plugin := range plugins {
		got[i] = plugin.ID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plugin IDs = %#v, want %#v", got, want)
	}
}

func statePathHome(state *State) string {
	return filepath.Dir(state.path)
}
