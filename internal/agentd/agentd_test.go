package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/web"
)

func TestConfigModeValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want hub.AgentMode
		ok   bool
	}{
		{name: "listen", cfg: Config{ListenAddr: "127.0.0.1:1"}, want: hub.AgentModeListen, ok: true},
		{name: "connect", cfg: Config{ConnectURL: "http://hub"}, want: hub.AgentModeConnect, ok: true},
		{name: "both", cfg: Config{ListenAddr: ":1", ConnectURL: "http://hub"}},
		{name: "neither", cfg: Config{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, err := tt.cfg.Mode()
			if tt.ok {
				if err != nil || mode != tt.want {
					t.Fatalf("Mode() = %q, %v; want %q", mode, err, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("Mode() = %q, nil; want error", mode)
			}
		})
	}
}

func TestDefaultAgentID(t *testing.T) {
	pattern := regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	first := DefaultAgentID()
	second := DefaultAgentID()
	if first == "" || second == "" || !pattern.MatchString(first) || !pattern.MatchString(second) {
		t.Fatalf("DefaultAgentID() = %q, %q", first, second)
	}
	parts := regexp.MustCompile(`-([0-9a-f]{4})$`).FindStringSubmatch(first)
	if len(parts) != 2 {
		t.Fatalf("DefaultAgentID() = %q, want four hex suffix", first)
	}
}

func TestAgentdListenServesWebAndRegisters(t *testing.T) {
	registry := hub.NewRegistry()
	hubServer := httptest.NewServer(hub.NewAgentAPI(registry, "token"))
	defer hubServer.Close()
	listenAddr := freeTCPAddress(t)
	cfg := Config{
		HomerHome:    t.TempDir(),
		Token:        "token",
		ListenAddr:   listenAddr,
		AdvertiseURL: "http://" + listenAddr,
		HubURL:       hubServer.URL,
		AgentID:      "listen-agent",
	}
	daemon := New(cfg, &recordingExecutor{})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- daemon.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("listen Run() = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("listen daemon did not stop")
		}
	}()

	waitFor(t, time.Second, func() bool {
		info, ok := registry.Get(cfg.AgentID)
		return ok && info.Mode == hub.AgentModeListen && info.Addr == cfg.AdvertiseURL
	})
	infoResponse := getHTTP(t, "http://"+listenAddr+"/agent/v1/info", "token")
	if infoResponse.StatusCode != http.StatusOK {
		t.Fatalf("agent info status = %d; body=%s", infoResponse.StatusCode, infoResponse.Body)
	}
	var infoBody struct {
		OK       bool              `json:"ok"`
		Identity web.AgentIdentity `json:"identity"`
	}
	decodeJSON(t, infoResponse.Body, &infoBody)
	if !infoBody.OK || infoBody.Identity.AgentID != cfg.AgentID || infoBody.Identity.Hostname == "" {
		t.Fatalf("agent info = %+v", infoBody)
	}
	health := getHTTP(t, "http://"+listenAddr+"/api/health", "")
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d; body=%s", health.StatusCode, health.Body)
	}
}

func TestAgentdConnectLoop(t *testing.T) {
	registry := hub.NewRegistry()
	var pollCount atomic.Int32
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/v1/poll" {
			pollCount.Add(1)
		}
		hub.NewAgentAPI(registry, "token").ServeHTTP(w, r)
	})
	hubServer := httptest.NewServer(api)
	defer hubServer.Close()
	exec := newRecordingExecutor()
	cfg := Config{ConnectURL: hubServer.URL, Token: "token", AgentID: "connect-agent", PollWait: time.Second, PollInterval: 10 * time.Millisecond, ReportTimeout: time.Second}
	daemon := New(cfg, exec)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- daemon.Run(ctx) }()
	waitFor(t, time.Second, func() bool {
		_, ok := registry.Get(cfg.AgentID)
		return ok
	})
	task := hub.Task{TaskID: "connect-status", Kind: hub.TaskKindStatus, CreatedAt: time.Now()}
	if err := registry.Enqueue(cfg.AgentID, task); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		result, err := registry.Wait(context.Background(), task.TaskID)
		return err == nil && result.OK && result.Kind == hub.TaskKindStatus
	})
	waitFor(t, time.Second, func() bool { return pollCount.Load() >= 2 })
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("connect Run() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connect daemon did not stop")
	}
}

