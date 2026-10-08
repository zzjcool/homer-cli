package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/toolctl"
	"github.com/zzjcool/homer-cli/internal/web"
)

func toolStatus(id, version string) toolctl.Status {
	return toolctl.Status{ID: id, Adapter: id, Label: strings.ToUpper(id), Version: version, Upgradable: true}
}

func TestNormalizeToolsKeepsNilAndEmptyApart(t *testing.T) {
	if got := normalizeTools(nil); got != nil {
		t.Fatalf("nil list became %#v; \"not reported\" must stay distinguishable", got)
	}
	got := normalizeTools([]toolctl.Status{})
	if got == nil || len(got) != 0 {
		t.Fatalf("empty list = %#v, want an empty non-nil list (\"nothing installed\")", got)
	}
}

func TestNormalizeToolsCleansWhatAnAgentSends(t *testing.T) {
	long := strings.Repeat("9", 500)
	in := []toolctl.Status{
		{ID: "  pi\x1b[31m  ", Adapter: "pi", Label: "pi\x00", Version: "0.87.1", Upgradable: true},
		{ID: "pi", Version: "1.0.0"}, // repeated ID: the first one wins
		{ID: "", Version: "2.0.0"},   // no ID: nothing to show or upgrade
		{ID: "herdr", Version: long, Error: strings.Repeat("e", 1000)},
	}
	got := normalizeTools(in)
	if len(got) != 2 {
		t.Fatalf("normalized = %+v, want pi and herdr only", got)
	}
	if got[0].ID != "pi" || got[0].Version != "0.87.1" || got[0].Label != "pi" || !got[0].Upgradable {
		t.Fatalf("first entry = %+v", got[0])
	}
	if got[1].ID != "herdr" || len([]rune(got[1].Version)) != maxToolText || len([]rune(got[1].Error)) != maxToolErr {
		t.Fatalf("second entry was not clipped: version=%d error=%d", len(got[1].Version), len(got[1].Error))
	}
	if in[0].ID != "  pi\x1b[31m  " {
		t.Fatal("normalizing must not edit the caller's list")
	}
}

func TestNormalizeToolsCapsTheListLength(t *testing.T) {
	var in []toolctl.Status
	for i := 0; i < maxTools*3; i++ {
		in = append(in, toolStatus(fmt.Sprintf("tool-%02d", i), "1.0.0"))
	}
	if got := normalizeTools(in); len(got) != maxTools {
		t.Fatalf("kept %d tools, want %d", len(got), maxTools)
	}
}

func TestRegisterKeepsToolsWhenOmitted(t *testing.T) {
	r := NewRegistry()
	first := registryTestAgent("agent-a")
	first.Tools = []toolctl.Status{toolStatus("pi", "0.87.1")}
	if err := r.Register(first); err != nil {
		t.Fatal(err)
	}
	// A listen agent's heartbeat that says nothing about tools must not wipe
	// what the last full report said.
	if err := r.Register(registryTestAgent("agent-a")); err != nil {
		t.Fatal(err)
	}
	info, _ := r.Get("agent-a")
	if len(info.Tools) != 1 || info.Tools[0].Version != "0.87.1" {
		t.Fatalf("tools after a silent heartbeat = %+v", info.Tools)
	}
}

func TestRegisterWithAnEmptyListClearsTools(t *testing.T) {
	r := NewRegistry()
	first := registryTestAgent("agent-a")
	first.Tools = []toolctl.Status{toolStatus("pi", "0.87.1")}
	if err := r.Register(first); err != nil {
		t.Fatal(err)
	}
	// The user uninstalled pi: the machine now says "nothing", and that is a
	// statement, not an omission.
	second := registryTestAgent("agent-a")
	second.Tools = []toolctl.Status{}
	if err := r.Register(second); err != nil {
		t.Fatal(err)
	}
	info, _ := r.Get("agent-a")
	if len(info.Tools) != 0 {
		t.Fatalf("tools after an explicit empty report = %+v", info.Tools)
	}
}

