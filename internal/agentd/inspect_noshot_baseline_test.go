package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

// A brand-new hub has no snapshot yet (HTTP 409), but this machine's own store
// may already hold a baseline from an earlier `homer init`/push. The whole
// status path treats the missing hub snapshot as "remote == base" (nil
// remote). inspect must agree: handing it a non-nil EMPTY remote means "the
// center is really empty", which turns every file that exists locally and in
// the baseline into a phantom pull-delete the console then offers to collect.
func TestInspectNoHubSnapshotWithBaselineMatchesWholeStatus(t *testing.T) {
	fixtureDir := t.TempDir()
	toolHome := filepath.Join(fixtureDir, "tool-home")
	homerHome := filepath.Join(fixtureDir, "homer-home")
	root := filepath.Join(toolHome, "pi-agent")
	for _, dir := range []string{toolHome, homerHome, root} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", toolHome)

	const content = `{"theme":"dark"}`
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: root, Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
		}},
	}}
	paths := executorPaths(homerHome)
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	// Baseline identical to the local file: nothing has drifted.
	if err := core.WriteSnapshotToStore(paths, core.AdapterSnapshot{AdapterID: "pi", Categories: []core.CategorySnapshot{{
		AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror,
		Files: core.SnapshotFiles{"settings.json": {Kind: "file", Content: content}},
	}}}); err != nil {
		t.Fatal(err)
	}

	dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"no-snapshot"}}`, http.StatusConflict)
	}))
	defer dataPlane.Close()
	executor := &localExecutor{homerHome: homerHome, hubURL: dataPlane.URL}

	want, err := executor.Status(context.Background())
	if err != nil {
		t.Fatalf("whole status: %v", err)
	}
	got, err := inspectLocalStatus(context.Background(), executor, paths, &config, newInspectProgressTracker(nil, 0))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("inspect differs from whole status on an empty hub with a local baseline\ninspect: %s\nstatus: %s", gotJSON, wantJSON)
	}
	for _, adapter := range got.Adapters {
		if adapter.Pull != 0 {
			t.Fatalf("phantom pull on an empty hub: adapter %s pull=%d", adapter.ID, adapter.Pull)
		}
	}
}
