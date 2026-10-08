package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/shellenv"
	"github.com/zzjcool/homer-cli/internal/web"
)

func TestInspectEquivalence(t *testing.T) {
	tests := []struct {
		name       string
		adapters   map[string]core.AdapterConfig
		base       []core.AdapterSnapshot
		remote     []core.AdapterSnapshot
		files      map[string]map[string]string
		wantIDs    []string
		wantErrors []string
		wantWarn   []string
	}{
		{
			name: "multiple-adapters-conflict-and-manifest",
			adapters: map[string]core.AdapterConfig{
				"pi": {
					Root: "~/pi-agent",
					Categories: map[string]core.CategoryConfig{
						"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMerge},
					},
				},
				"vscode": {
					Root: "~/vscode-user",
					Categories: map[string]core.CategoryConfig{
						"extensions": {
							Kind:     kindPointer(core.CategoryKindManifest),
							Mode:     core.SyncModeMirror,
							ListCmd:  "missing-inspect-fixture-cli",
							ApplyCmd: "missing-inspect-fixture-cli install",
						},
					},
				},
			},
			base: []core.AdapterSnapshot{{AdapterID: "pi", Categories: []core.CategorySnapshot{{
				AdapterID: "pi", Category: "settings", Mode: core.SyncModeMerge,
				Files: core.SnapshotFiles{"settings.json": {Kind: "json", Content: `{"theme":"light","stable":true}`}},
			}}}},
			remote: []core.AdapterSnapshot{
				{AdapterID: "pi", Categories: []core.CategorySnapshot{{
					AdapterID: "pi", Category: "settings", Mode: core.SyncModeMerge,
					Files: core.SnapshotFiles{"settings.json": {Kind: "json", Content: `{"theme":"blue","stable":true}`}},
				}}},
			},
			files: map[string]map[string]string{
				"pi": {"settings.json": `{"theme":"dark","stable":true}`},
			},
			wantIDs:  []string{"pi", "vscode"},
			wantWarn: []string{"vscode: missing-inspect-fixture-cli 未安装，extensions 已按空清单处理；pull 时远端清单将视为全量待装"},
		},
		{
			name: "scan-error-is-preserved",
			adapters: map[string]core.AdapterConfig{
				"opencode": {
					Root: "~/does-not-exist-opencode-root",
					Categories: map[string]core.CategoryConfig{
						"config": {Paths: []string{"opencode.json"}, Mode: core.SyncModeMerge},
					},
				},
				"pi": {
					Root: "~/pi-agent",
					Categories: map[string]core.CategoryConfig{
						"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
					},
				},
			},
			base: []core.AdapterSnapshot{{AdapterID: "pi", Categories: []core.CategorySnapshot{{
				AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror,
				Files: core.SnapshotFiles{"settings.json": {Kind: "file", Content: "baseline\n"}},
			}}}},
			files:      map[string]map[string]string{"pi": {"settings.json": "local\n"}},
			wantIDs:    []string{"opencode", "pi"},
			wantErrors: []string{"adapter root 不可读: opencode ("},
		},
		{
			name: "filtered-adapter-still-merges-full-status-before-selection",
			adapters: map[string]core.AdapterConfig{
				"pi": {Root: "~/pi-agent", Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
				}},
				"vscode": {Root: "~/vscode-user", Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
				}},
			},
			files: map[string]map[string]string{
				"pi":     {"settings.json": "local pi\n"},
				"vscode": {"settings.json": "local vscode\n"},
			},
			wantIDs: []string{"pi", "vscode"},
		},
		{
			name: "remote-adapter-not-configured-on-machine",
			adapters: map[string]core.AdapterConfig{
				"pi": {
					Root: "~/pi-agent",
					Categories: map[string]core.CategoryConfig{
						"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
					},
				},
			},
			remote: []core.AdapterSnapshot{{AdapterID: "vscode", Categories: []core.CategorySnapshot{{
				AdapterID: "vscode", Category: "settings", Mode: core.SyncModeMirror,
				Files: core.SnapshotFiles{"settings.json": {Kind: "file", Content: "center only\n"}},
			}}}},
			files:   map[string]map[string]string{"pi": {"settings.json": "local\n"}},
			wantIDs: []string{"pi", "vscode"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, progress := runInspectEquivalenceFixture(t, test.adapters, test.base, test.remote, test.files)
			for _, id := range test.wantIDs {
				if !containsAdapter(result.Adapters, id) {
					t.Errorf("status is missing adapter %q: %+v", id, result.Adapters)
				}
			}
			for _, text := range test.wantErrors {
				if !containsSubstring(result.Errors, text) {
					t.Errorf("status errors %q do not contain %q", result.Errors, text)
				}
			}
			for _, text := range test.wantWarn {
				if !containsString(result.Warnings, text) {
					t.Errorf("status warnings %q do not contain %q", result.Warnings, text)
				}
			}
			if test.name == "filtered-adapter-still-merges-full-status-before-selection" {
				selected := selectInspectAdapters(result, []string{"pi"})
				if len(selected.Adapters) != 1 || selected.Adapters[0].ID != "pi" {
					t.Fatalf("adapter selection = %+v", selected.Adapters)
				}
			}
			if len(progress) != len(result.Adapters) {
				t.Errorf("optimized inspect emitted %d adapter events for %d status adapters: %#v", len(progress), len(result.Adapters), progress)
			}
			for i, event := range progress {
				if event.Stage != "adapter" || event.Done != i+1 || event.Total != len(result.Adapters) {
					t.Errorf("adapter progress[%d] = %+v", i, event)
				}
			}
		})
	}

	t.Run("fresh-machine-keeps-no-config-marker", func(t *testing.T) {
		home := t.TempDir()
		toolHome := t.TempDir()
		t.Setenv("HOME", toolHome)
		paths := core.GetHomerPaths(func(key string) string {
			if key == "HOMER_HOME" {
				return home
			}
			return os.Getenv(key)
		})
		if _, err := core.LoadConfig(paths); !os.IsNotExist(err) {
			t.Fatalf("fixture unexpectedly has homer.json: %v", err)
		}
		local := &localExecutor{homerHome: home}
		expected, err := local.Status(context.Background())
		if err != nil {
			t.Fatalf("whole RunStatus on fresh machine: %v", err)
		}
		d := New(Config{HomerHome: home}, local)
		inspected, err := d.inspect(context.Background(), web.InspectParams{}, nil)
		if err != nil {
			t.Fatalf("inspect on fresh machine: %v", err)
		}
		if !reflect.DeepEqual(inspected.Status, expected) {
			inspectJSON, _ := json.MarshalIndent(inspected.Status, "", "  ")
			expectedJSON, _ := json.MarshalIndent(expected, "", "  ")
			t.Fatalf("fresh-machine inspect differs from whole RunStatus\ninspect: %s\nstatus:  %s", inspectJSON, expectedJSON)
		}
		if !containsSubstring(inspected.Status.Errors, "未找到 homer 配置") {
			t.Fatalf("fresh-machine marker missing from inspect errors: %q", inspected.Status.Errors)
		}
	})
}