func TestUpdateToolsReplacesTheList(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(registryTestAgent("agent-a")); err != nil {
		t.Fatal(err)
	}
	r.UpdateTools("agent-a", []toolctl.Status{toolStatus("pi", "0.87.1"), toolStatus("herdr", "0.9.1")})
	r.UpdateTools("agent-a", []toolctl.Status{toolStatus("pi", "0.90.2")})
	info, _ := r.Get("agent-a")
	if len(info.Tools) != 1 || info.Tools[0].ID != "pi" || info.Tools[0].Version != "0.90.2" {
		t.Fatalf("tools = %+v, want only pi 0.90.2", info.Tools)
	}
	r.UpdateTools("agent-a", nil)
	info, _ = r.Get("agent-a")
	if info.Tools == nil || len(info.Tools) != 0 {
		t.Fatalf("tools after UpdateTools(nil) = %#v, want a stored empty list", info.Tools)
	}
	r.UpdateTools("nobody", []toolctl.Status{toolStatus("pi", "1.0.0")}) // must not panic
}

func TestRegistryHandsOutCopiesOfTools(t *testing.T) {
	r := NewRegistry()
	agent := registryTestAgent("agent-a")
	agent.Tools = []toolctl.Status{toolStatus("pi", "0.87.1")}
	if err := r.Register(agent); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("agent-a")
	got.Tools[0].Version = "tampered"
	listed := r.List()
	listed[0].Tools[0].ID = "tampered"
	again, _ := r.Get("agent-a")
	if again.Tools[0].Version != "0.87.1" || again.Tools[0].ID != "pi" {
		t.Fatalf("a caller edited the registry through a copy: %+v", again.Tools)
	}
	agent.Tools[0].Version = "tampered-after-register"
	again, _ = r.Get("agent-a")
	if again.Tools[0].Version != "0.87.1" {
		t.Fatalf("registry kept the caller's slice: %+v", again.Tools)
	}
}

func TestNoteToolVersion(t *testing.T) {
	r := NewRegistry()
	agent := registryTestAgent("agent-a")
	agent.Tools = []toolctl.Status{
		{ID: "pi", Version: "0.80.0", Error: "读不出版本：旧的错误"},
		toolStatus("herdr", "0.9.1"),
	}
	if err := r.Register(agent); err != nil {
		t.Fatal(err)
	}
	r.NoteToolVersion("agent-a", "pi", " 0.90.2 ")
	info, _ := r.Get("agent-a")
	if info.Tools[0].Version != "0.90.2" || info.Tools[0].Error != "" {
		t.Fatalf("pi = %+v, want the new version and no stale error", info.Tools[0])
	}
	if info.Tools[1].Version != "0.9.1" {
		t.Fatalf("herdr changed too: %+v", info.Tools[1])
	}

	r.NoteToolVersion("agent-a", "opencode", "9.9.9") // not on this machine: no entry invented
	r.NoteToolVersion("agent-a", "pi", "")            // nothing measured: nothing recorded
	r.NoteToolVersion("nobody", "pi", "1.0.0")        // unknown machine
	var nilRegistry *Registry
	nilRegistry.NoteToolVersion("agent-a", "pi", "1.0.0") // must not panic
	info, _ = r.Get("agent-a")
	if len(info.Tools) != 2 || info.Tools[0].Version != "0.90.2" {
		t.Fatalf("tools after the no-op notes = %+v", info.Tools)
	}
}

func TestTaskLifetimePerKind(t *testing.T) {
	cases := map[TaskKind]time.Duration{
		TaskKindStatus:      TaskTTL,
		TaskKindDiff:        TaskTTL,
		TaskKindPush:        TaskTTL,
		TaskKindSSHKey:      TaskTTL,
		TaskKindSecret:      TaskTTL,
		TaskKindPull:        PullWait,
		TaskKindUpgrade:     UpgradeWait,
		TaskKindToolUpgrade: ToolUpgradeWait,
		TaskKind("unknown"): TaskTTL,
	}
	for kind, want := range cases {
		if got := TaskLifetime(kind); got != want {
			t.Errorf("TaskLifetime(%q) = %v, want %v", kind, got, want)
		}
	}
	// The dispatcher waits this long; the registry must not give up sooner.
	if ToolUpgradeWait <= toolctl.UpgradeTimeout {
		t.Fatalf("ToolUpgradeWait %v does not cover an upgrade command of %v", ToolUpgradeWait, toolctl.UpgradeTimeout)
	}
}

