package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/kballard/go-shellquote"
	"github.com/zzjcool/homer-cli/internal/core"
)

func TestInspectNoHubSnapshotUsesParallelPathOnce(t *testing.T) {
	// This script has no self-exec path: one append, then exit. The counter
	// filename is embedded in the script because manifest commands run with a
	// deliberately minimal environment that does not inherit test env vars.
	fixtureDir := t.TempDir()
	counterPath := filepath.Join(fixtureDir, "manifest-scans.txt")
	scriptPath := filepath.Join(fixtureDir, "count-manifest-scan.sh")
	script := "#!/bin/sh\nprintf 'scan\\n' >> " + shellquote.Join(counterPath) + "\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write manifest counter script: %v", err)
	}

	toolHome := filepath.Join(fixtureDir, "tool-home")
	homerHome := filepath.Join(fixtureDir, "homer-home")
	if err := os.MkdirAll(toolHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(homerHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", toolHome)
	root := filepath.Join(toolHome, "pi-agent")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	listCommand := shellquote.Join(scriptPath)
	kind := core.CategoryKindManifest
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: root, Categories: map[string]core.CategoryConfig{
			"packages": {Kind: &kind, Mode: core.SyncModeMirror, ListCmd: listCommand, ApplyCmd: "unused install"},
		}},
	}}
	paths := executorPaths(homerHome)
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatalf("save inspect config: %v", err)
	}

	var snapshotRequests atomic.Int32
	dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		snapshotRequests.Add(1)
		http.Error(w, `{"error":{"code":"no-snapshot"}}`, http.StatusConflict)
	}))
	defer dataPlane.Close()
	executor := &localExecutor{homerHome: homerHome, hubURL: dataPlane.URL}

	// Use the established whole RunStatus-equivalent executor path as the
	// baseline, then isolate scan and request counts to optimized inspect.
	want, err := executor.Status(context.Background())
	if err != nil {
		t.Fatalf("whole RunStatus baseline: %v", err)
	}
	if err := os.WriteFile(counterPath, nil, 0o600); err != nil {
		t.Fatalf("reset manifest scan counter: %v", err)
	}
	snapshotRequests.Store(0)

	got, err := inspectLocalStatus(context.Background(), executor, paths, &config, newInspectProgressTracker(nil, 0))
	if err != nil {
		t.Fatalf("per-adapter inspect without a hub snapshot: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("inspect without remote snapshot differs from whole RunStatus\ninspect: %s\nstatus: %s", gotJSON, wantJSON)
	}
	counter, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatalf("read manifest scan counter: %v", err)
	}
	if scans := countManifestScanLines(counter); scans != 1 {
		t.Fatalf("inspect scanned manifests %d times, want exactly once; counter=%q", scans, counter)
	}
	if requests := snapshotRequests.Load(); requests != 1 {
		t.Fatalf("inspect made %d snapshot requests for a stable 409 hub, want one optimized-path request", requests)
	}
}

func countManifestScanLines(contents []byte) int {
	count := 0
	for _, value := range contents {
		if value == '\n' {
			count++
		}
	}
	return count
}