func runInspectEquivalenceFixture(t *testing.T, adapters map[string]core.AdapterConfig, base, remote []core.AdapterSnapshot, files map[string]map[string]string) (commands.StatusReport, []web.InspectEvent) {
	t.Helper()
	toolHome := t.TempDir()
	homerHome := t.TempDir()
	t.Setenv("HOME", toolHome)
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return homerHome
		}
		return os.Getenv(key)
	})
	config := core.HomerConfig{Version: 1, Adapters: adapters}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatalf("save fixture homer.json: %v", err)
	}
	for _, snapshot := range base {
		if err := core.WriteSnapshotToStore(paths, snapshot); err != nil {
			t.Fatalf("write fixture base snapshot: %v", err)
		}
	}
	for adapterID, adapterConfig := range adapters {
		if strings.Contains(adapterConfig.Root, "does-not-exist") {
			continue
		}
		if err := os.MkdirAll(core.ExpandHome(adapterConfig.Root), 0o755); err != nil {
			t.Fatalf("mkdir adapter root: %v", err)
		}
		if _, ok := files[adapterID]; !ok {
			continue
		}
	}
	for adapterID, adapterFiles := range files {
		adapterConfig, ok := adapters[adapterID]
		if !ok {
			t.Fatalf("local files reference unknown adapter %q", adapterID)
		}
		root := core.ExpandHome(adapterConfig.Root)
		for name, content := range adapterFiles {
			path := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir tool file: %v", err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("write tool file: %v", err)
			}
		}
	}
	wireStore := wireStoreFromSnapshots(remote)
	var fullRequests atomic.Int32
	var notModified atomic.Int32
	dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		etag := `"g1"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fullRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubSnapshotPayload{Generation: 1, HomerJSON: `{}`, Store: wireStore})
	}))
	defer dataPlane.Close()
	executor := &localExecutor{homerHome: homerHome, hubURL: dataPlane.URL, credential: "fixture-secret"}
	want, err := executor.Status(context.Background())
	if err != nil {
		t.Fatalf("whole RunStatus baseline: %v", err)
	}
	var progressMu sync.Mutex
	var progress []web.InspectEvent
	tracker := newInspectProgressTracker(func(value any) {
		event, ok := value.(web.InspectEvent)
		if !ok {
			return
		}
		progressMu.Lock()
		progress = append(progress, event)
		progressMu.Unlock()
	}, 0)
	got, err := inspectLocalStatus(context.Background(), executor, paths, &config, tracker)
	if err != nil {
		t.Fatalf("per-adapter inspect: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("per-adapter status differs from whole RunStatus\ninspect: %s\nstatus: %s", gotJSON, wantJSON)
	}
	if fullRequests.Load() != 1 || notModified.Load() != 1 {
		t.Fatalf("whole+inspect snapshot requests = 200:%d 304:%d, want one 200 then one 304", fullRequests.Load(), notModified.Load())
	}
	if tracker.currentTotal() != len(got.Adapters) || tracker.done != len(got.Adapters) {
		t.Fatalf("inspect did not complete each adapter slice: done=%d total=%d report=%d", tracker.done, tracker.currentTotal(), len(got.Adapters))
	}
	progressMu.Lock()
	events := append([]web.InspectEvent(nil), progress...)
	progressMu.Unlock()
	return got, events
}

func TestInspectReusesConditionalSnapshotAcrossCalls(t *testing.T) {
	toolHome := t.TempDir()
	homerHome := t.TempDir()
	t.Setenv("HOME", toolHome)
	paths := executorPaths(homerHome)
	root := filepath.Join(toolHome, "pi-agent")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {
			Root: root,
			Categories: map[string]core.CategoryConfig{
				"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
			},
		},
	}}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatalf("save inspect config: %v", err)
	}
	var fullRequests atomic.Int32
	var notModified atomic.Int32
	dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		etag := `"g1"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fullRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"generation":1,"homerJson":"{}","store":{}}`))
	}))
	defer dataPlane.Close()

	d := New(Config{HomerHome: homerHome, DataURL: dataPlane.URL}, nil)
	for i := 0; i < 2; i++ {
		result, err := d.inspect(context.Background(), web.InspectParams{}, nil)
		if err != nil {
			t.Fatalf("inspect %d: %v", i+1, err)
		}
		if len(result.Status.Adapters) != 1 || result.Status.Adapters[0].ID != "pi" {
			t.Fatalf("inspect %d status = %+v", i+1, result.Status)
		}
	}
	if got := fullRequests.Load(); got != 1 {
		t.Fatalf("two consecutive inspect calls downloaded %d full snapshots, want one", got)
	}
	if got := notModified.Load(); got != 1 {
		t.Fatalf("two consecutive inspect calls received %d conditional 304s, want one", got)
	}
}

func TestInspectCollectsCredentialsSecretsAndKeys(t *testing.T) {
	toolHome := t.TempDir()
	homerHome := t.TempDir()
	t.Setenv("HOME", toolHome)
	paths := executorPaths(homerHome)
	root := filepath.Join(toolHome, ".pi", "agent")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(root, "auth.json")
	if err := os.WriteFile(credentialPath, []byte(`{"token":"local-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(root, "settings.json")
	if err := os.WriteFile(settingsPath, []byte("{\n  \"api_key\": \"abcdefghijklmnopqrstuvwx123456\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {
			Root: "~/.pi/agent",
			Categories: map[string]core.CategoryConfig{
				"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
			},
		},
	}}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatalf("save inspect fixture config: %v", err)
	}
	executor := &localExecutor{homerHome: homerHome}
	d := New(Config{HomerHome: homerHome}, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var events []web.InspectEvent
	result, err := d.inspect(ctx, web.InspectParams{
		Adapters:    []string{"pi"},
		Credentials: []string{"~/.pi/agent/auth.json", "~/.pi/agent/missing.json"},
		WantKeys:    true,
	}, func(value any) {
		if event, ok := value.(web.InspectEvent); ok {
			events = append(events, event)
		}
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !result.Present["~/.pi/agent/auth.json"] || result.Present["~/.pi/agent/missing.json"] {
		t.Fatalf("credential presence = %#v", result.Present)
	}
	if len(result.Secrets) != 1 || result.Secrets[0].Path != "pi/settings/settings.json" || result.Secrets[0].Line != 2 {
		t.Fatalf("secret preflight = %#v", result.Secrets)
	}
	if result.Keys == nil || !result.Keys.OK {
		t.Fatalf("keyring list = %#v", result.Keys)
	}
	stages := map[string]bool{}
	for _, event := range events {
		stages[event.Stage] = true
	}
	for _, stage := range []string{"adapter", "credentials", "secrets", "keys"} {
		if !stages[stage] {
			t.Errorf("inspect did not emit %q progress: %#v", stage, events)
		}
	}
	// The preflight is read-only: the local file remains unchanged and no
	// upload is attempted without a hub.
	if content, err := os.ReadFile(settingsPath); err != nil || !strings.Contains(string(content), "abcdefghijklmnopqrstuvwx123456") {
		t.Fatalf("secret preflight changed its source file: content=%q err=%v", content, err)
	}
}

func TestInspectPATHProbeAtMostOnce(t *testing.T) {
	// Invalidate before installing the reader so this assertion cannot inherit
	// a login PATH cached by another test in the same package.
	shellenv.Invalidate()
	previousRead := shellenv.ReadLoginPATH
	previousTTL := shellenv.LoginPATHTTL
	var reads atomic.Int32
	shellenv.ReadLoginPATH = func() string {
		reads.Add(1)
		return os.Getenv("PATH")
	}
	shellenv.LoginPATHTTL = 30 * time.Second
	t.Cleanup(func() {
		shellenv.ReadLoginPATH = previousRead
		shellenv.LoginPATHTTL = previousTTL
		shellenv.Invalidate()
	})

	toolHome := t.TempDir()
	homerHome := t.TempDir()
	t.Setenv("HOME", toolHome)
	paths := executorPaths(homerHome)
	root := filepath.Join(toolHome, "pi-agent")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {
			Root: root,
			Categories: map[string]core.CategoryConfig{
				"packages": {
					Kind:     kindPointer(core.CategoryKindManifest),
					Mode:     core.SyncModeMirror,
					ListCmd:  "homer-inspect-path-count-missing-cli",
					ApplyCmd: "homer-inspect-path-count-missing-cli install",
				},
			},
		},
	}}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatalf("save PATH fixture config: %v", err)
	}
	executor := &localExecutor{homerHome: homerHome}
	d := New(Config{HomerHome: homerHome}, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := d.inspect(ctx, web.InspectParams{Credentials: []string{"~/missing-auth.json"}, WantKeys: true}, nil)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(result.Status.Adapters) != 1 || result.Status.Adapters[0].ID != "pi" {
		t.Fatalf("inspect status = %+v", result.Status)
	}
	if got := reads.Load(); got > 1 {
		t.Fatalf("inspect launched login PATH reader %d times, want <= 1", got)
	}
}

