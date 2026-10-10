package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
)

// TestScopedPushRetriesOnGenerationConflict (R2): a scoped push whose CAS
// precondition fails because another machine moved the center must
// re-download, re-judge, and publish against the new generation — once. The
// second attempt succeeds and the report is a normal pushed one.
func TestScopedPushRetriesOnGenerationConflict(t *testing.T) {
	userHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(userHome, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userHome, ".pi", "agent", "settings.json"), []byte("{\"theme\":\"local\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	homerHome := filepath.Join(userHome, ".homer")
	if err := core.SaveConfig(core.GetHomerPaths(func(k string) string {
		if k == "HOMER_HOME" {
			return homerHome
		}
		return ""
	}), core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: filepath.Join(userHome, ".pi", "agent"), Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
		}},
	}}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	generation := 3
	downloadAdvanced := false
	var uploads, conflicts int32

	// The upload endpoint is the same /api/snapshot with POST: split by method.
	hub2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			mu.Lock()
			current := generation
			if !downloadAdvanced {
				// Another machine publishes right after the FIRST download,
				// before the agent can POST: that CAS base goes stale. The
				// retry's download must see a stable center.
				generation = current + 1
				downloadAdvanced = true
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":         true,
				"generation": current,
				"homerJson":  "{}",
				"store": map[string]map[string]string{
					"pi": {"settings/settings.json": "{\"theme\":\"center\"}\n"},
				},
			})
		case http.MethodPost:
			var payload hubSnapshotPayload
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode upload: %v", err)
			}
			mu.Lock()
			current := generation
			mu.Unlock()
			if payload.Generation > 0 && payload.Generation != current {
				atomic.AddInt32(&conflicts, 1)
				// The center moved after this decision was prepared.
				if atomic.LoadInt32(&conflicts) == 1 {
					mu.Lock()
					generation = current + 1
					mu.Unlock()
				}
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]any{
					"code": "generation-conflict", "message": "中心已被其他机器更新",
				}})
				return
			}
			atomic.AddInt32(&uploads, 1)
			mu.Lock()
			generation++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "generation": generation})
		default:
			w.Header().Set("Allow", "GET, POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer hub2.Close()

	executor := NewLocalExecutorWithHub(homerHome, hub2.URL, "secret")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	report, err := executor.Push(ctx, true, []string{"pi"}, true, false)
	if err != nil {
		t.Fatalf("scoped push: %v", err)
	}
	if report.Status != commands.PushStatusPushed {
		t.Fatalf("scoped push should publish after one CAS retry: %+v", report)
	}
	if got := atomic.LoadInt32(&conflicts); got != 1 {
		t.Fatalf("expected exactly one CAS conflict, got %d", got)
	}
	if got := atomic.LoadInt32(&uploads); got != 1 {
		t.Fatalf("expected one successful upload, got %d", got)
	}
}

// TestScopedPushKeepsDecisionAfterRepeatedConflict: when the center keeps
// moving, the scoped push gives up after the retry and surfaces the
// generation conflict so the caller keeps its recorded decision.
func TestScopedPushKeepsDecisionAfterRepeatedConflict(t *testing.T) {
	userHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(userHome, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userHome, ".pi", "agent", "settings.json"), []byte("{\"theme\":\"local\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	homerHome := filepath.Join(userHome, ".homer")
	if err := core.SaveConfig(core.GetHomerPaths(func(k string) string {
		if k == "HOMER_HOME" {
			return homerHome
		}
		return ""
	}), core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: filepath.Join(userHome, ".pi", "agent"), Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
		}},
	}}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	generation := 10
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			mu.Lock()
			current := generation
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":         true,
				"generation": current,
				"homerJson":  "{}",
				"store": map[string]map[string]string{
					"pi": {"settings/settings.json": "{\"theme\":\"center\"}\n"},
				},
			})
		case http.MethodPost:
			var payload hubSnapshotPayload
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode upload: %v", err)
				return
			}
			// Always advance: every CAS check fails.
			mu.Lock()
			generation++
			mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]any{
				"code":    "generation-conflict",
				"message": "中心已被其他机器更新",
			}})
		default:
			w.Header().Set("Allow", "GET, POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer hub.Close()

	executor := NewLocalExecutorWithHub(homerHome, hub.URL, "secret")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	report, err := executor.Push(ctx, true, []string{"pi"}, true, false)
	if err != nil {
		t.Fatalf("scoped push: %v", err)
	}
	if report.Status == commands.PushStatusPushed {
		t.Fatalf("a permanently moving center must not be overwritten: %+v", report)
	}
	surfaced := false
	for _, line := range report.Errors {
		if strings.Contains(line, "中心世代已变化") {
			surfaced = true
			break
		}
	}
	if !surfaced {
		t.Fatalf("report must surface the generation conflict: %+v", report)
	}
}
