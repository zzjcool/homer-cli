package commands

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/pluginregistry"
)

func TestInitAdapterRegistrationUsesAdapterPluginsOnly(t *testing.T) {
	var wantOrder []string
	for _, plugin := range pluginregistry.Builtins() {
		if plugin.Role != pluginregistry.RoleAdapter {
			continue
		}
		if plugin.Adapter == nil {
			t.Fatalf("adapter plugin %q has no Adapter config", plugin.ID)
		}
		wantOrder = append(wantOrder, plugin.ID)
		if got, ok := KNOWN_ADAPTERS[plugin.ID]; !ok || !reflect.DeepEqual(got, *plugin.Adapter) {
			t.Errorf("KNOWN_ADAPTERS[%q] = (%#v, %v), want %#v", plugin.ID, got, ok, *plugin.Adapter)
		}
	}
	if !reflect.DeepEqual(knownAdapterOrder, wantOrder) {
		t.Fatalf("knownAdapterOrder = %#v, want plugin registry order %#v", knownAdapterOrder, wantOrder)
	}
	if len(KNOWN_ADAPTERS) != len(wantOrder) {
		t.Fatalf("KNOWN_ADAPTERS contains %d entries, want %d adapter plugins", len(KNOWN_ADAPTERS), len(wantOrder))
	}
	for _, plugin := range pluginregistry.Builtins() {
		_, found := KNOWN_ADAPTERS[plugin.ID]
		if want := plugin.Role == pluginregistry.RoleAdapter; found != want {
			t.Errorf("KNOWN_ADAPTERS contains %q = %v, role=%q", plugin.ID, found, plugin.Role)
		}
	}
}

func TestUnknownInitAdapterListsDynamicPluginRegistryAdapters(t *testing.T) {
	var adapterIDs []string
	for _, plugin := range pluginregistry.Builtins() {
		if plugin.Role == pluginregistry.RoleAdapter {
			adapterIDs = append(adapterIDs, plugin.ID)
		}
	}
	_, err := selectedAdapters([]string{"missing"})
	if err == nil {
		t.Fatal("selectedAdapters accepted an unknown plugin id")
	}
	want := "未知 adapter: missing；可用: " + strings.Join(adapterIDs, ", ") + "；自定义 adapter 请直接编辑 homer.json 或在 hub 插件页安装"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want %q", err, want)
	}
}
