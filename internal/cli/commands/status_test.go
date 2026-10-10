package commands

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/engine"
)

// The console's machine view must show WHICH files (e.g. which
// extensions), not just counts — StatusCategoryReport carries the file
// list with per-file status.
func TestStatusReportListsFiles(t *testing.T) {
	base := core.SnapshotFiles{}
	local := core.SnapshotFiles{
		"a.json": {Content: "{}"},
		"b.json": {Content: "{}"},
	}
	remote := core.SnapshotFiles{}
	drifts := engine.ComputeDrift([]core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{
			{Category: "extensions", Mode: core.SyncModeMirror, Files: base},
		}},
	}, []core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{
			{Category: "extensions", Mode: core.SyncModeMirror, Files: local},
		}},
	}, []core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{
			{Category: "extensions", Mode: core.SyncModeMirror, Files: remote},
		}},
	})
	report := BuildStatusReport(drifts)
	if len(report.Adapters) != 1 {
		t.Fatalf("adapters = %d", len(report.Adapters))
	}
	cats := report.Adapters[0].Categories
	if len(cats) != 1 || cats[0].Name != "extensions" {
		t.Fatalf("categories = %+v", cats)
	}
	files := cats[0].Files
	if len(files) != 2 {
		t.Fatalf("files = %+v (want a.json + b.json)", files)
	}
	paths := []string{files[0].Path, files[1].Path}
	sort.Strings(paths)
	if paths[0] != "a.json" || paths[1] != "b.json" {
		t.Fatalf("file paths = %v", paths)
	}
	if files[0].Status == "" || files[1].Status == "" {
		t.Fatalf("file statuses empty: %+v", files)
	}
}

// Manifest categories (pi/packages, vscode/extensions) list PACKAGE
// NAMES, never the virtual manifest file — see applyManifestView.
// manifest file: "which extensions are installed" is one line per
// package — the virtual file is an implementation detail.
func TestStatusReportManifestListsPackages(t *testing.T) {
	base := core.SnapshotFiles{}
	local := core.SnapshotFiles{
		"packages.manifest.txt": {Content: "npm:pi-lens\nnpm:pi-sop\n"},
	}
	remote := core.SnapshotFiles{}
	drifts := engine.ComputeDrift([]core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{{Category: "packages", Mode: core.SyncModeMirror, Files: base}}},
	}, []core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{{Category: "packages", Mode: core.SyncModeMirror, Files: local}}},
	}, []core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{{Category: "packages", Mode: core.SyncModeMirror, Files: remote}}},
	})
	report := BuildStatusReport(drifts)
	cats := report.Adapters[0].Categories
	if len(cats) != 1 || cats[0].Name != "packages" {
		t.Fatalf("categories = %+v", cats)
	}
	files := cats[0].Files
	if len(files) != 1 || files[0].Path != "packages.manifest.txt" {
		t.Fatalf("pre-expand files = %+v (raw virtual file — expected only before manifest expansion)", files)
	}
}

// Merge pulls are per JSON key. The count is useless in the console
// unless those keys are named in the file list.
func TestStatusReportListsMergePullKeys(t *testing.T) {
	entry := func(content string) core.SnapshotEntry {
		return core.SnapshotEntry{Kind: "json", Content: content}
	}
	base := core.SnapshotFiles{"settings.json": entry(`{"a":1,"b":2}`)}
	local := core.SnapshotFiles{"settings.json": entry(`{"a":1,"b":2}`)}
	remote := core.SnapshotFiles{"settings.json": entry(`{"a":1,"b":4,"c":5}`)}
	drifts := engine.ComputeDrift([]core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{{Category: "settings", Mode: core.SyncModeMerge, Files: base}}},
	}, []core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{{Category: "settings", Mode: core.SyncModeMerge, Files: local}}},
	}, []core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{{Category: "settings", Mode: core.SyncModeMerge, Files: remote}}},
	})
	report := BuildStatusReport(drifts)
	if len(report.Adapters) != 1 || report.Adapters[0].Pull != 2 {
		t.Fatalf("report = %+v", report.Adapters)
	}
	got := map[string]string{}
	for _, file := range report.Adapters[0].Categories[0].Files {
		got[file.Path] = file.Status
	}
	if got["settings.json:b"] != "pull" || got["settings.json:c"] != "pull" || len(got) != 2 {
		t.Fatalf("files = %+v", report.Adapters[0].Categories[0].Files)
	}
}

