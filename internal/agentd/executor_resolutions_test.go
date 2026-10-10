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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/resolutions"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/web"
)

type resolutionAdapterFixture struct {
	Base   string
	Local  string
	Center string
}

type resolutionUpload struct {
	Adapters []string
	Store    map[string]map[string]string
}

type resolutionHubFixture struct {
	mu sync.Mutex

	generation int
	homerJSON  string
	store      map[string]map[string]string
	events     []string
	uploads    []resolutionUpload
	failGet    bool
	failPost   bool
	blockGet   bool
	getStarted chan struct{}
	releaseGet chan struct{}
	getBlocked bool
}

func newResolutionHubFixture(generation int, homerJSON string, store map[string]map[string]string) *resolutionHubFixture {
	return &resolutionHubFixture{
		generation: generation,
		homerJSON:  homerJSON,
		store:      cloneResolutionStore(store),
		getStarted: make(chan struct{}),
		releaseGet: make(chan struct{}),
	}
}

func (h *resolutionHubFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/snapshot":
		h.mu.Lock()
		h.events = append(h.events, "GET")
		block := h.blockGet && !h.getBlocked
		if block {
			h.getBlocked = true
			close(h.getStarted)
		}
		h.mu.Unlock()
		if block {
			select {
			case <-h.releaseGet:
			case <-r.Context().Done():
				return
			}
		}
		h.mu.Lock()
		if h.failGet {
			h.mu.Unlock()
			http.Error(w, "fixture pull failed", http.StatusServiceUnavailable)
			return
		}
		payload := hubSnapshotPayload{
			Generation: h.generation,
			HomerJSON:  h.homerJSON,
			Store:      cloneResolutionStore(h.store),
		}
		generation := h.generation
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", fmt.Sprintf("\"resolution-%d\"", generation))
		_ = json.NewEncoder(w).Encode(payload)
	case r.Method == http.MethodPost && r.URL.Path == "/api/snapshot":
		var payload hubSnapshotPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid snapshot", http.StatusBadRequest)
			return
		}
		adapters := append([]string(nil), payload.Adapters...)
		h.mu.Lock()
		h.events = append(h.events, "POST")
		h.uploads = append(h.uploads, resolutionUpload{Adapters: adapters, Store: cloneResolutionStore(payload.Store)})
		if h.failPost {
			h.mu.Unlock()
			http.Error(w, "fixture upload failed", http.StatusServiceUnavailable)
			return
		}
		if payload.Adapters == nil {
			h.store = cloneResolutionStore(payload.Store)
		} else {
			for _, adapter := range payload.Adapters {
				delete(h.store, adapter)
				if files, ok := payload.Store[adapter]; ok {
					h.store[adapter] = cloneResolutionStore(map[string]map[string]string{adapter: files})[adapter]
				}
			}
		}
		if payload.HomerJSON != "" {
			h.homerJSON = payload.HomerJSON
		}
		h.generation++
		generation := h.generation
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"generation":%d}`, generation)
	default:
		http.NotFound(w, r)
	}
}

func cloneResolutionStore(store map[string]map[string]string) map[string]map[string]string {
	copy := make(map[string]map[string]string, len(store))
	for adapter, files := range store {
		copy[adapter] = make(map[string]string, len(files))
		for path, content := range files {
			copy[adapter][path] = content
		}
	}
	return copy
}

func (h *resolutionHubFixture) snapshot() (int, map[string]map[string]string, []string, []resolutionUpload) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.generation, cloneResolutionStore(h.store), append([]string(nil), h.events...), append([]resolutionUpload(nil), h.uploads...)
}

func (h *resolutionHubFixture) setFailures(get, post bool) {
	h.mu.Lock()
	h.failGet = get
	h.failPost = post
	h.mu.Unlock()
}

func (h *resolutionHubFixture) blockFirstGet() {
	h.mu.Lock()
	h.blockGet = true
	h.mu.Unlock()
}

type resolutionExecutorFixture struct {
	home      string
	paths     core.HomerPaths
	roots     map[string]string
	executor  *localExecutor
	hub       *resolutionHubFixture
	httpHub   *httptest.Server
	centerGen int
}

func newResolutionExecutorFixture(t *testing.T, adapters map[string]resolutionAdapterFixture) *resolutionExecutorFixture {
	t.Helper()
	if len(adapters) == 0 {
		t.Fatal("resolution test fixture needs at least one adapter")
	}
	toolHome := t.TempDir()
	t.Setenv("HOME", toolHome)
	homerHome := t.TempDir()
	paths := homerPathsForHome(homerHome)
	ids := make([]string, 0, len(adapters))
	config := core.HomerConfig{Version: 1, Adapters: make(map[string]core.AdapterConfig, len(adapters))}
	roots := make(map[string]string, len(adapters))
	baseSnapshots := make([]core.AdapterSnapshot, 0, len(adapters))
	remoteSnapshots := make([]core.AdapterSnapshot, 0, len(adapters))
	for adapter := range adapters {
		ids = append(ids, adapter)
	}
	sort.Strings(ids)
	for _, adapter := range ids {
		values := adapters[adapter]
		root := filepath.Join(toolHome, ".resolution-fixture", adapter)
		roots[adapter] = root
		config.Adapters[adapter] = core.AdapterConfig{
			Root: root,
			Categories: map[string]core.CategoryConfig{
				"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
			},
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(values.Local), 0o600); err != nil {
			t.Fatal(err)
		}
		baseSnapshots = append(baseSnapshots, resolutionSnapshot(adapter, values.Base))
		remoteSnapshots = append(remoteSnapshots, resolutionSnapshot(adapter, values.Center))
	}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatalf("save fixture homer.json: %v", err)
	}
	for _, snapshot := range baseSnapshots {
		if err := core.WriteSnapshotToStore(paths, snapshot); err != nil {
			t.Fatalf("write fixture base for %s: %v", snapshot.AdapterID, err)
		}
	}
	meta, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read fixture homer.json: %v", err)
	}
	hubFixture := newResolutionHubFixture(5, string(meta), wireStoreFromSnapshots(remoteSnapshots))
	server := httptest.NewServer(hubFixture)
	executor := &localExecutor{homerHome: homerHome, hubURL: server.URL, credential: "resolution-fixture-token"}
	fixture := &resolutionExecutorFixture{
		home:      homerHome,
		paths:     paths,
		roots:     roots,
		executor:  executor,
		hub:       hubFixture,
		httpHub:   server,
		centerGen: 5,
	}
	t.Cleanup(server.Close)
	return fixture
}

func resolutionSnapshot(adapter, content string) core.AdapterSnapshot {
	return core.AdapterSnapshot{AdapterID: adapter, Categories: []core.CategorySnapshot{{
		AdapterID: adapter, Category: "settings", Mode: core.SyncModeMirror,
		Files: core.SnapshotFiles{"settings.json": {Kind: "file", Content: content}},
	}}}
}

func (f *resolutionExecutorFixture) record(t *testing.T, choice string, adapters ...string) []resolutions.Entry {
	t.Helper()
	result, err := resolutions.Record(f.paths, choice, adapters, f.centerGen, nil)
	if err != nil {
		t.Fatalf("record %s resolutions: %v", choice, err)
	}
	return result.Recorded
}

func (f *resolutionExecutorFixture) readLocal(t *testing.T, adapter string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(f.roots[adapter], "settings.json"))
	if err != nil {
		t.Fatalf("read local %s: %v", adapter, err)
	}
	return string(contents)
}

func (f *resolutionExecutorFixture) loadEntries(t *testing.T) []resolutions.Entry {
	t.Helper()
	file, err := resolutions.Load(f.paths)
	if err != nil {
		t.Fatalf("load staged resolutions: %v", err)
	}
	return file.Entries
}

func TestPullApplyingResolutions_ConfigPolicyFixesCategoryDefinitionDrift(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi": {Base: "base\n", Local: "local\n", Center: "center\n"},
	})
	centerConfig, err := core.LoadConfig(fixture.paths)
	if err != nil {
		t.Fatal(err)
	}
	pi := centerConfig.Adapters["pi"]
	pi.Categories = map[string]core.CategoryConfig{
		"settings": pi.Categories["settings"],
		"files":    {Paths: []string{"files/"}, Mode: core.SyncModeMirror},
	}
	centerConfig.Adapters["pi"] = pi
	meta := centerMeta(t, fixture.home, *centerConfig)
	fixture.hub.mu.Lock()
	fixture.hub.homerJSON = string(meta)
	fixture.hub.store["pi"]["files/new.txt"] = "center file\n"
	fixture.hub.mu.Unlock()
	fixture.record(t, resolutions.ChoiceCenter, "pi")
	daemon := New(Config{HomerHome: fixture.home}, fixture.executor)

	result, err := daemon.runTaskCommand(context.Background(), &stream.Request{Method: string(hub.TaskKindPull)}, hub.TaskOptions{
		Confirm: true, Adapters: []string{"pi"}, CenterGeneration: fixture.centerGen,
		ApplyResolutions: true, ConfigPolicy: map[string]string{"pi": "center"},
	})
	if err != nil {
		t.Fatalf("resolved pull: %v", err)
	}
	report, ok := result.(commands.PullReport)
	if !ok {
		t.Fatalf("resolved pull result = %T, want PullReport", result)
	}
	if !report.OK || report.Status != commands.PullStatusApplied {
		t.Fatalf("category definition drift blocked the resolved pull: %+v", report)
	}
	if data, err := os.ReadFile(filepath.Join(fixture.roots["pi"], "files", "new.txt")); err != nil || string(data) != "center file\n" {
		t.Fatalf("center files category was not applied: %q, %v", data, err)
	}
	if entries := fixture.loadEntries(t); len(entries) != 0 {
		t.Fatalf("staged decision was not consumed: %+v", entries)
	}
}

func TestTaskPullConfigPolicyUsesLocalExecutor(t *testing.T) {
	daemon := New(Config{HomerHome: t.TempDir()}, &resolutionTaskExecutor{})
	_, err := daemon.runTaskCommand(context.Background(), &stream.Request{Method: string(hub.TaskKindPull)}, hub.TaskOptions{
		Confirm: true, ConfigPolicy: map[string]string{"pi": "center"},
	})
	if err == nil || !strings.Contains(err.Error(), "executor") {
		t.Fatalf("pull with config policy error = %v, want local-executor assertion error", err)
	}
}

func TestTaskPullConfigPolicyIsAppliedWithoutResolutions(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi": {Base: "center\n", Local: "center\n", Center: "center\n"},
	})
	centerConfig, err := core.LoadConfig(fixture.paths)
	if err != nil {
		t.Fatal(err)
	}
	pi := centerConfig.Adapters["pi"]
	pi.Categories["files"] = core.CategoryConfig{Paths: []string{"files/"}, Mode: core.SyncModeMirror}
	centerConfig.Adapters["pi"] = pi
	fixture.hub.mu.Lock()
	fixture.hub.homerJSON = string(centerMeta(t, fixture.home, *centerConfig))
	fixture.hub.store["pi"]["files/new.txt"] = "center file\n"
	fixture.hub.mu.Unlock()
	daemon := New(Config{HomerHome: fixture.home}, fixture.executor)

	result, err := daemon.runTaskCommand(context.Background(), &stream.Request{Method: string(hub.TaskKindPull)}, hub.TaskOptions{
		Confirm: true, Adapters: []string{"pi"}, ConfigPolicy: map[string]string{"pi": "center"},
	})
	if err != nil {
		t.Fatalf("ordinary pull with config policy: %v", err)
	}
	report, ok := result.(commands.PullReport)
	if !ok || !report.OK {
		t.Fatalf("ordinary pull with config policy result = %#v", result)
	}
	if data, err := os.ReadFile(filepath.Join(fixture.roots["pi"], "files", "new.txt")); err != nil || string(data) != "center file\n" {
		t.Fatalf("center files category was not applied: %q, %v", data, err)
	}
}

func TestPullApplyingResolutions_ConfigPolicySurvivesLegacyNoEntryPath(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi": {Base: "base\n", Local: "local\n", Center: "center\n"},
	})
	centerConfig, err := core.LoadConfig(fixture.paths)
	if err != nil {
		t.Fatal(err)
	}
	pi := centerConfig.Adapters["pi"]
	pi.Categories["files"] = core.CategoryConfig{Paths: []string{"files/"}, Mode: core.SyncModeMirror}
	centerConfig.Adapters["pi"] = pi
	fixture.hub.mu.Lock()
	fixture.hub.homerJSON = string(centerMeta(t, fixture.home, *centerConfig))
	fixture.hub.store["pi"]["files/new.txt"] = "center file\n"
	fixture.hub.mu.Unlock()
	if err := os.WriteFile(filepath.Join(fixture.roots["pi"], "settings.json"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := fixture.executor.PullApplyingResolutions(context.Background(), ResolvedPullRequest{
		Confirm: true, Adapters: []string{"pi"}, CenterGeneration: fixture.centerGen,
		ConfigPolicy: map[string]string{"pi": "center"},
	})
	if err != nil {
		t.Fatalf("legacy fallback pull: %v", err)
	}
	if !report.OK {
		t.Fatalf("legacy fallback pull failed: %+v", report)
	}
	got, err := core.LoadConfig(fixture.paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Adapters["pi"].Categories["files"]; !ok {
		t.Fatalf("config policy was lost on no-entry fallback: %+v", got.Adapters["pi"])
	}
	if entries := fixture.loadEntries(t); len(entries) != 0 {
		t.Fatalf("unexpected resolutions = %+v", entries)
	}
}

func TestPullApplyingResolutions_CenterRewritesAndClears(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi": {Base: "base\n", Local: "local\n", Center: "center\n"},
	})
	fixture.record(t, resolutions.ChoiceCenter, "pi")

	report, err := fixture.executor.PullApplyingResolutions(context.Background(), ResolvedPullRequest{
		Confirm: true, Adapters: []string{"pi"}, CenterGeneration: fixture.centerGen,
	})
	if err != nil {
		t.Fatalf("resolved pull: %v", err)
	}
	if !report.OK || report.Status != commands.PullStatusApplied || fixture.readLocal(t, "pi") != "center\n" {
		t.Fatalf("pull report/local = %+v / %q", report, fixture.readLocal(t, "pi"))
	}
	if report.Applied.BackupDir == "" {
		t.Fatal("center rewrite did not create a backup directory")
	}
	if info, err := os.Stat(report.Applied.BackupDir); err != nil || !info.IsDir() {
		t.Fatalf("backup directory %q: %v", report.Applied.BackupDir, err)
	}
	if entries := fixture.loadEntries(t); len(entries) != 0 {
		t.Fatalf("staged decisions after applied center choice = %+v", entries)
	}
	if len(report.Resolutions) != 1 || report.Resolutions[0].Adapter != "pi" || report.Resolutions[0].Status != resolutions.OutcomeApplied {
		t.Fatalf("resolution outcomes = %+v", report.Resolutions)
	}
}

func TestPullApplyingResolutions_LocalPublishesAndSkipsMachine(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi": {Base: "base\n", Local: "local\n", Center: "center\n"},
	})
	fixture.record(t, resolutions.ChoiceLocal, "pi")

	report, err := fixture.executor.PullApplyingResolutions(context.Background(), ResolvedPullRequest{
		Confirm: true, Adapters: []string{"pi"}, CenterGeneration: fixture.centerGen,
	})
	if err != nil {
		t.Fatalf("resolved pull: %v", err)
	}
	if !report.OK || fixture.readLocal(t, "pi") != "local\n" {
		t.Fatalf("local decision changed machine or failed: report=%+v local=%q", report, fixture.readLocal(t, "pi"))
	}
	_, store, events, uploads := fixture.hub.snapshot()
	if len(uploads) != 1 || !reflect.DeepEqual(uploads[0].Adapters, []string{"pi"}) {
		t.Fatalf("hub upload scopes = %+v (events %v)", uploads, events)
	}
	if store["pi"]["settings/settings.json"] != "local\n" {
		t.Fatalf("published center content = %q, want local", store["pi"]["settings/settings.json"])
	}
	if !reflect.DeepEqual(events, []string{"GET", "POST"}) {
		t.Fatalf("local-only decision unexpectedly ran a pull; hub events = %v", events)
	}
	if len(report.Resolutions) != 1 || report.Resolutions[0].Status != resolutions.OutcomePublished {
		t.Fatalf("resolution outcomes = %+v", report.Resolutions)
	}
	if entries := fixture.loadEntries(t); len(entries) != 0 {
		t.Fatalf("staged decisions after publish = %+v", entries)
	}
}

func TestPullApplyingResolutions_StaleNotApplied(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi": {Base: "base\n", Local: "local\n", Center: "center\n"},
	})
	fixture.record(t, resolutions.ChoiceCenter, "pi")

	report, err := fixture.executor.PullApplyingResolutions(context.Background(), ResolvedPullRequest{
		Confirm: true, Adapters: []string{"pi"}, CenterGeneration: fixture.centerGen + 1,
	})
	if err != nil {
		t.Fatalf("stale pull: %v", err)
	}
	if report.OK || report.Status != commands.PullStatusConflictsRemain || fixture.readLocal(t, "pi") != "local\n" {
		t.Fatalf("stale decision was applied: report=%+v local=%q", report, fixture.readLocal(t, "pi"))
	}
	if len(report.Resolutions) != 1 || report.Resolutions[0].Status != resolutions.OutcomeStale {
		t.Fatalf("stale outcomes = %+v", report.Resolutions)
	}
	if entries := fixture.loadEntries(t); len(entries) != 1 {
		t.Fatalf("stale decision was deleted: %+v", entries)
	}
}

func TestPullApplyingResolutions_PullErrorKeepsEntry(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi": {Base: "base\n", Local: "local\n", Center: "center\n"},
	})
	fixture.record(t, resolutions.ChoiceCenter, "pi")
	fixture.hub.setFailures(true, false)

	report, err := fixture.executor.PullApplyingResolutions(context.Background(), ResolvedPullRequest{
		Confirm: true, Adapters: []string{"pi"}, CenterGeneration: fixture.centerGen,
	})
	if err != nil {
		t.Fatalf("failed resolved pull should return its report: %v", err)
	}
	if report.OK || report.Status != commands.PullStatusError || len(report.Resolutions) != 1 || report.Resolutions[0].Status != resolutions.OutcomeFailed {
		t.Fatalf("pull error report = %+v", report)
	}
	if !containsSubstring(report.Errors, "决定已保留") || len(fixture.loadEntries(t)) != 1 {
		t.Fatalf("pull error did not retain the decision: errors=%v entries=%+v", report.Errors, fixture.loadEntries(t))
	}
}

func TestPullApplyingResolutions_LocalUploadFailureKeepsEntry(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi":  {Base: "base-pi\n", Local: "local-pi\n", Center: "center-pi\n"},
		"pad": {Base: "base-pad\n", Local: "local-pad\n", Center: "center-pad\n"},
	})
	fixture.record(t, resolutions.ChoiceLocal, "pi")
	fixture.record(t, resolutions.ChoiceCenter, "pad")
	fixture.hub.setFailures(false, true)

	report, err := fixture.executor.PullApplyingResolutions(context.Background(), ResolvedPullRequest{
		Confirm: true, Adapters: []string{"pi", "pad"}, CenterGeneration: fixture.centerGen,
	})
	if err != nil {
		t.Fatalf("local upload failure should return its report: %v", err)
	}
	if report.OK || len(report.Resolutions) != 2 || report.Resolutions[0].Status != resolutions.OutcomeFailed || report.Resolutions[1].Status != resolutions.OutcomeApplied {
		t.Fatalf("local upload failure report = %+v", report)
	}
	if fixture.readLocal(t, "pad") != "center-pad\n" {
		t.Fatalf("remaining center decision was not applied: %q", fixture.readLocal(t, "pad"))
	}
	entries := fixture.loadEntries(t)
	if len(entries) != 1 || entries[0].Adapter != "pi" {
		t.Fatalf("failed local decision/consumed center decision = %+v", entries)
	}
	if !containsSubstring(report.Errors, "决定已保留") {
		t.Fatalf("failed local report errors = %v", report.Errors)
	}
}

func TestPullApplyingResolutions_LocalSecretsRejectedKeepsEntry(t *testing.T) {
	const suspectSecret = "sk-ant-abcdefghijklmnopqrst"
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi": {Base: "base\n", Local: suspectSecret + "\n", Center: "center\n"},
	})
	fixture.record(t, resolutions.ChoiceLocal, "pi")

	report, err := fixture.executor.PullApplyingResolutions(context.Background(), ResolvedPullRequest{
		Confirm: true, Adapters: []string{"pi"}, CenterGeneration: fixture.centerGen,
	})
	if err != nil {
		t.Fatalf("secret rejection should return a report: %v", err)
	}
	if report.OK || report.Status != commands.PullStatusError || len(report.Resolutions) != 1 || report.Resolutions[0].Status != resolutions.OutcomeFailed {
		t.Fatalf("secret rejection report = %+v", report)
	}
	if !strings.Contains(report.Resolutions[0].Message, "pi 没有写入中心") || !strings.Contains(report.Resolutions[0].Message, "决定已保留") {
		t.Fatalf("secret rejection guidance = %q", report.Resolutions[0].Message)
	}
	_, _, _, uploads := fixture.hub.snapshot()
	if len(uploads) != 0 {
		t.Fatalf("secret rejection uploaded data: %+v", uploads)
	}
	if entries := fixture.loadEntries(t); len(entries) != 1 {
		t.Fatalf("secret-rejected decision was cleared: %+v", entries)
	}
}

func TestPullApplyingResolutions_NoEntriesEqualsLegacyPull(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi": {Base: "base\n", Local: "local\n", Center: "center\n"},
	})
	request := ResolvedPullRequest{Confirm: true, Adapters: []string{"pi"}, CenterGeneration: fixture.centerGen}
	resolved, err := fixture.executor.PullApplyingResolutions(context.Background(), request)
	if err != nil {
		t.Fatalf("PullApplyingResolutions: %v", err)
	}
	legacy, err := fixture.executor.Pull(context.Background(), request.Confirm, request.Adapters, false)
	if err != nil {
		t.Fatalf("legacy Pull: %v", err)
	}
	resolvedJSON, _ := json.Marshal(resolved)
	legacyJSON, _ := json.Marshal(legacy)
	if !reflect.DeepEqual(resolved, legacy) || string(resolvedJSON) != string(legacyJSON) {
		t.Fatalf("empty staged-resolution pull differs from legacy pull\nresolved: %s\nlegacy:   %s", resolvedJSON, legacyJSON)
	}
}

func TestPullApplyingResolutions_MootClearedAsNoop(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi": {Base: "same\n", Local: "same\n", Center: "same\n"},
	})
	fixture.record(t, resolutions.ChoiceCenter, "pi")

	report, err := fixture.executor.PullApplyingResolutions(context.Background(), ResolvedPullRequest{
		Confirm: true, Adapters: []string{"pi"}, CenterGeneration: fixture.centerGen,
	})
	if err != nil {
		t.Fatalf("moot pull: %v", err)
	}
	if !report.OK || report.Status != commands.PullStatusNoDrift || len(report.Resolutions) != 1 || report.Resolutions[0].Status != resolutions.OutcomeNoop {
		t.Fatalf("moot decision report = %+v", report)
	}
	if entries := fixture.loadEntries(t); len(entries) != 0 {
		t.Fatalf("moot decision was not consumed: %+v", entries)
	}
}

func TestPullApplyingResolutions_OnlyConsumesSelected(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi":  {Base: "base-pi\n", Local: "local-pi\n", Center: "center-pi\n"},
		"pad": {Base: "base-pad\n", Local: "local-pad\n", Center: "center-pad\n"},
	})
	fixture.record(t, resolutions.ChoiceCenter, "pi", "pad")

	report, err := fixture.executor.PullApplyingResolutions(context.Background(), ResolvedPullRequest{
		Confirm: true, Adapters: []string{"pi"}, CenterGeneration: fixture.centerGen,
	})
	if err != nil {
		t.Fatalf("selected pull: %v", err)
	}
	if !report.OK || fixture.readLocal(t, "pi") != "center-pi\n" || fixture.readLocal(t, "pad") != "local-pad\n" {
		t.Fatalf("adapter selection was not respected: report=%+v pi=%q pad=%q", report, fixture.readLocal(t, "pi"), fixture.readLocal(t, "pad"))
	}
	entries := fixture.loadEntries(t)
	if len(entries) != 1 || entries[0].Adapter != "pad" {
		t.Fatalf("unselected staged decision was consumed: %+v", entries)
	}
}

func TestPullApplyingResolutions_CASKeepsReRecorded(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi": {Base: "base\n", Local: "local\n", Center: "center\n"},
	})
	old := fixture.record(t, resolutions.ChoiceCenter, "pi")[0]
	fixture.hub.blockFirstGet()
	pullDone := make(chan error, 1)
	go func() {
		_, err := fixture.executor.PullApplyingResolutions(context.Background(), ResolvedPullRequest{
			Confirm: true, Adapters: []string{"pi"}, CenterGeneration: fixture.centerGen,
		})
		pullDone <- err
	}()
	select {
	case <-fixture.hub.getStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("pull did not reach the snapshot download")
	}
	newEntry := resolutions.Entry{
		Adapter:            "pi",
		Choice:             resolutions.ChoiceLocal,
		RecordedAt:         time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano),
		GenerationAtRecord: fixture.centerGen,
	}
	if _, err := resolutions.Record(fixture.paths, newEntry.Choice, []string{newEntry.Adapter}, newEntry.GenerationAtRecord, func() string { return newEntry.RecordedAt }); err != nil {
		t.Fatalf("re-record while pull is in flight: %v", err)
	}
	close(fixture.hub.releaseGet)
	select {
	case err := <-pullDone:
		if err != nil {
			t.Fatalf("resolved pull: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resolved pull did not finish")
	}
	entries := fixture.loadEntries(t)
	if len(entries) != 1 || !entries[0].Same(newEntry) || old.Same(entries[0]) {
		t.Fatalf("CAS removed the re-recorded decision: old=%+v new=%+v entries=%+v", old, newEntry, entries)
	}
}

func TestPullApplyingResolutions_ConcurrentRecordSerialized(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi": {Base: "base\n", Local: "local\n", Center: "center\n"},
	})
	d := New(Config{HomerHome: fixture.home, AgentID: "concurrent-record"}, fixture.executor)
	const writers = 32
	var wg sync.WaitGroup
	errorsOut := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			choice := resolutions.ChoiceCenter
			if index%2 == 1 {
				choice = resolutions.ChoiceLocal
			}
			report, err := d.runResolveRecord(context.Background(), hub.TaskOptions{
				ResolutionAction: resolutions.ActionRecord,
				ResolutionChoice: choice,
				Adapters:         []string{"pi"},
				CenterGeneration: fixture.centerGen,
			})
			if err != nil {
				errorsOut <- err
				return
			}
			if !report.OK || report.Pending != 1 {
				errorsOut <- fmt.Errorf("record %d report: %+v", index, report)
			}
		}(i)
	}
	wg.Wait()
	close(errorsOut)
	for err := range errorsOut {
		t.Error(err)
	}
	entries := fixture.loadEntries(t)
	if len(entries) != 1 || entries[0].Adapter != "pi" || entries[0].GenerationAtRecord != fixture.centerGen {
		t.Fatalf("serialized record state = %+v", entries)
	}
}

func TestPullApplyingResolutions_PublishBeforePull(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi":  {Base: "base-pi\n", Local: "local-pi\n", Center: "center-pi\n"},
		"pad": {Base: "base-pad\n", Local: "local-pad\n", Center: "center-pad\n"},
	})
	fixture.record(t, resolutions.ChoiceLocal, "pi")
	fixture.record(t, resolutions.ChoiceCenter, "pad")

	report, err := fixture.executor.PullApplyingResolutions(context.Background(), ResolvedPullRequest{
		Confirm: true, Adapters: []string{"pi", "pad"}, CenterGeneration: fixture.centerGen,
	})
	if err != nil {
		t.Fatalf("mixed resolved pull: %v", err)
	}
	_, _, events, _ := fixture.hub.snapshot()
	postIndex, lastPrePostGet, firstPostPostGet := -1, -1, -1
	for index, event := range events {
		if event == "POST" && postIndex < 0 {
			postIndex = index
			continue
		}
		if event == "GET" {
			if postIndex < 0 {
				lastPrePostGet = index
			} else if firstPostPostGet < 0 {
				firstPostPostGet = index
			}
		}
	}
	if postIndex < 0 || lastPrePostGet < 0 || firstPostPostGet < 0 || !(lastPrePostGet < postIndex && postIndex < firstPostPostGet) {
		t.Fatalf("hub request order does not publish before pull: events=%v", events)
	}
	if !report.OK || fixture.readLocal(t, "pi") != "local-pi\n" || fixture.readLocal(t, "pad") != "center-pad\n" {
		t.Fatalf("mixed decisions report/files = %+v pi=%q pad=%q", report, fixture.readLocal(t, "pi"), fixture.readLocal(t, "pad"))
	}
}

type resolutionTaskExecutor struct {
	push commands.PushReport
	pull commands.PullReport
}

func (*resolutionTaskExecutor) Status(context.Context) (commands.StatusReport, error) {
	return commands.StatusReport{Adapters: []commands.StatusAdapterReport{}, Errors: []string{}}, nil
}
func (*resolutionTaskExecutor) Diff(context.Context, web.DiffParams) (string, error) { return "", nil }
func (e *resolutionTaskExecutor) Push(context.Context, bool, []string, bool, bool) (commands.PushReport, error) {
	return e.push, nil
}
func (e *resolutionTaskExecutor) Pull(context.Context, bool, []string, bool) (commands.PullReport, error) {
	return e.pull, nil
}

func newClearGuardTask(t *testing.T, executor Executor) (*Daemon, core.HomerPaths) {
	t.Helper()
	home := t.TempDir()
	paths := homerPathsForHome(home)
	if _, err := resolutions.Record(paths, resolutions.ChoiceCenter, []string{"pi"}, 1, nil); err != nil {
		t.Fatal(err)
	}
	return New(Config{HomerHome: home, AgentID: "clear-immediate"}, executor), paths
}

func TestClearOnSuccess_CenterImmediate(t *testing.T) {
	exec := &resolutionTaskExecutor{pull: commands.PullReport{OK: true, Status: commands.PullStatusApplied}}
	d, paths := newClearGuardTask(t, exec)
	result, err := d.runTaskCommand(context.Background(), &stream.Request{Method: string(hub.TaskKindPull)}, hub.TaskOptions{
		Confirm: true, Adapters: []string{"pi"}, ClearResolutions: true,
	})
	if err != nil {
		t.Fatalf("immediate center pull: %v", err)
	}
	if report, ok := result.(commands.PullReport); !ok || !report.OK {
		t.Fatalf("immediate center report = %#v", result)
	}
	if file, err := resolutions.Load(paths); err != nil || len(file.Entries) != 0 {
		t.Fatalf("successful center pull did not clear decision: %+v, %v", file.Entries, err)
	}
}

func TestClearOnSuccess_Failure(t *testing.T) {
	exec := &resolutionTaskExecutor{pull: commands.PullReport{OK: false, Status: commands.PullStatusError, Errors: []string{"fixture failure"}}}
	d, paths := newClearGuardTask(t, exec)
	result, err := d.runTaskCommand(context.Background(), &stream.Request{Method: string(hub.TaskKindPull)}, hub.TaskOptions{
		Confirm: true, Adapters: []string{"pi"}, ClearResolutions: true,
	})
	if err != nil {
		t.Fatalf("failed immediate center pull: %v", err)
	}
	if report, ok := result.(commands.PullReport); !ok || report.OK {
		t.Fatalf("failed immediate center report = %#v", result)
	}
	if file, err := resolutions.Load(paths); err != nil || len(file.Entries) != 1 {
		t.Fatalf("failed center pull cleared decision: %+v, %v", file.Entries, err)
	}
}

func TestClearOnSuccess_LocalImmediate(t *testing.T) {
	exec := &resolutionTaskExecutor{push: commands.PushReport{OK: true, Status: commands.PushStatusPushed}}
	d, paths := newClearGuardTask(t, exec)
	result, err := d.runTaskCommand(context.Background(), &stream.Request{Method: string(hub.TaskKindPush)}, hub.TaskOptions{
		Confirm: true, Adapters: []string{"pi"}, ClearResolutions: true,
	})
	if err != nil {
		t.Fatalf("immediate local push: %v", err)
	}
	if report, ok := result.(commands.PushReport); !ok || !report.OK {
		t.Fatalf("immediate local report = %#v", result)
	}
	if file, err := resolutions.Load(paths); err != nil || len(file.Entries) != 0 {
		t.Fatalf("successful local push did not clear decision: %+v, %v", file.Entries, err)
	}
}

func TestClearGuardConcurrentRerecordIsPreserved(t *testing.T) {
	exec := &resolutionTaskExecutor{pull: commands.PullReport{OK: true, Status: commands.PullStatusApplied}}
	d, paths := newClearGuardTask(t, exec)
	guard := d.beginClear(hub.TaskOptions{ClearResolutions: true, Adapters: []string{"pi"}})
	newEntry := resolutions.Entry{
		Adapter:            "pi",
		Choice:             resolutions.ChoiceLocal,
		RecordedAt:         time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano),
		GenerationAtRecord: 1,
	}
	if _, err := resolutions.Record(paths, newEntry.Choice, []string{newEntry.Adapter}, newEntry.GenerationAtRecord, func() string { return newEntry.RecordedAt }); err != nil {
		t.Fatal(err)
	}
	if warnings := guard.finish(true); len(warnings) != 0 {
		t.Fatalf("CAS guard warnings = %v", warnings)
	}
	file, err := resolutions.Load(paths)
	if err != nil || len(file.Entries) != 1 || !file.Entries[0].Same(newEntry) {
		t.Fatalf("clear guard removed re-recorded decision: %+v, %v", file.Entries, err)
	}
}

func TestPullApplyingResolutions_LoadErrorFallsBackWithWarning(t *testing.T) {
	fixture := newResolutionExecutorFixture(t, map[string]resolutionAdapterFixture{
		"pi": {Base: "base\n", Local: "local\n", Center: "center\n"},
	})
	if err := os.WriteFile(resolutions.Path(fixture.paths), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := fixture.executor.PullApplyingResolutions(context.Background(), ResolvedPullRequest{
		Confirm: true, Adapters: []string{"pi"}, CenterGeneration: fixture.centerGen,
	})
	if err != nil {
		t.Fatalf("fail-open pull: %v", err)
	}
	if report.Status != commands.PullStatusConflictsRemain || !containsSubstring(report.Warnings, "按普通下发执行") {
		t.Fatalf("corrupt-resolution fallback report = %+v", report)
	}
}

func TestExecutorPullResolvedRequiresLocalExecutor(t *testing.T) {
	d := New(Config{HomerHome: t.TempDir()}, &testExecutor{})
	if _, err := d.executorPullResolved(context.Background(), ResolvedPullRequest{Confirm: true, Adapters: []string{"pi"}}); err == nil {
		t.Fatal("fake executor unexpectedly accepted resolution-aware pull")
	}
}

func TestResolveRecordListMissingReturnsEmpty(t *testing.T) {
	d := New(Config{HomerHome: t.TempDir()}, &testExecutor{})
	report, err := d.runResolveRecord(context.Background(), hub.TaskOptions{ResolutionAction: resolutions.ActionList})
	if err != nil || !report.OK || report.Pending != 0 || report.Entries == nil {
		t.Fatalf("empty list report = %+v, %v", report, err)
	}
}

func TestClearOnSuccessWarningDoesNotFailTask(t *testing.T) {
	home := t.TempDir()
	paths := homerPathsForHome(home)
	if err := os.WriteFile(resolutions.Path(paths), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	exec := &resolutionTaskExecutor{pull: commands.PullReport{OK: true, Status: commands.PullStatusApplied}}
	d := New(Config{HomerHome: home}, exec)
	result, err := d.runTaskCommand(context.Background(), &stream.Request{Method: string(hub.TaskKindPull)}, hub.TaskOptions{
		Confirm: true, Adapters: []string{"pi"}, ClearResolutions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	report := result.(commands.PullReport)
	if !report.OK || !containsSubstring(report.Warnings, "读取待清除决定失败") {
		t.Fatalf("clear snapshot failure report = %+v", report)
	}
}

var _ Executor = (*resolutionTaskExecutor)(nil)
var _ http.Handler = (*resolutionHubFixture)(nil)
