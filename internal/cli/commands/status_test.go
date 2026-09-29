package commands

import (
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