func TestAgentdDispatchPerKind(t *testing.T) {
	registry := hub.NewRegistry()
	hubServer := httptest.NewServer(hub.NewAgentAPI(registry, "token"))
	defer hubServer.Close()
	exec := newRecordingExecutor()
	cfg := Config{ConnectURL: hubServer.URL, Token: "token", AgentID: "dispatch-agent", PollWait: time.Second, PollInterval: 5 * time.Millisecond, ReportTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- New(cfg, exec).Run(ctx) }()
	waitFor(t, time.Second, func() bool {
		_, ok := registry.Get(cfg.AgentID)
		return ok
	})

	tasks := []hub.Task{
		{TaskID: "dispatch-status", Kind: hub.TaskKindStatus, CreatedAt: time.Now()},
		{TaskID: "dispatch-diff", Kind: hub.TaskKindDiff, Options: hub.TaskOptions{Adapter: "pi", Category: "settings"}, CreatedAt: time.Now()},
		{TaskID: "dispatch-push", Kind: hub.TaskKindPush, Options: hub.TaskOptions{Confirm: true}, CreatedAt: time.Now()},
		{TaskID: "dispatch-pull", Kind: hub.TaskKindPull, Options: hub.TaskOptions{Confirm: true}, CreatedAt: time.Now()},
	}
	for _, task := range tasks {
		if err := registry.Enqueue(cfg.AgentID, task); err != nil {
			t.Fatal(err)
		}
		result, err := registry.Wait(context.Background(), task.TaskID)
		if err != nil || !result.OK {
			t.Fatalf("task %s result = %+v, %v", task.TaskID, result, err)
		}
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch daemon did not stop")
	}
	calls := exec.snapshot()
	// The drift summary (uploaded with each poll, throttled to one full
	// status per DriftInterval) may add leading status calls — filter
	// them out, then require the task sequence to be intact and in order.
	taskCalls := make([]call, 0, len(tasks))
	for _, c := range calls {
		if c.kind == hub.TaskKindStatus {
			continue
		}
		taskCalls = append(taskCalls, c)
	}
	want := []call{
		{kind: hub.TaskKindDiff, adapter: "pi", category: "settings"},
		{kind: hub.TaskKindPush, confirm: true},
		{kind: hub.TaskKindPull, confirm: true},
	}
	if len(taskCalls) != len(want) {
		t.Fatalf("task calls = %+v (all: %+v)", taskCalls, calls)
	}
	for i := range want {
		if taskCalls[i] != want[i] {
			t.Fatalf("call[%d] = %+v, want %+v", i, taskCalls[i], want[i])
		}
	}
}

func TestAgentdExecutorError(t *testing.T) {
	registry := hub.NewRegistry()
	hubServer := httptest.NewServer(hub.NewAgentAPI(registry, "token"))
	defer hubServer.Close()
	exec := newRecordingExecutor()
	exec.statusErr = errors.New("status failed")
	cfg := Config{ConnectURL: hubServer.URL, Token: "token", AgentID: "error-agent", PollWait: time.Second, PollInterval: 5 * time.Millisecond, ReportTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- New(cfg, exec).Run(ctx) }()
	waitFor(t, time.Second, func() bool { _, ok := registry.Get(cfg.AgentID); return ok })
	task := hub.Task{TaskID: "error-status", Kind: hub.TaskKindStatus, CreatedAt: time.Now()}
	if err := registry.Enqueue(cfg.AgentID, task); err != nil {
		t.Fatal(err)
	}
	result, err := registry.Wait(context.Background(), task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Error != "status failed" {
		t.Fatalf("result = %+v", result)
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("error daemon did not stop")
	}
}

func TestAgentdReportTimeout(t *testing.T) {
	registry := hub.NewRegistry()
	hubServer := httptest.NewServer(hub.NewAgentAPI(registry, "token"))
	defer hubServer.Close()
	exec := newRecordingExecutor()
	exec.blockStatus = true
	cfg := Config{ConnectURL: hubServer.URL, Token: "token", AgentID: "timeout-agent", PollWait: time.Second, PollInterval: 5 * time.Millisecond, ReportTimeout: 30 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- New(cfg, exec).Run(ctx) }()
	waitFor(t, time.Second, func() bool { _, ok := registry.Get(cfg.AgentID); return ok })
	task := hub.Task{TaskID: "timeout-status", Kind: hub.TaskKindStatus, CreatedAt: time.Now()}
	if err := registry.Enqueue(cfg.AgentID, task); err != nil {
		t.Fatal(err)
	}
	result, err := registry.Wait(context.Background(), task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || !regexp.MustCompile(`timeout`).MatchString(result.Error) {
		t.Fatalf("timeout result = %+v", result)
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout daemon did not stop")
	}
	// The blocked executor goroutine releases once its task context is
	// cancelled (recordingExecutor waits on ctx.Done). Assert the daemon
	// settles back to its baseline goroutine count: plan W-B2 requires
	// "no goroutine leak (runtime.NumGoroutine within ±small delta)".
	baseline := runtime.NumGoroutine()
	leaked := true
	for attempt := 0; attempt < 50; attempt++ {
		if runtime.NumGoroutine() <= baseline+2 {
			leaked = false
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if leaked {
		t.Fatalf("goroutines leaked after timeout: baseline=%d now=%d", baseline, runtime.NumGoroutine())
	}
}

func TestAgentdCtxCancel(t *testing.T) {
	registry := hub.NewRegistry()
	hubServer := httptest.NewServer(hub.NewAgentAPI(registry, "token"))
	defer hubServer.Close()
	cfg := Config{ConnectURL: hubServer.URL, Token: "token", AgentID: "cancel-agent", PollWait: 10 * time.Second, PollInterval: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- New(cfg, newRecordingExecutor()).Run(ctx) }()
	waitFor(t, time.Second, func() bool { _, ok := registry.Get(cfg.AgentID); return ok })
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() after cancel = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled daemon did not stop")
	}
}

func TestLocalExecutor(t *testing.T) {
	home := localExecutorFixture(t)
	want, err := commands.RunStatus(commands.StatusOptions{HomerHome: home})
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewLocalExecutor(home).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Status() = %+v, want %+v", got, want)
	}
}

type call struct {
	kind     hub.TaskKind
	adapter  string
	category string
	confirm  bool
}

type recordingExecutor struct {
	mu          sync.Mutex
	calls       []call
	statusErr   error
	blockStatus bool
}

func newRecordingExecutor() *recordingExecutor { return &recordingExecutor{} }

func (e *recordingExecutor) Status(ctx context.Context) (commands.StatusReport, error) {
	e.record(call{kind: hub.TaskKindStatus})
	if e.blockStatus {
		<-ctx.Done()
	}
	if e.statusErr != nil {
		return commands.StatusReport{}, e.statusErr
	}
	return commands.StatusReport{Adapters: []commands.StatusAdapterReport{{ID: "pi"}}, Errors: []string{}}, nil
}

func (e *recordingExecutor) Diff(ctx context.Context, params web.DiffParams) (string, error) {
	e.record(call{kind: hub.TaskKindDiff, adapter: params.Adapter, category: params.Category})
	return "diff", nil
}

func (e *recordingExecutor) Push(ctx context.Context, confirm bool) (commands.PushReport, error) {
	e.record(call{kind: hub.TaskKindPush, confirm: confirm})
	return commands.PushReport{OK: true, Status: commands.PushStatusPushed}, nil
}

func (e *recordingExecutor) Pull(ctx context.Context, confirm bool) (commands.PullReport, error) {
	e.record(call{kind: hub.TaskKindPull, confirm: confirm})
	return commands.PullReport{OK: true, Status: commands.PullStatusApplied}, nil
}

func (e *recordingExecutor) record(item call) {
	e.mu.Lock()
	e.calls = append(e.calls, item)
	e.mu.Unlock()
}

func (e *recordingExecutor) snapshot() []call {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]call(nil), e.calls...)
}

func freeTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

type httpResponse struct {
	StatusCode int
	Body       []byte
}

func getHTTP(t *testing.T, endpoint, token string) httpResponse {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpResponse{StatusCode: response.StatusCode, Body: body}
}

func decodeJSON(t *testing.T, body []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func localExecutorFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return os.Getenv(key)
	})
	tool := filepath.Join(home, "tool")
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: tool, Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
		}},
	}}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool, "settings.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := core.WriteSnapshotToStore(paths, core.AdapterSnapshot{AdapterID: "pi", Categories: []core.CategorySnapshot{{AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{"settings.json": {Kind: "file", Content: `{"ok":true}`}}}}}); err != nil {
		t.Fatal(err)
	}
	return home
}

// No-git data plane wiring: an executor built with a hub transport uploads
// snapshots on push and downloads them on pull — over the same HTTP
// channel the daemon already uses (endpoint + credential injected).
func TestLocalExecutorHubTransport(t *testing.T) {
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

	// A stub hub: /api/snapshot download serves one file, upload records.
	var uploaded []core.AdapterSnapshot
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/snapshot":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"generation":3,"homerJson":"","store":{"pi":{"settings/settings.json":"from-hub\n"}}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/snapshot":
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				Store map[string]map[string]string `json:"store"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			uploaded = append(uploaded, core.AdapterSnapshot{AdapterID: "pi"})
			fmt.Fprint(w, `{"ok":true,"generation":4}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hub.Close()

	executor := NewLocalExecutorWithHub(home, hub.URL, "")
	ctx := context.Background()

	// Pull: the hub's current generation lands in the tool directory.
	pullReport, err := executor.Pull(ctx, true)
	if err != nil || !pullReport.OK || pullReport.Status != commands.PullStatusApplied {
		t.Fatalf("pull = %#v err=%v", pullReport, err)
	}
	applied, _ := os.ReadFile(filepath.Join(tool, "settings.json"))
	if string(applied) != "from-hub\n" {
		t.Fatalf("tool = %q (want hub content)", applied)
	}

	// Push: a local change uploads to the hub.
	if err := os.WriteFile(filepath.Join(tool, "settings.json"), []byte("next-change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pushReport, err := executor.Push(ctx, true)
	if err != nil || !pushReport.OK || pushReport.Status != commands.PushStatusPushed {
		t.Fatalf("push = %#v err=%v", pushReport, err)
	}
	if len(uploaded) == 0 {
		t.Fatal("hub received no upload")
	}
}
