package commands

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

func TestRunStatusIncludesConfigOutlineFromLoadedConfig(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return os.Getenv(key)
	})
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"vscode": {
				Root: "~/.config/Code/User",
				Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
				},
			},
			"pi": {
				Root: "~/.pi/agent",
				Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json", "mcp.json"}, Mode: core.SyncModeMirror},
					"agents":   {Paths: []string{"agents/"}, Mode: core.SyncModeMirror},
				},
			},
		},
	}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatalf("save config: %v", err)
	}

	report, err := RunStatus(StatusOptions{HomerHome: home}, DriftSources{})
	if err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if report.ConfigOutline == nil {
		t.Fatal("loaded config must attach configOutline")
	}
	want := []ConfigOutlineAdapter{
		{
			ID:   "pi",
			Root: "~/.pi/agent",
			Categories: []ConfigOutlineCategory{
				{Name: "agents", Paths: []string{"agents/"}},
				{Name: "settings", Paths: []string{"settings.json", "mcp.json"}},
			},
		},
		{
			ID:         "vscode",
			Root:       "~/.config/Code/User",
			Categories: []ConfigOutlineCategory{{Name: "settings", Paths: []string{"settings.json"}}},
		},
	}
	if !reflect.DeepEqual(*report.ConfigOutline, want) {
		t.Fatalf("config outline = %#v\nwant %#v", *report.ConfigOutline, want)
	}

	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var reportFields map[string]json.RawMessage
	if err := json.Unmarshal(data, &reportFields); err != nil {
		t.Fatalf("inspect report JSON: %v", err)
	}
	var outlineFields []map[string]json.RawMessage
	if err := json.Unmarshal(reportFields["configOutline"], &outlineFields); err != nil {
		t.Fatalf("inspect configOutline JSON: %v", err)
	}
	if len(outlineFields) != 2 || string(outlineFields[0]["id"]) != `"pi"` ||
		len(outlineFields[0]) != 3 || string(outlineFields[0]["root"]) != `"~/.pi/agent"` {
		t.Fatalf("configOutline adapter shape is not lowerCamel: %s", reportFields["configOutline"])
	}
	var categoryFields []map[string]json.RawMessage
	if err := json.Unmarshal(outlineFields[0]["categories"], &categoryFields); err != nil {
		t.Fatalf("inspect configOutline categories: %v", err)
	}
	if len(categoryFields) != 2 || string(categoryFields[0]["name"]) != `"agents"` ||
		string(categoryFields[0]["paths"]) != `["agents/"]` || len(categoryFields[0]) != 2 {
		t.Fatalf("configOutline category shape is not lowerCamel: %s", outlineFields[0]["categories"])
	}
	var roundTrip StatusReport
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if !reflect.DeepEqual(roundTrip.ConfigOutline, report.ConfigOutline) {
		t.Fatalf("config outline did not round-trip: got %#v, want %#v", roundTrip.ConfigOutline, report.ConfigOutline)
	}
}
