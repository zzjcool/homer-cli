package sync

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/manifest"
)

func TestSplitManifestActionsGoldenVectors(t *testing.T) {
	manifestKind := core.CategoryKindManifest
	config := core.HomerConfig{Adapters: map[string]core.AdapterConfig{
		"vscode": {Categories: map[string]core.CategoryConfig{
			"extensions": {
				Kind:     &manifestKind,
				Mode:     core.SyncModeMirror,
				ListCmd:  "code --list-extensions",
				ApplyCmd: "code --install-extension",
			},
		}},
	}}
	plan := PullPlan{Actions: []PullAction{
		{Type: PullActionWrite, AdapterID: "vscode", Category: "extensions", RelPath: "extensions.manifest.txt", Content: "pub.z\npub.a\npub.z\n"},
		{Type: PullActionConflict, AdapterID: "vscode", Category: "extensions", RelPath: "extensions.manifest.txt", LocalContent: "pub.local\n", RemoteContent: "pub.c\npub.b\npub.c\npub.local\n"},
		{Type: PullActionDelete, AdapterID: "vscode", Category: "extensions", RelPath: "extensions.manifest.txt"},
		{Type: PullActionWrite, AdapterID: "vscode", Category: "settings", RelPath: "settings.json", Content: "remote"},
		{Type: PullActionWrite, AdapterID: "other", Category: "extensions", RelPath: "extensions.manifest.txt", Content: "other"},
	}}
	local := []core.AdapterSnapshot{{AdapterID: "vscode", Categories: []core.CategorySnapshot{{
		AdapterID: "vscode", Category: "extensions", Mode: core.SyncModeMirror,
		Files: core.SnapshotFiles{manifest.VirtualFileName("extensions"): {Kind: "file", Content: "pub.local\npub.z\n"}},
	}}}}

	remaining, tasks, warnings := SplitManifestActions(config, plan, local)
	wantRemaining := []PullAction{
		{Type: PullActionWrite, AdapterID: "vscode", Category: "settings", RelPath: "settings.json", Content: "remote"},
		{Type: PullActionWrite, AdapterID: "other", Category: "extensions", RelPath: "extensions.manifest.txt", Content: "other"},
	}
	if !reflect.DeepEqual(remaining.Actions, wantRemaining) {
		t.Fatalf("remaining actions = %#v, want %#v", remaining.Actions, wantRemaining)
	}
	wantTasks := []manifest.Task{
		{AdapterID: "vscode", Category: "extensions", IDs: []string{"pub.a"}, ListCmd: "code --list-extensions", ApplyCmd: "code --install-extension"},
		{AdapterID: "vscode", Category: "extensions", IDs: []string{"pub.b", "pub.c"}, ListCmd: "code --list-extensions", ApplyCmd: "code --install-extension"},
	}
	if !reflect.DeepEqual(tasks, wantTasks) {
		t.Fatalf("tasks = %#v, want %#v", tasks, wantTasks)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "不执行卸载") || !strings.Contains(warnings[0], "vscode/extensions") {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestSplitManifestActionsUsesEmptyLocalAndKeepsNonManifestDeletes(t *testing.T) {
	kind := core.CategoryKindManifest
	config := core.HomerConfig{Adapters: map[string]core.AdapterConfig{
		"a": {Categories: map[string]core.CategoryConfig{
			"m":     {Kind: &kind, Mode: core.SyncModeMirror, ApplyCmd: "install", ListCmd: "list"},
			"files": {Paths: []string{"a.txt"}, Mode: core.SyncModeMirror},
		}},
	}}
	plan := PullPlan{Actions: []PullAction{
		{Type: PullActionWrite, AdapterID: "a", Category: "m", RelPath: "m.manifest.txt", Content: "b\na\na\n"},
		{Type: PullActionDelete, AdapterID: "a", Category: "files", RelPath: "a.txt"},
	}}
	remaining, tasks, warnings := SplitManifestActions(config, plan, nil)
	if len(tasks) != 1 || !reflect.DeepEqual(tasks[0].IDs, []string{"a", "b"}) {
		t.Fatalf("empty-local task = %#v", tasks)
	}
	if len(remaining.Actions) != 1 || remaining.Actions[0].Type != PullActionDelete {
		t.Fatalf("non-manifest action was not passed through = %#v", remaining.Actions)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings = %#v", warnings)
	}
}

func TestBuildManifestPreviewAndCap(t *testing.T) {
	task := manifest.Task{AdapterID: "vscode", Category: "extensions", IDs: []string{"pub.two", "pub.one", "pub.two"}}
	want := "  安装扩展 2 个（vscode/extensions）\n    + pub.one\n    + pub.two"
	if got := BuildManifestPreview([]manifest.Task{task}); got != want {
		t.Fatalf("preview = %q, want %q", got, want)
	}

	ids := make([]string, 25)
	for index := range ids {
		ids[index] = "pub." + string(rune('a'+index))
	}
	preview := BuildManifestPreview([]manifest.Task{{AdapterID: "a", Category: "c", IDs: ids}})
	lines := strings.Split(preview, "\n")
	if len(lines) != 21 || !strings.HasSuffix(preview, "… 其余 6 条省略") {
		t.Fatalf("capped preview has %d lines: %q", len(lines), preview)
	}
}