var (
	longKinds    = []TaskKind{TaskKindPull, TaskKindUpgrade, TaskKindToolUpgrade}
	ordinaryKind = []TaskKind{TaskKindStatus, TaskKindDiff, TaskKindPush, TaskKindSSHKey, TaskKindSecret}
)

// withQueueTTL shortens how long a task may wait to be taken. The tests that
// use it must not run in parallel.
func withQueueTTL(t *testing.T, ttl time.Duration) {
	t.Helper()
	previous := taskQueueTTL
	taskQueueTTL = ttl
	t.Cleanup(func() { taskQueueTTL = previous })
}

// takenAgo pretends the machine took the task that long ago.
func takenAgo(r *Registry, taskID string, ago time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks[taskID].claimedAt = time.Now().Add(-ago)
}

// queuedAgo pretends the task was queued that long ago.
func queuedAgo(r *Registry, taskID string, ago time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks[taskID].task.CreatedAt = time.Now().Add(-ago)
}

func takeTask(t *testing.T, r *Registry, kind TaskKind) Task {
	t.Helper()
	if err := r.Register(registryTestAgent("agent-a")); err != nil {
		t.Fatal(err)
	}
	task := Task{TaskID: "task-" + string(kind), Kind: kind, CreatedAt: time.Now()}
	if err := r.Enqueue("agent-a", task); err != nil {
		t.Fatalf("enqueue %s: %v", kind, err)
	}
	got, ok := r.Poll("agent-a", 0, context.Background())
	if !ok || got.TaskID != task.TaskID {
		t.Fatalf("Poll() = (%+v, %v), want the %s task", got, ok, kind)
	}
	return task
}

