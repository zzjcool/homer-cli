package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/core"
)

func TestSnapshotCache(t *testing.T) {
	var generation atomic.Int64
	generation.Store(1)
	var gets200 atomic.Int32
	var gets304 atomic.Int32
	var lastValidator atomic.Value
	dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			generation.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"ok":true,"generation":%d}`, generation.Load())
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer data-secret" {
			http.Error(w, "missing data-plane credential", http.StatusUnauthorized)
			return
		}
		validator := r.Header.Get("If-None-Match")
		lastValidator.Store(validator)
		etag := fmt.Sprintf(`"g%d"`, generation.Load())
		w.Header().Set("ETag", etag)
		if validator == etag {
			gets304.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		gets200.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubSnapshotPayload{
			Generation: int(generation.Load()),
			HomerJSON:  fmt.Sprintf(`{"generation":%d}`, generation.Load()),
			Store: map[string]map[string]string{
				"pi": {"settings/settings.json": fmt.Sprintf(`{"generation":%d}`, generation.Load())},
			},
		})
	}))
	defer dataPlane.Close()

	controlPlane := httptest.NewServer(http.NotFoundHandler())
	defer controlPlane.Close()
	executor := New(Config{
		HubURL:      controlPlane.URL,
		DataURL:     dataPlane.URL,
		AgentSecret: "data-secret",
	}, nil).exec.(*localExecutor)

	first, firstMeta, err := executor.downloadHubSnapshot(context.Background())
	if err != nil {
		t.Fatalf("first snapshot download: %v", err)
	}
	if gets200.Load() != 1 || gets304.Load() != 0 {
		t.Fatalf("first download HTTP counts = 200:%d 304:%d", gets200.Load(), gets304.Load())
	}
	if len(first) != 1 || string(firstMeta) != `{"generation":1}` {
		t.Fatalf("first snapshot/meta = %#v %s", first, firstMeta)
	}
	if first[0].Categories[0].Files["settings.json"].Content != `{"generation":1}` {
		t.Fatalf("first decoded snapshot = %#v", first)
	}
	if got := lastValidator.Load(); got != nil && got.(string) != "" {
		t.Fatalf("initial request sent an unexpected validator %q", got)
	}

	// Returned slices are independent copies; a cache hit must return the
	// original decoded generation, not a caller's mutations.
	first[0].Categories[0].Files["settings.json"] = core.SnapshotEntry{Kind: "file", Content: "corrupted caller copy"}
	second, secondMeta, err := executor.downloadHubSnapshot(context.Background())
	if err != nil {
		t.Fatalf("conditional snapshot download: %v", err)
	}
	if gets200.Load() != 1 || gets304.Load() != 1 {
		t.Fatalf("second download HTTP counts = 200:%d 304:%d", gets200.Load(), gets304.Load())
	}
	if got := lastValidator.Load().(string); got != `"g1"` {
		t.Fatalf("If-None-Match = %q, want g1", got)
	}
	if second[0].Categories[0].Files["settings.json"].Content != `{"generation":1}` || string(secondMeta) != `{"generation":1}` {
		t.Fatalf("304 did not reuse immutable decoded cache: %#v meta=%s", second, secondMeta)
	}

	const callers = 20
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snapshot, _, downloadErr := executor.downloadHubSnapshot(context.Background())
			if downloadErr == nil && (len(snapshot) != 1 || snapshot[0].Categories[0].Files["settings.json"].Content != `{"generation":1}`) {
				downloadErr = fmt.Errorf("concurrent cache result = %#v", snapshot)
			}
			errs <- downloadErr
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := gets200.Load(); got != 1 {
		t.Fatalf("concurrent cache hits issued %d full downloads, want 1", got)
	}
	if got := gets304.Load(); got < 2 || got > callers+1 {
		t.Fatalf("concurrent conditional download count = %d, want between one shared 304 and %d independent callers", got-1, callers)
	}

	// A changed generation forces a full body, then upload invalidation forces
	// the next request to omit If-None-Match even when the cache has a body.
	generation.Store(2)
	changed, _, err := executor.downloadHubSnapshot(context.Background())
	if err != nil {
		t.Fatalf("changed snapshot download: %v", err)
	}
	if changed[0].Categories[0].Files["settings.json"].Content != `{"generation":2}` || gets200.Load() != 2 {
		t.Fatalf("changed generation cache = %#v, HTTP 200 count %d", changed, gets200.Load())
	}
	if _, err := executor.uploadHubSnapshot(context.Background(), []core.AdapterSnapshot{}, nil); err != nil {
		t.Fatalf("upload snapshot: %v", err)
	}
	if _, _, err := executor.downloadHubSnapshot(context.Background()); err != nil {
		t.Fatalf("download after upload invalidation: %v", err)
	}
	if got := lastValidator.Load().(string); got != "" {
		t.Fatalf("request after upload sent stale If-None-Match %q", got)
	}
	if got := gets200.Load(); got != 3 {
		t.Fatalf("full download count after upload = %d, want 3", got)
	}

	// The cache never treats a 304 as a body and falls back to an unconditional
	// request if invalidation races a conditional response.
	executor.snapshotCache.invalidate()
	if _, _, err := executor.downloadHubSnapshot(context.Background()); err != nil {
		t.Fatalf("unconditional download after explicit invalidation: %v", err)
	}
	if got := gets200.Load(); got != 4 {
		t.Fatalf("download after explicit invalidation returned 200 count %d, want 4", got)
	}
}

func TestSnapshotCacheInvalidationRaceRetriesUnconditional(t *testing.T) {
	executor := &localExecutor{hubURL: "", credential: "token"}
	executor.snapshotCache.store(0, `"g1"`, []core.AdapterSnapshot{{AdapterID: "old"}}, []byte("old"))
	conditionalStarted := make(chan struct{})
	releaseConditional := make(chan struct{})
	var first sync.Once
	var fullRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"g1"` {
			first.Do(func() { close(conditionalStarted) })
			<-releaseConditional
			w.Header().Set("ETag", `"g1"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Header.Get("If-None-Match") != "" {
			t.Errorf("retry retained stale validator %q", r.Header.Get("If-None-Match"))
		}
		fullRequests.Add(1)
		w.Header().Set("ETag", `"g2"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"generation":2,"homerJson":"g2","store":{"pi":{"settings/settings.json":"new"}}}`))
	}))
	defer server.Close()
	executor.hubURL = server.URL
	result := make(chan struct {
		snapshots []core.AdapterSnapshot
		meta      []byte
		err       error
	}, 1)
	go func() {
		snapshots, meta, err := executor.downloadHubSnapshot(context.Background())
		result <- struct {
			snapshots []core.AdapterSnapshot
			meta      []byte
			err       error
		}{snapshots: snapshots, meta: meta, err: err}
	}()
	select {
	case <-conditionalStarted:
	case <-time.After(time.Second):
		t.Fatal("conditional snapshot request did not start")
	}
	executor.snapshotCache.invalidate()
	close(releaseConditional)
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("download after invalidation race: %v", got.err)
		}
		if len(got.snapshots) != 1 || got.snapshots[0].AdapterID != "pi" || got.snapshots[0].Categories[0].Files["settings.json"].Content != "new" || string(got.meta) != "g2" {
			t.Fatalf("download returned a stale/invalidated generation: %#v meta=%q", got.snapshots, got.meta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("download did not retry after invalidation")
	}
	if got := fullRequests.Load(); got != 1 {
		t.Fatalf("retry full snapshot count = %d, want one", got)
	}
}

func TestSnapshotCacheRejectsOlderETag(t *testing.T) {
	var cache snapshotCache
	cache.store(0, `"g2"`, []core.AdapterSnapshot{{AdapterID: "newer"}}, []byte("newer"))
	cache.store(0, `"g1"`, []core.AdapterSnapshot{{AdapterID: "older"}}, []byte("older"))
	snapshots, meta, ok := cache.get(`"g2"`, 0)
	if !ok || len(snapshots) != 1 || snapshots[0].AdapterID != "newer" || string(meta) != "newer" {
		t.Fatalf("out-of-order cache state = %#v %q ok=%v", snapshots, meta, ok)
	}
}

func TestSnapshotCacheRetries304AfterInvalidation(t *testing.T) {
	var cache snapshotCache
	cache.store(0, `"g1"`, []core.AdapterSnapshot{{AdapterID: "pi"}}, []byte("cached"))
	etag, epoch, valid := cache.requestState()
	if !valid || etag != `"g1"` {
		t.Fatalf("initial cache state = etag %q valid %v", etag, valid)
	}
	cache.invalidate()
	if _, _, ok := cache.get(etag, epoch); ok {
		t.Fatal("invalidated generation remained readable")
	}
	if !strings.HasPrefix(etag, `"g`) {
		t.Fatalf("unexpected test etag %q", etag)
	}
}
