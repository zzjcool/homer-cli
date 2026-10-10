package web

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
)

func TestBuildSyncChoicesConfigDriftCategoriesAndRoot(t *testing.T) {
	baseCategory := core.CategoryConfig{Paths: []string{"settings.json"}, Mode: core.SyncModeMirror}
	cases := []struct {
		name      string
		center    core.AdapterConfig
		machine   commands.ConfigOutlineAdapter
		wantDrift ConfigDrift
	}{
		{
			name: "new category",
			center: core.AdapterConfig{Root: "/tool", Categories: map[string]core.CategoryConfig{
				"settings": baseCategory,
				"files":    {Paths: []string{"files/"}, Mode: core.SyncModeMirror},
			}},
			machine: commands.ConfigOutlineAdapter{ID: "pi", Root: "/tool", Categories: []commands.ConfigOutlineCategory{
				{Name: "settings", Paths: []string{"settings.json"}},
			}},
			wantDrift: ConfigDrift{NewCategories: []string{"files"}},
		},
		{
			name: "extra category",
			center: core.AdapterConfig{Root: "/tool", Categories: map[string]core.CategoryConfig{
				"settings": baseCategory,
			}},
			machine: commands.ConfigOutlineAdapter{ID: "pi", Root: "/tool", Categories: []commands.ConfigOutlineCategory{
				{Name: "settings", Paths: []string{"settings.json"}},
				{Name: "local", Paths: []string{"local/"}},
			}},
			wantDrift: ConfigDrift{ExtraCategories: []string{"local"}},
		},
		{
			name: "category path additions",
			center: core.AdapterConfig{Root: "/tool", Categories: map[string]core.CategoryConfig{
				"settings": {Paths: []string{"settings.json", "mcp.json"}, Mode: core.SyncModeMirror},
			}},
			machine: commands.ConfigOutlineAdapter{ID: "pi", Root: "/tool", Categories: []commands.ConfigOutlineCategory{
				{Name: "settings", Paths: []string{"settings.json"}},
			}},
			wantDrift: ConfigDrift{CategoryAdditions: []string{"settings"}},
		},
		{
			name: "root change",
			center: core.AdapterConfig{Root: "/center/tool", Categories: map[string]core.CategoryConfig{
				"settings": baseCategory,
			}},
			machine: commands.ConfigOutlineAdapter{ID: "pi", Root: "/machine/tool", Categories: []commands.ConfigOutlineCategory{
				{Name: "settings", Paths: []string{"settings.json"}},
			}},
			wantDrift: ConfigDrift{RootChanged: true, MachineRoot: "/machine/tool", CenterRoot: "/center/tool"},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			center := &core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{"pi": test.center}}
			outline := []commands.ConfigOutlineAdapter{test.machine}
			choice := buildConfigDriftChoice(t, center, &outline)
			if choice.ConfigDrift == nil {
				t.Fatal("expected config drift")
			}
			if !reflect.DeepEqual(*choice.ConfigDrift, test.wantDrift) {
				t.Fatalf("config drift = %#v, want %#v", *choice.ConfigDrift, test.wantDrift)
			}
		})
	}
}

func TestBuildSyncChoicesOmitsConfigDriftWithoutDifference(t *testing.T) {
	config := core.AdapterConfig{Root: "/tool", Categories: map[string]core.CategoryConfig{
		"settings": {Paths: []string{"settings.json", "mcp.json"}, Mode: core.SyncModeMirror},
	}}
	center := &core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{"pi": config}}
	outline := []commands.ConfigOutlineAdapter{{
		ID: "pi", Root: "/tool", Categories: []commands.ConfigOutlineCategory{{
			Name: "settings", Paths: []string{"settings.json", "mcp.json"},
		}},
	}}
	choice := buildConfigDriftChoice(t, center, &outline)
	if choice.ConfigDrift != nil {
		t.Fatalf("matching definitions must omit configDrift: %#v", choice.ConfigDrift)
	}
}

func TestBuildSyncChoicesOmitsConfigDriftWithoutGeneration(t *testing.T) {
	center, err := json.Marshal(&core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: "/center", Categories: map[string]core.CategoryConfig{
			"files": {Paths: []string{"files/"}, Mode: core.SyncModeMirror},
		}},
	}})
	if err != nil {
		t.Fatalf("marshal center config: %v", err)
	}
	outline := []commands.ConfigOutlineAdapter{{ID: "pi", Root: "/machine"}}
	choices := []AdapterChoice{{ID: "pi"}}
	attachConfigDrift(choices, &outline, center, 0)
	if choices[0].ConfigDrift != nil {
		t.Fatalf("without a center generation configDrift must be omitted: %#v", choices[0].ConfigDrift)
	}
}

func TestConfigDriftJSONUsesOptionalLowerCamelFields(t *testing.T) {
	data, err := json.Marshal(AdapterChoice{ID: "pi", ConfigDrift: &ConfigDrift{
		NewCategories: []string{"files"},
		RootChanged:   true,
	}})
	if err != nil {
		t.Fatalf("marshal choice: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal choice: %v", err)
	}
	var drift map[string]json.RawMessage
	if err := json.Unmarshal(payload["configDrift"], &drift); err != nil {
		t.Fatalf("unmarshal configDrift: %v", err)
	}
	if string(drift["newCategories"]) != `["files"]` || string(drift["rootChanged"]) != `true` {
		t.Fatalf("configDrift fields = %s", payload["configDrift"])
	}
	for _, key := range []string{"extraCategories", "categoryAdditions", "machineRoot", "centerRoot"} {
		if _, exists := drift[key]; exists {
			t.Errorf("empty configDrift.%s must be omitted: %s", key, payload["configDrift"])
		}
	}

	withoutDrift, err := json.Marshal(AdapterChoice{ID: "pi"})
	if err != nil {
		t.Fatalf("marshal drift-free choice: %v", err)
	}
	if strings.Contains(string(withoutDrift), "configDrift") {
		t.Fatalf("nil ConfigDrift must be omitted: %s", withoutDrift)
	}
}

func TestBuildSyncChoicesOmitsConfigDriftWithoutMachineOutline(t *testing.T) {
	center := &core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: "/center", Categories: map[string]core.CategoryConfig{"files": {Paths: []string{"files/"}, Mode: core.SyncModeMirror}}},
	}}
	choice := buildConfigDriftChoice(t, center, nil)
	if choice.ConfigDrift != nil {
		t.Fatalf("missing machine outline must omit configDrift: %#v", choice.ConfigDrift)
	}
}

func buildConfigDriftChoice(t *testing.T, center *core.HomerConfig, outline *[]commands.ConfigOutlineAdapter) AdapterChoice {
	t.Helper()
	fixture := makeFixtureAtHome(t, t.TempDir(), "base\n")
	centerMeta, err := json.Marshal(center)
	if err != nil {
		t.Fatalf("marshal center config: %v", err)
	}
	if _, err := gens.New(fixture.home).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "center"},
	}, centerMeta); err != nil {
		t.Fatalf("publish center generation: %v", err)
	}
	server := newWebServer(t, fixture, "test-token", nil, nil)
	response, err := server.buildSyncChoicesResponse("dispatch", "box", commands.StatusReport{
		Adapters:      []commands.StatusAdapterReport{{ID: "pi"}},
		ConfigOutline: outline,
	})
	if err != nil {
		t.Fatalf("build choices: %v", err)
	}
	for _, choice := range response.Adapters {
		if choice.ID == "pi" {
			return choice
		}
	}
	t.Fatalf("pi choice missing: %#v", response.Adapters)
	return AdapterChoice{}
}