func TestStatusReportListsMergeConflictKeys(t *testing.T) {
	entry := func(content string) core.SnapshotEntry {
		return core.SnapshotEntry{Kind: "json", Content: content}
	}
	base := core.SnapshotFiles{"settings.json": entry(`{"a":1,"b":2}`)}
	local := core.SnapshotFiles{"settings.json": entry(`{"a":9,"b":2}`)}
	remote := core.SnapshotFiles{"settings.json": entry(`{"a":3,"b":2}`)}
	drifts := engine.ComputeDrift([]core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{{Category: "settings", Mode: core.SyncModeMerge, Files: base}}},
	}, []core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{{Category: "settings", Mode: core.SyncModeMerge, Files: local}}},
	}, []core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{{Category: "settings", Mode: core.SyncModeMerge, Files: remote}}},
	})
	report := BuildStatusReport(drifts)
	got := map[string]string{}
	for _, file := range report.Adapters[0].Categories[0].Files {
		got[file.Path] = file.Status
	}
	if got["settings.json:a"] != "conflict" {
		t.Fatalf("files = %+v", report.Adapters[0].Categories[0].Files)
	}
}

// applyManifestView expands a manifest category's virtual file into one
// StatusFileReport per package.
func TestApplyManifestViewExpandsPackages(t *testing.T) {
	report := StatusReport{Adapters: []StatusAdapterReport{{
		ID: "pi",
		Categories: []StatusCategoryReport{{
			Name:  "packages",
			Kind:  string(core.CategoryKindManifest),
			Files: []StatusFileReport{{Path: "packages.manifest.txt", Status: "push"}},
		}},
	}}}
	kinds := map[string]map[string]string{"pi": {"packages": "manifest"}}
	local := map[string]map[string]string{"pi": {"packages": "npm:pi-lens\nnpm:pi-sop\n"}}
	applyManifestView(&report, kinds, local)
	files := report.Adapters[0].Categories[0].Files
	if len(files) != 2 {
		t.Fatalf("expanded files = %+v", files)
	}
	if files[0].Path != "npm:pi-lens" || files[1].Path != "npm:pi-sop" {
		t.Fatalf("package names = %+v", files)
	}
}

// A machine without homer.json must still SHOW its live configuration:
// the built-in default adapters (pi/herdr/opencode/vscode) are compiled
// into the binary — scanning them read-only needs no init. The report
// carries a warning explaining the fallback (no baseline yet).
func TestStatusFallsBackToDefaultsOnFreshMachine(t *testing.T) {
	home := t.TempDir()
	// A pi installation exists on this fresh machine (packages +
	// settings), but no homer.json.
	piRoot := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(filepath.Join(piRoot, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(piRoot, "extensions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(piRoot, "settings.json"), []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOMER_HOME", home)
	t.Setenv("HOME", home)
	report, err := RunStatus(StatusOptions{})
	if err != nil {
		t.Fatalf("fresh machine status must fall back to defaults: %v", err)
	}
	if report.ConfigOutline != nil {
		t.Fatalf("fresh machine fallback must not attach configOutline: %#v", *report.ConfigOutline)
	}
	found := false
	for _, adapter := range report.Adapters {
		if adapter.ID == "pi" {
			found = true
			for _, category := range adapter.Categories {
				if category.Name == "settings" && len(category.Files) > 0 {
					return // PASS: live config visible without init
				}
			}
		}
	}
	if !found {
		t.Fatalf("pi adapter missing from fallback scan: %+v", report.Adapters)
	}
	t.Fatal("pi settings files missing from fallback scan")
}

// The pi/packages manifest category IS the flagship scenario (plugin
// sync: "查看 pi 安装了什么插件"). It must be part of the compiled
// default adapters so fresh machines report their plugins without any
// configuration — a machine with pi plugins installed must show them.
func TestDefaultAdaptersIncludePiPackages(t *testing.T) {
	config := defaultAdaptersForScan()
	piPackages, ok := config["pi"].Categories["packages"]
	if !ok {
		t.Fatal("compiled default must include pi/packages (plugin manifest — the flagship sync category)")
	}
	if piPackages.Kind == nil || *piPackages.Kind != core.CategoryKindManifest {
		t.Fatal("pi/packages must be a manifest category")
	}
	if piPackages.ListCmd == "" || piPackages.ApplyCmd == "" {
		t.Fatal("pi/packages must carry listCmd and applyCmd")
	}
}