func wireStoreFromSnapshots(snapshots []core.AdapterSnapshot) map[string]map[string]string {
	store := make(map[string]map[string]string)
	for _, snapshot := range snapshots {
		for _, category := range snapshot.Categories {
			for relPath, entry := range category.Files {
				if store[snapshot.AdapterID] == nil {
					store[snapshot.AdapterID] = make(map[string]string)
				}
				store[snapshot.AdapterID][category.Category+"/"+relPath] = entry.Content
			}
		}
	}
	return store
}

func kindPointer(kind core.CategoryKind) *core.CategoryKind { return &kind }

func containsAdapter(adapters []commands.StatusAdapterReport, id string) bool {
	for _, adapter := range adapters {
		if adapter.ID == id {
			return true
		}
	}
	return false
}

func containsSubstring(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func TestInspectParamsAdapterSelectionKeepsGlobalDiagnostics(t *testing.T) {
	status := commands.StatusReport{
		Adapters: []commands.StatusAdapterReport{{ID: "pi"}, {ID: "opencode"}},
		Errors:   []string{"global scan error"},
		Warnings: []string{"global scan warning"},
		Disabled: []string{"pi/extensions", "opencode"},
	}
	selected := selectInspectAdapters(status, []string{"pi"})
	if len(selected.Adapters) != 1 || selected.Adapters[0].ID != "pi" || !reflect.DeepEqual(selected.Errors, status.Errors) || !reflect.DeepEqual(selected.Warnings, status.Warnings) || !reflect.DeepEqual(selected.Disabled, []string{"pi/extensions"}) {
		t.Fatalf("selected status = %+v", selected)
	}
	if fmt.Sprint(status.Adapters[0].ID) != "pi" {
		t.Fatal("selection mutated the input status")
	}
}