// A task the dispatcher is willing to wait minutes for must not be dropped at
// the ordinary limit once its machine is working on it, with the result the
// machine eventually reports thrown away.
func TestLongTasksOutliveTheOrdinaryTTLOnceTaken(t *testing.T) {
	for _, kind := range longKinds {
		t.Run(string(kind), func(t *testing.T) {
			r := NewRegistry()
			task := takeTask(t, r, kind)
			queuedAgo(r, task.TaskID, TaskTTL+5*time.Second)
			takenAgo(r, task.TaskID, TaskTTL+3*time.Second)
			if err := r.Submit(TaskResult{TaskID: task.TaskID, AgentID: "agent-a", Kind: kind, OK: true, Report: json.RawMessage(`{"ok":true}`)}); err != nil {
				t.Fatalf("Submit: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, err := r.Wait(ctx, task.TaskID)
			if err != nil || string(result.Report) != `{"ok":true}` {
				t.Fatalf("Wait = %+v, %v", result, err)
			}
		})
	}
}

func TestLongTasksStillExpireAtTheirOwnLimitOnceTaken(t *testing.T) {
	for _, kind := range longKinds {
		t.Run(string(kind), func(t *testing.T) {
			r := NewRegistry()
			task := takeTask(t, r, kind)
			queuedAgo(r, task.TaskID, TaskLifetime(kind)+2*time.Second)
			takenAgo(r, task.TaskID, TaskLifetime(kind)+time.Second)
			if err := r.Submit(TaskResult{TaskID: task.TaskID, AgentID: "agent-a", Kind: kind, OK: true, Report: json.RawMessage(`{"ok":true}`)}); err == nil {
				t.Fatal("Submit accepted a result after the kind's lifetime")
			}
			if _, err := r.Wait(context.Background(), task.TaskID); err == nil {
				t.Fatal("Wait returned for a task that outlived its lifetime")
			}
		})
	}
}

// The other kinds keep the limit they have always had, counted from being
// queued, taken or not.
func TestOrdinaryTasksKeepTheirLimitOnceTaken(t *testing.T) {
	for _, kind := range ordinaryKind {
		t.Run(string(kind), func(t *testing.T) {
			r := NewRegistry()
			task := takeTask(t, r, kind)
			queuedAgo(r, task.TaskID, TaskTTL+time.Second)
			takenAgo(r, task.TaskID, time.Second)
			if err := r.Submit(TaskResult{TaskID: task.TaskID, AgentID: "agent-a", Kind: kind, OK: true, Report: json.RawMessage(`{"ok":true}`)}); err == nil {
				t.Fatal("Submit accepted a result for an ordinary task that outlived TaskTTL")
			}
		})
	}
}

// A task nobody has taken lives TaskTTL, whatever its kind. This is what lets
// a caller learn in two minutes, not twelve, that the machine is gone.
func TestQueuedTasksOfEveryKindExpireAtTheQueueLimit(t *testing.T) {
	for _, kind := range append(append([]TaskKind{}, longKinds...), ordinaryKind...) {
		t.Run(string(kind), func(t *testing.T) {
			r := NewRegistry()
			if err := r.Register(registryTestAgent("agent-a")); err != nil {
				t.Fatal(err)
			}
			task := Task{TaskID: "queued-" + string(kind), Kind: kind, CreatedAt: time.Now().Add(-TaskTTL - time.Second)}
			if err := r.Enqueue("agent-a", task); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			if got, ok := r.Poll("agent-a", 0, context.Background()); ok {
				t.Fatalf("Poll() delivered a task that waited past the queue limit: %+v", got)
			}
			if _, err := r.Wait(context.Background(), task.TaskID); err == nil {
				t.Fatal("Wait returned for a task that waited past the queue limit")
			}
		})
	}
}

// The case that hung the suite: a long task queued for a machine that never
// polls. Waiting for it must end at the queue limit.
func TestWaitForATaskNobodyTakesEndsAtTheQueueLimit(t *testing.T) {
	withQueueTTL(t, 300*time.Millisecond)
	for _, kind := range longKinds {
		t.Run(string(kind), func(t *testing.T) {
			r := NewRegistry()
			if err := r.Register(registryTestAgent("agent-a")); err != nil {
				t.Fatal(err)
			}
			task := Task{TaskID: "nobody-" + string(kind), Kind: kind, CreatedAt: time.Now()}
			if err := r.Enqueue("agent-a", task); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started := time.Now()
			_, err := r.Wait(ctx, task.TaskID)
			if err == nil || !strings.Contains(err.Error(), "expired") {
				t.Fatalf("Wait = %v, want the task to expire", err)
			}
			if ctx.Err() != nil || time.Since(started) > 3*time.Second {
				t.Fatalf("Wait ran for %v and was ended by the caller's deadline, not the queue limit", time.Since(started))
			}
		})
	}
}

// A waiter that started while the task was queued must notice that the machine
// took it, and keep waiting for as long as the kind may run.
func TestWaitFollowsATaskThatIsTaken(t *testing.T) {
	withQueueTTL(t, 300*time.Millisecond)

	type outcome struct {
		result TaskResult
		err    error
	}
	run := func(t *testing.T, kind TaskKind) (outcome, error) {
		t.Helper()
		r := NewRegistry()
		if err := r.Register(registryTestAgent("agent-a")); err != nil {
			t.Fatal(err)
		}
		task := Task{TaskID: "followed-" + string(kind), Kind: kind, CreatedAt: time.Now()}
		if err := r.Enqueue("agent-a", task); err != nil {
			t.Fatal(err)
		}
		waited := make(chan outcome, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := r.Wait(ctx, task.TaskID)
			waited <- outcome{result, err}
		}()
		time.Sleep(100 * time.Millisecond)
		if _, ok := r.Poll("agent-a", 0, context.Background()); !ok {
			t.Fatal("the machine could not take the task")
		}
		time.Sleep(600 * time.Millisecond) // well past the queue limit
		submitErr := r.Submit(TaskResult{TaskID: task.TaskID, AgentID: "agent-a", Kind: kind, OK: true, Report: json.RawMessage(`{"ok":true}`)})
		select {
		case got := <-waited:
			return got, submitErr
		case <-time.After(5 * time.Second):
			t.Fatal("Wait did not return")
			return outcome{}, nil
		}
	}

	for _, kind := range longKinds {
		t.Run(string(kind), func(t *testing.T) {
			got, submitErr := run(t, kind)
			if submitErr != nil || got.err != nil || string(got.result.Report) != `{"ok":true}` {
				t.Fatalf("a long task that was taken in time: Submit = %v, Wait = %+v, %v", submitErr, got.result, got.err)
			}
		})
	}
	t.Run("ordinary kinds keep the old limit", func(t *testing.T) {
		got, submitErr := run(t, TaskKindStatus)
		if submitErr == nil || got.err == nil {
			t.Fatalf("an ordinary task that ran past its limit: Submit = %v, Wait = %+v, %v", submitErr, got.result, got.err)
		}
	})
}

// The dispatcher is willing to wait minutes for these, but a machine that
// never takes the task is a machine that is gone.
func TestLongRequestsToAMachineThatNeverPollsEndAtTheQueueLimit(t *testing.T) {
	withQueueTTL(t, 300*time.Millisecond)
	calls := map[string]func(*Dispatcher, context.Context) error{
		"pull": func(d *Dispatcher, ctx context.Context) error {
			_, err := d.AgentPull(ctx, "gone", true, web.SyncScope{})
			return err
		},
		"upgrade": func(d *Dispatcher, ctx context.Context) error {
			_, err := d.AgentUpgrade(ctx, "gone")
			return err
		},
		"tool-upgrade": func(d *Dispatcher, ctx context.Context) error {
			_, err := d.AgentToolUpgrade(ctx, "gone", "pi")
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			registry := NewRegistry()
			if err := registry.Register(AgentInfo{AgentID: "gone", Hostname: "gone", Mode: AgentModeConnect, LastSeen: time.Now()}); err != nil {
				t.Fatal(err)
			}
			dispatcher := NewDispatcher(registry, "")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started := time.Now()
			err := call(dispatcher, ctx)
			if err == nil || !hasAgentCode(err, "agent-timeout", http.StatusGatewayTimeout) {
				t.Fatalf("error = %v, want agent-timeout", err)
			}
			if ctx.Err() != nil || time.Since(started) > 3*time.Second {
				t.Fatalf("the call ran for %v: it was ended by the caller's deadline, not the queue limit", time.Since(started))
			}
		})
	}
}

func TestAgentAPIStoresTools(t *testing.T) {
	registry := NewRegistry()
	server := httptest.NewServer(NewAgentAPI(registry, "token"))
	defer server.Close()

	registered := postAgentJSON(t, server.URL+"/agent/v1/register", "token", map[string]any{
		"agentId": "agent-a", "hostname": "box-a", "mode": "connect", "version": "v1",
		"tools": []any{
			map[string]any{"id": "pi", "adapter": "pi", "label": "pi", "version": "0.87.1", "upgradable": true},
			map[string]any{"id": "herdr", "adapter": "herdr", "label": "herdr", "version": "0.9.1", "upgradable": true},
		},
	})
	if registered.StatusCode != http.StatusOK {
		t.Fatalf("register = %d %s", registered.StatusCode, registered.Body)
	}
	info, _ := registry.Get("agent-a")
	if len(info.Tools) != 2 || info.Tools[0].ID != "pi" || info.Tools[0].Version != "0.87.1" || !info.Tools[0].Upgradable {
		t.Fatalf("registered tools = %+v", info.Tools)
	}

	// A poll that says nothing about tools keeps them.
	if polled := postAgentJSON(t, server.URL+"/agent/v1/poll", "token", map[string]any{"agentId": "agent-a", "waitSeconds": 0}); polled.StatusCode != http.StatusOK {
		t.Fatalf("quiet poll = %d %s", polled.StatusCode, polled.Body)
	}
	info, _ = registry.Get("agent-a")
	if len(info.Tools) != 2 {
		t.Fatalf("a poll without tools changed them: %+v", info.Tools)
	}

	// A poll with tools replaces them.
	if polled := postAgentJSON(t, server.URL+"/agent/v1/poll", "token", map[string]any{
		"agentId": "agent-a", "waitSeconds": 0,
		"tools": []any{map[string]any{"id": "pi", "label": "pi", "version": "0.90.2"}},
	}); polled.StatusCode != http.StatusOK {
		t.Fatalf("tools poll = %d %s", polled.StatusCode, polled.Body)
	}
	info, _ = registry.Get("agent-a")
	if len(info.Tools) != 1 || info.Tools[0].Version != "0.90.2" {
		t.Fatalf("a poll with tools did not replace them: %+v", info.Tools)
	}

	// An explicit empty list means everything was uninstalled.
	if polled := postAgentJSON(t, server.URL+"/agent/v1/poll", "token", map[string]any{
		"agentId": "agent-a", "waitSeconds": 0, "tools": []any{},
	}); polled.StatusCode != http.StatusOK {
		t.Fatalf("empty tools poll = %d %s", polled.StatusCode, polled.Body)
	}
	info, _ = registry.Get("agent-a")
	if len(info.Tools) != 0 {
		t.Fatalf("an empty report did not clear the tools: %+v", info.Tools)
	}

	// A heartbeat register that omits them (an older agent) keeps what is known.
	postAgentJSON(t, server.URL+"/agent/v1/poll", "token", map[string]any{
		"agentId": "agent-a", "waitSeconds": 0, "tools": []any{map[string]any{"id": "pi", "version": "0.90.2"}},
	})
	if again := postAgentJSON(t, server.URL+"/agent/v1/register", "token", map[string]any{
		"agentId": "agent-a", "hostname": "box-a", "mode": "connect", "version": "v1",
	}); again.StatusCode != http.StatusOK {
		t.Fatalf("re-register = %d %s", again.StatusCode, again.Body)
	}
	info, _ = registry.Get("agent-a")
	if len(info.Tools) != 1 {
		t.Fatalf("a register without tools wiped them: %+v", info.Tools)
	}
}

func TestAgentAPIEnrollStoresTools(t *testing.T) {
	registry := NewRegistry()
	api := NewAgentAPI(registry, "hub-token")
	server := httptest.NewServer(api)
	defer server.Close()
	code, err := api.Enrollment.Mint(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	enrolled := postAgentJSON(t, server.URL+"/agent/v1/enroll", "", map[string]any{
		"code": code, "agentId": "agent-e", "hostname": "box", "mode": "connect", "version": "dev",
		"tools": []any{map[string]any{"id": "pi", "label": "pi", "version": "0.87.1"}},
	})
	if enrolled.StatusCode != http.StatusOK {
		t.Fatalf("enroll = %d %s", enrolled.StatusCode, enrolled.Body)
	}
	info, _ := registry.Get("agent-e")
	if len(info.Tools) != 1 || info.Tools[0].Version != "0.87.1" {
		t.Fatalf("enroll tools = %+v", info.Tools)
	}
}

func TestDispatcherListAgentsMapsTools(t *testing.T) {
	registry := NewRegistry()
	agent := registryTestAgent("agent-a")
	agent.Tools = []toolctl.Status{toolStatus("pi", "0.87.1")}
	if err := registry.Register(agent); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(registryTestAgent("agent-b")); err != nil {
		t.Fatal(err)
	}
	listed := NewDispatcher(registry, "").ListAgents()
	if len(listed) != 2 {
		t.Fatalf("listed %d agents", len(listed))
	}
	if len(listed[0].Tools) != 1 || listed[0].Tools[0].ID != "pi" || listed[0].Tools[0].Version != "0.87.1" || !listed[0].Tools[0].Upgradable {
		t.Fatalf("agent-a tools = %+v", listed[0].Tools)
	}
	if len(listed[1].Tools) != 0 {
		t.Fatalf("agent-b has no tools but listed %+v", listed[1].Tools)
	}
}

func upgradeReportJSON(tool, before, after string) string {
	return fmt.Sprintf(`{"ok":true,"status":"upgraded","tool":%q,"label":%q,"before":%q,"after":%q,"note":"%s %s → %s"}`, tool, tool, before, after, tool, before, after)
}

func TestAgentToolUpgradeConnect(t *testing.T) {
	registry := NewRegistry()
	agent := registryTestAgent("agent-a")
	agent.Tools = []toolctl.Status{toolStatus("pi", "0.80.0"), toolStatus("herdr", "0.9.1")}
	if err := registry.Register(agent); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, "")
	type outcome struct {
		report json.RawMessage
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		report, err := dispatcher.AgentToolUpgrade(context.Background(), "agent-a", "pi")
		done <- outcome{report, err}
	}()

	task, ok := registry.Poll("agent-a", time.Second, context.Background())
	if !ok || task.Kind != TaskKindToolUpgrade || task.Options.Tool != "pi" {
		t.Fatalf("task = %+v ok=%v, want a tool-upgrade task naming pi", task, ok)
	}
	report := upgradeReportJSON("pi", "0.80.0", "0.90.2")
	if err := registry.Submit(TaskResult{TaskID: task.TaskID, AgentID: "agent-a", Kind: task.Kind, OK: true, Report: json.RawMessage(report)}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || string(got.report) != report {
			t.Fatalf("result = %s, %v", got.report, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AgentToolUpgrade never returned")
	}
	info, _ := registry.Get("agent-a")
	if info.Tools[0].Version != "0.90.2" {
		t.Fatalf("pi version after the upgrade = %q, want it recorded at once", info.Tools[0].Version)
	}
	if info.Tools[1].Version != "0.9.1" {
		t.Fatalf("herdr changed: %+v", info.Tools[1])
	}
}

// A machine that tried and failed still answers; its note and output are
// what the person needs to see, so they must reach the web layer unchanged.
func TestAgentToolUpgradeConnectRelaysAFailureReport(t *testing.T) {
	registry := NewRegistry()
	agent := registryTestAgent("agent-a")
	agent.Tools = []toolctl.Status{toolStatus("pi", "0.80.0")}
	if err := registry.Register(agent); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, "")
	done := make(chan struct {
		report json.RawMessage
		err    error
	}, 1)
	go func() {
		report, err := dispatcher.AgentToolUpgrade(context.Background(), "agent-a", "pi")
		done <- struct {
			report json.RawMessage
			err    error
		}{report, err}
	}()
	task, ok := registry.Poll("agent-a", time.Second, context.Background())
	if !ok {
		t.Fatal("no task delivered")
	}
	failure := `{"ok":false,"status":"failed","tool":"pi","before":"0.80.0","note":"pi 升级没有成功：exit status 1","output":"npm ERR! network","manual":"curl -fsSL https://pi.dev/install.sh | sh"}`
	if err := registry.Submit(TaskResult{TaskID: task.TaskID, AgentID: "agent-a", Kind: task.Kind, OK: false, Report: json.RawMessage(failure)}); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || string(got.report) != failure {
		t.Fatalf("failure report = %s, %v; want it relayed as is", got.report, got.err)
	}
	info, _ := registry.Get("agent-a")
	if info.Tools[0].Version != "0.80.0" {
		t.Fatalf("a failed upgrade changed the recorded version to %q", info.Tools[0].Version)
	}
}

func TestAgentToolUpgradeListen(t *testing.T) {
	const token = "dial-token"
	bodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/tools/upgrade" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		bodies <- string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upgradeReportJSON("pi", "0.80.0", "0.90.2")))
	}))
	defer server.Close()

	registry := NewRegistry()
	agent := AgentInfo{AgentID: "listen-a", Hostname: "box", Mode: AgentModeListen, Addr: server.URL,
		Tools: []toolctl.Status{toolStatus("pi", "0.80.0")}}
	if err := registry.Register(agent); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, token)
	report, err := dispatcher.AgentToolUpgrade(context.Background(), "listen-a", "pi")
	if err != nil {
		t.Fatalf("AgentToolUpgrade: %v", err)
	}
	if gotBody := <-bodies; gotBody != `{"tool":"pi"}` {
		t.Fatalf("body sent to the machine = %s, want only the tool name", gotBody)
	}
	if !strings.Contains(string(report), `"after":"0.90.2"`) {
		t.Fatalf("report = %s", report)
	}
	info, _ := registry.Get("listen-a")
	if info.Tools[0].Version != "0.90.2" {
		t.Fatalf("pi version after the upgrade = %q", info.Tools[0].Version)
	}
}

