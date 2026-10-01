package sync

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

func TestParseAdapterIDs(t *testing.T) {
	tests := []struct {
		name    string
		ids     []string
		want    []string
		wantErr string
	}{
		{name: "nil", ids: nil, wantErr: "请选择至少一个适配器"},
		{name: "empty", ids: []string{}, wantErr: "请选择至少一个适配器"},
		{name: "blanks", ids: []string{"", "  ", ","}, wantErr: "请选择至少一个适配器"},
		{name: "trim and comma", ids: []string{" vscode , pad "}, want: []string{"vscode", "pad"}},
		{name: "duplicates keep first", ids: []string{"pad", "vscode", "pad"}, want: []string{"pad", "vscode"}},
		{name: "repeated values", ids: []string{"vscode", "pad"}, want: []string{"vscode", "pad"}},
		{name: "uppercase", ids: []string{"VSCode"}, wantErr: "适配器 ID 无效: VSCode"},
		{name: "traversal", ids: []string{"../x"}, wantErr: "适配器 ID 无效: ../x"},
		{name: "slash", ids: []string{"pi/settings"}, wantErr: "适配器 ID 无效: pi/settings"},
		{name: "leading digit", ids: []string{"9abc"}, wantErr: "适配器 ID 无效: 9abc"},
		{name: "single letter", ids: []string{"a"}, want: []string{"a"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseAdapterIDs(test.ids)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("ids = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestFilterSnapshotsNilMeansAll(t *testing.T) {
	snapshots := []core.AdapterSnapshot{scopeSnap("pi", "a"), scopeSnap("vscode", "b")}
	got := FilterSnapshots(snapshots, nil)
	if !reflect.DeepEqual(got, snapshots) {
		t.Fatalf("nil filter changed snapshots: %#v", got)
	}
	got = FilterSnapshots(snapshots, []string{"vscode", "missing"})
	if len(got) != 1 || got[0].AdapterID != "vscode" {
		t.Fatalf("filtered = %#v", got)
	}
	if len(FilterSnapshots(snapshots, []string{})) != 0 {
		t.Fatal("empty selection must drop every snapshot")
	}
}

func TestMissingAdapterIDsPreservesOrder(t *testing.T) {
	snapshots := []core.AdapterSnapshot{scopeSnap("pi", "a")}
	got := MissingAdapterIDs(snapshots, []string{"vscode", "pi", "pad"})
	if !reflect.DeepEqual(got, []string{"vscode", "pad"}) {
		t.Fatalf("missing = %#v", got)
	}
	if MissingAdapterIDs(snapshots, []string{"pi"}) != nil {
		t.Fatal("present adapter must not be reported missing")
	}
}

func TestMergeScopedStoreKeepsUntouchedAdapters(t *testing.T) {
	base := map[string]map[string]string{
		"pi":     {"settings/settings.json": "pi-old"},
		"vscode": {"settings/settings.json": "code-old"},
	}
	patch := map[string]map[string]string{
		"vscode": {"settings/settings.json": "code-new"},
		"pad":    {"config/config.json": "should-not-leak"},
	}
	got := MergeScopedStore(base, patch, []string{"vscode"})
	if got["pi"]["settings/settings.json"] != "pi-old" {
		t.Fatalf("pi was touched: %#v", got)
	}
	if got["vscode"]["settings/settings.json"] != "code-new" {
		t.Fatalf("vscode = %#v", got["vscode"])
	}
	if _, ok := got["pad"]; ok {
		t.Fatal("patch adapter outside the scope must not be added")
	}
	base["vscode"]["settings/settings.json"] = "mutated-base"
	if got["vscode"]["settings/settings.json"] != "code-new" {
		t.Fatal("merge result aliases the base map")
	}
	got["pi"]["settings/settings.json"] = "mutated-result"
	if base["pi"]["settings/settings.json"] != "pi-old" {
		t.Fatal("merge result aliases the base file map")
	}
}

func TestMergeScopedStoreRemovesSelectedAdapter(t *testing.T) {
	base := map[string]map[string]string{
		"pi":  {"settings/settings.json": "keep"},
		"pad": {"config/config.json": "gone"},
	}
	got := MergeScopedStore(base, map[string]map[string]string{}, []string{"pad"})
	if _, ok := got["pad"]; ok {
		t.Fatal("selected adapter missing from the patch must be removed")
	}
	if got["pi"]["settings/settings.json"] != "keep" {
		t.Fatalf("pi = %#v", got["pi"])
	}

	emptyPatch := map[string]map[string]string{"pad": {}}
	got = MergeScopedStore(base, emptyPatch, []string{"pad"})
	if _, ok := got["pad"]; ok {
		t.Fatal("selected adapter with no files must be removed")
	}
}

func TestMergeScopedStoreOnEmptyCenter(t *testing.T) {
	got := MergeScopedStore(nil, map[string]map[string]string{
		"vscode": {"settings/settings.json": "code"},
	}, []string{"vscode"})
	if got["vscode"]["settings/settings.json"] != "code" {
		t.Fatalf("store = %#v", got)
	}
	if _, ok := got["pi"]; ok {
		t.Fatal("empty center must not gain adapters outside the scope")
	}
}

func TestDecideScopedPublish(t *testing.T) {
	config := core.HomerConfig{Version: 1}
	base := []core.AdapterSnapshot{scopeSnap("pad", "base"), scopeSnap("vscode", "same")}
	local := []core.AdapterSnapshot{scopeSnap("pad", "base"), scopeSnap("vscode", "same")}
	remote := []core.AdapterSnapshot{scopeSnap("vscode", "same")}

	publish, conflicts := DecideScopedPublish(config, base, local, remote, []string{"pad"})
	if !publish || len(conflicts) != 0 {
		t.Fatalf("missing center adapter: publish=%v conflicts=%v", publish, conflicts)
	}

	publish, conflicts = DecideScopedPublish(config, base, local, remote, []string{"vscode"})
	if publish || len(conflicts) != 0 {
		t.Fatalf("identical adapter: publish=%v conflicts=%v", publish, conflicts)
	}

	localChanged := []core.AdapterSnapshot{scopeSnap("vscode", "local")}
	publish, conflicts = DecideScopedPublish(config, base, localChanged, []core.AdapterSnapshot{scopeSnap("vscode", "same")}, []string{"vscode"})
	if !publish || len(conflicts) != 0 {
		t.Fatalf("local change: publish=%v conflicts=%v", publish, conflicts)
	}

	centerAhead := []core.AdapterSnapshot{scopeSnap("vscode", "center")}
	publish, conflicts = DecideScopedPublish(config, base, local, centerAhead, []string{"vscode"})
	if !publish || len(conflicts) != 0 {
		t.Fatalf("center ahead must still publish on an explicit collect: publish=%v conflicts=%v", publish, conflicts)
	}

	conflictLocal := []core.AdapterSnapshot{scopeSnap("vscode", "local"), scopeSnap("pad", "pad")}
	conflictRemote := []core.AdapterSnapshot{scopeSnap("vscode", "center")}
	publish, conflicts = DecideScopedPublish(config, base, conflictLocal, conflictRemote, []string{"vscode", "pad"})
	if len(conflicts) == 0 || conflicts[0].AdapterID != "vscode" {
		t.Fatalf("conflicts = %#v", conflicts)
	}
	if !publish {
		t.Fatal("a sibling adapter missing from the center is still a publish, but the conflict must be reported")
	}
}

func scopeSnap(id, content string) core.AdapterSnapshot {
	return core.AdapterSnapshot{
		AdapterID: id,
		Categories: []core.CategorySnapshot{{
			AdapterID: id,
			Category:  "settings",
			Mode:      core.SyncModeMirror,
			Files: core.SnapshotFiles{
				"settings.json": {Kind: "file", Content: content},
			},
		}},
	}
}
