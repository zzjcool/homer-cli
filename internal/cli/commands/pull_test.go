package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
)

func TestRunPull_PreferRemoteAdapters_OnlyListedRewritten(t *testing.T) {
	paths, roots, sources := preferRemoteRunPullFixture(t)
	report := RunPull(PullOptions{
		HomerHome:            paths.Home,
		Yes:                  true,
		PreferRemoteAdapters: []string{"pad"},
	}, &PullDeps{
		UI:          HeadlessUI{},
		Sources:     sources,
		HubSnapshot: sources.Remote,
	})
	if report.OK || report.Status != PullStatusConflictsRemain {
		t.Fatalf("pull = %#v, want conflicts-remain for the unselected adapter", report)
	}
	if len(report.Conflicts) != 1 || report.Conflicts[0].AdapterID != "pi" {
		t.Fatalf("conflicts = %#v, want only the unselected pi conflict", report.Conflicts)
	}
	if len(report.Applied.Written) != 1 || report.Applied.Written[0].AdapterID != "pad" {
		t.Fatalf("written = %#v, want only the selected pad adapter", report.Applied.Written)
	}
	if got := mustReadScope(t, filepath.Join(roots["pad"], "settings.json")); got != "pad-center" {
		t.Fatalf("pad = %q, want center content", got)
	}
	if got := mustReadScope(t, filepath.Join(roots["pi"], "settings.json")); got != "pi-local" {
		t.Fatalf("pi = %q, want unchanged local content", got)
	}
}

func TestRunPull_PreferRemoteBeatsAdapters(t *testing.T) {
	paths, roots, sources := preferRemoteRunPullFixture(t)
	report := RunPull(PullOptions{
		HomerHome:            paths.Home,
		Yes:                  true,
		PreferRemote:         true,
		PreferRemoteAdapters: []string{"pad"},
	}, &PullDeps{
		UI:          HeadlessUI{},
		Sources:     sources,
		HubSnapshot: sources.Remote,
	})
	if !report.OK || report.Status != PullStatusApplied || len(report.Conflicts) != 0 {
		t.Fatalf("pull = %#v, want PreferRemote to resolve all conflicts", report)
	}
	for adapterID, want := range map[string]string{"pad": "pad-center", "pi": "pi-center"} {
		if got := mustReadScope(t, filepath.Join(roots[adapterID], "settings.json")); got != want {
			t.Fatalf("%s = %q, want center content %q", adapterID, got, want)
		}
	}
}

func TestRunPull_NoOptionUnchanged(t *testing.T) {
	paths, roots, sources := preferRemoteRunPullFixture(t)
	report := RunPull(PullOptions{HomerHome: paths.Home, Yes: true}, &PullDeps{
		UI:          HeadlessUI{},
		Sources:     sources,
		HubSnapshot: sources.Remote,
	})
	if report.OK || report.Status != PullStatusConflictsRemain || len(report.Conflicts) != 2 {
		t.Fatalf("pull = %#v, want both conflicts unchanged without either option", report)
	}
	for adapterID, want := range map[string]string{"pad": "pad-local", "pi": "pi-local"} {
		if got := mustReadScope(t, filepath.Join(roots[adapterID], "settings.json")); got != want {
			t.Fatalf("%s = %q, want unchanged local content %q", adapterID, got, want)
		}
	}
}

func TestNewPullReport_NonNilSlices(t *testing.T) {
	report := NewPullReport(PullStatusNoDrift)
	if report.Conflicts == nil || report.Resolutions == nil || report.Warnings == nil || report.Errors == nil {
		t.Fatalf("report slices must be non-nil: %#v", report)
	}
	if report.Applied.Written == nil || report.Applied.Deleted == nil || report.Applied.Conflicts == nil {
		t.Fatalf("applied result slices must be non-nil: %#v", report.Applied)
	}
}

func preferRemoteRunPullFixture(t *testing.T) (core.HomerPaths, map[string]string, syncx.SyncSources) {
	t.Helper()
	paths, roots := writeScopeTools(t)
	contents := []struct {
		adapterID string
		base      string
		local     string
		remote    string
	}{
		{adapterID: "pad", base: "old", local: "pad-local", remote: "pad-center"},
		{adapterID: "pi", base: "keep", local: "pi-local", remote: "pi-center"},
	}
	sources := syncx.SyncSources{}
	for _, item := range contents {
		if err := os.WriteFile(filepath.Join(roots[item.adapterID], "settings.json"), []byte(item.local), 0o644); err != nil {
			t.Fatal(err)
		}
		sources.Base = append(sources.Base, scopeAdapterSnap(item.adapterID, item.base))
		sources.Local = append(sources.Local, scopeAdapterSnap(item.adapterID, item.local))
		sources.Remote = append(sources.Remote, scopeAdapterSnap(item.adapterID, item.remote))
	}
	return paths, roots, sources
}

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