// Everything else the dispatcher asks a listen machine gives up after a
// minute. An upgrade is allowed to take minutes, so it must not share that
// overall timeout.
func TestAgentToolUpgradeListenOutlivesTheOrdinaryTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/tools/upgrade" {
			_, _ = w.Write([]byte(upgradeReportJSON("pi", "0.80.0", "0.90.2")))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"report":{}}`))
	}))
	defer server.Close()

	registry := NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "listen-a", Mode: AgentModeListen, Addr: server.URL}); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, "")
	dispatcher.Client = &http.Client{Timeout: 100 * time.Millisecond}

	// The control: a status request on the same client does time out.
	if _, err := dispatcher.AgentStatus(context.Background(), "listen-a"); err == nil || !hasAgentCode(err, "agent-timeout", http.StatusGatewayTimeout) {
		t.Fatalf("control AgentStatus error = %v, want the short client to time out", err)
	}
	report, err := dispatcher.AgentToolUpgrade(context.Background(), "listen-a", "pi")
	if err != nil || !strings.Contains(string(report), `"after":"0.90.2"`) {
		t.Fatalf("AgentToolUpgrade = %s, %v; the short client timeout must not apply", report, err)
	}
}

func TestAgentToolUpgradeListenHonoursTheCallersContext(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	registry := NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "listen-a", Mode: AgentModeListen, Addr: server.URL}); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, "")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := dispatcher.AgentToolUpgrade(ctx, "listen-a", "pi")
	if err == nil || !hasAgentCode(err, "agent-timeout", http.StatusGatewayTimeout) {
		t.Fatalf("error = %v, want agent-timeout once the caller gives up", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatalf("returned after %v; the caller's deadline was ignored", time.Since(started))
	}
}

func TestAgentToolUpgradeRefusals(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(AgentInfo{AgentID: "gone", Hostname: "gone", Mode: AgentModeConnect, LastSeen: time.Now().Add(-10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(registryTestAgent("here")); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, "")

	started := time.Now()
	_, err := dispatcher.AgentToolUpgrade(context.Background(), "gone", "pi")
	if err == nil || !hasAgentCode(err, "agent-offline", http.StatusServiceUnavailable) || !strings.Contains(err.Error(), "离线") {
		t.Fatalf("offline machine error = %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatalf("an offline machine must be refused at once, took %v", time.Since(started))
	}
	if _, err := dispatcher.AgentToolUpgrade(context.Background(), "nobody", "pi"); err == nil || !hasAgentCode(err, "agent-not-found", http.StatusNotFound) {
		t.Fatalf("unknown machine error = %v", err)
	}
	for _, tool := range []string{"", "   "} {
		if _, err := dispatcher.AgentToolUpgrade(context.Background(), "here", tool); err == nil || !hasAgentCode(err, "bad-request", http.StatusBadRequest) {
			t.Fatalf("empty tool %q error = %v", tool, err)
		}
	}
	// None of the refusals may leave a task behind for the machine.
	if task, ok := registry.Poll("here", 0, context.Background()); ok {
		t.Fatalf("a refused request queued %+v", task)
	}
}
