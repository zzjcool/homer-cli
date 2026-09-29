package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

// No-git data plane: pull with an injected hub snapshot applies the hub's
// current content without any git upstream. The former isGitRepo → fetch →
// ff-only precondition chain is gone when the snapshot is provided.
func TestRunPullAppliesHubSnapshot(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(string) string { return home })
	tool := filepath.Join(home, "tool")
	if err := os.MkdirAll(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: tool, Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
		}},
	}}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	store := core.AdapterSnapshot{AdapterID: "pi", Categories: []core.CategorySnapshot{
		{AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{
			"settings.json": core.SnapshotEntry{Kind: "file", Content: "base\n"},
		}},
	}}
	if err := core.WriteSnapshotToStore(paths, store); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool, "settings.json"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hubSnapshot := []core.AdapterSnapshot{{AdapterID: "pi", Categories: []core.CategorySnapshot{
		{AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{
			"settings.json": core.SnapshotEntry{Kind: "file", Content: "from-hub\n"},
		}},
	}}}

	report := RunPull(PullOptions{HomerHome: home, Yes: true}, &PullDeps{
		UI:          HeadlessUI{},
		NoFetch:     true,
		HubSnapshot: hubSnapshot,
	})
	if !report.OK || report.Status != PullStatusApplied {
		t.Fatalf("pull report = %#v errors=%v warnings=%v", report, report.Errors, report.Warnings)
	}
	applied, err := os.ReadFile(filepath.Join(tool, "settings.json"))
	if err != nil || string(applied) != "from-hub\n" {
		t.Fatalf("tool file = %q err=%v (want hub content)", applied, err)
	}
}

// No-git data plane: push with an injected snapshot sink uploads the
// prepared snapshot instead of committing to git. The sink receives what
// would have been the git commit content: every adapter's store snapshot.
func TestRunPushUploadsHubSnapshot(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(string) string { return home })
	tool := filepath.Join(home, "tool")
	if err := os.MkdirAll(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: tool, Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
		}},
	}}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	store := core.AdapterSnapshot{AdapterID: "pi", Categories: []core.CategorySnapshot{
		{AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{
			"settings.json": core.SnapshotEntry{Kind: "file", Content: "base\n"},
		}},
	}}
	if err := core.WriteSnapshotToStore(paths, store); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool, "settings.json"), []byte("local-change\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var uploaded []core.AdapterSnapshot
	report := RunPush(PushOptions{HomerHome: home, Yes: true}, &PushDeps{
		UI: HeadlessUI{},
		HubSink: func(snapshot []core.AdapterSnapshot) (int, error) {
			uploaded = snapshot
			return 1, nil
		},
	})
	if !report.OK || report.Status != PushStatusPushed {
		t.Fatalf("push report = %#v errors=%v warnings=%v", report, report.Errors, report.Warnings)
	}
	if len(uploaded) != 1 || uploaded[0].Categories[0].Files["settings.json"].Content != "local-change\n" {
		t.Fatalf("uploaded = %#v (want the local change)", uploaded)
	}
	// The machine store is also updated (baseline for the next three-way).
	applied, err := os.ReadFile(filepath.Join(paths.StoreDir, "pi", "settings", "settings.json"))
	if err != nil || string(applied) != "local-change\n" {
		t.Fatalf("store = %q err=%v", applied, err)
	}
}
