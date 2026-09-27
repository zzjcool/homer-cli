package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func registryTestAgent(agentID string) AgentInfo {
	return AgentInfo{
		AgentID:  agentID,
		Hostname: agentID + "-host",
		Mode:     AgentModeConnect,
		Version:  "test",
	}
}

func registryTestTask(taskID string) Task {
	return Task{
		TaskID:    taskID,
		Kind:      TaskKindStatus,
		CreatedAt: time.Now(),
	}
}

func TestRegisterUpsertAndList(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(AgentInfo{AgentID: "agent-b", Hostname: "box-b", Mode: AgentModeConnect, Version: "v1"}); err != nil {
		t.Fatalf("register agent-b: %v", err)
	}
	if err := r.Register(AgentInfo{AgentID: "agent-a", Hostname: "box-a", Mode: AgentModeListen, Addr: "http://box-a:7761", Version: "v1"}); err != nil {
		t.Fatalf("register agent-a: %v", err)
	}
	if err := r.Register(AgentInfo{AgentID: "agent-b", Hostname: "box-b-new", Mode: AgentModeConnect, Version: "v2"}); err != nil {
		t.Fatalf("upsert agent-b: %v", err)
	}

	got := r.List()
	if len(got) != 2 {
		t.Fatalf("List() returned %d agents, want 2", len(got))
	}
	if ids := []string{got[0].AgentID, got[1].AgentID}; !reflect.DeepEqual(ids, []string{"agent-a", "agent-b"}) {
		t.Fatalf("List() IDs = %v, want [agent-a agent-b]", ids)
	}
	if got[1].Hostname != "box-b-new" || got[1].Version != "v2" {
		t.Fatalf("upserted agent-b = %+v", got[1])
	}

	got[0].Hostname = "mutated"
	got[0].LastSeen = time.Time{}
	info, ok := r.Get("agent-a")
	if !ok {
		t.Fatal("Get(agent-a) reported missing agent")
	}
	if info.Hostname != "box-a" || info.LastSeen.IsZero() {
		t.Fatalf("mutating List result changed registry: %+v", info)
	}
}

func TestRegisterValidation(t *testing.T) {
	r := NewRegistry()
	tests := []struct {
		name string
		info AgentInfo
	}{
		{name: "empty agent id", info: AgentInfo{Mode: AgentModeConnect}},
		{name: "listen without address", info: AgentInfo{AgentID: "listen", Mode: AgentModeListen}},
		{name: "invalid mode", info: AgentInfo{AgentID: "invalid", Mode: AgentMode("other")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := r.Register(tt.info); err == nil {
				t.Fatal("Register() succeeded for invalid agent")
			}
		})
	}
}

func TestEnqueueUnknownAgentAndFull(t *testing.T) {
	r := NewRegistry()
	if err := r.Enqueue("missing", registryTestTask("missing-task")); err == nil {
		t.Fatal("Enqueue() succeeded for an unknown agent")
	}
	if err := r.Register(registryTestAgent("agent-a")); err != nil {
		t.Fatalf("register: %v", err)
	}
	for i := 0; i < TaskQueueCapacity; i++ {
		if err := r.Enqueue("agent-a", registryTestTask(fmt.Sprintf("task-%02d", i))); err != nil {
			t.Fatalf("Enqueue(%d): %v", i, err)
		}
	}
	if err := r.Enqueue("agent-a", registryTestTask("task-over-capacity")); err == nil {
		t.Fatal("Enqueue() succeeded after queue reached capacity")
	}
}

func TestPollImmediateAndWakes(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(registryTestAgent("agent-a")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Enqueue("agent-a", registryTestTask("immediate")); err != nil {
		t.Fatalf("enqueue immediate: %v", err)
	}
	got, ok := r.Poll("agent-a", 0, context.Background())
	if !ok || got.TaskID != "immediate" {
		t.Fatalf("immediate Poll() = (%+v, %v), want immediate task", got, ok)
	}
	if gotAgain, ok := r.Poll("agent-a", 0, context.Background()); ok || gotAgain.TaskID != "" {
		t.Fatalf("task was delivered more than once: (%+v, %v)", gotAgain, ok)
	}

	started := make(chan struct{})
	result := make(chan struct {
		task    Task
		ok      bool
		elapsed time.Duration
	}, 1)
	go func() {
		start := time.Now()
		close(started)
		task, ok := r.Poll("agent-a", time.Second, context.Background())
		result <- struct {
			task    Task
			ok      bool
			elapsed time.Duration
		}{task: task, ok: ok, elapsed: time.Since(start)}
	}()
	<-started
	time.Sleep(10 * time.Millisecond)
	if err := r.Enqueue("agent-a", registryTestTask("woken")); err != nil {
		t.Fatalf("enqueue woken: %v", err)
	}
	select {
	case got := <-result:
		if !got.ok || got.task.TaskID != "woken" {
			t.Fatalf("woken Poll() = (%+v, %v)", got.task, got.ok)
		}
		if got.elapsed > 200*time.Millisecond {
			t.Fatalf("Poll() wake took %s, want <= 200ms", got.elapsed)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Poll() did not wake after Enqueue()")
	}
	if got, ok := r.Poll("agent-a", 0, context.Background()); ok || got.TaskID != "" {
		t.Fatalf("woken task was delivered more than once: (%+v, %v)", got, ok)
	}
}

func TestPollWaitTimeoutAndCtx(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(registryTestAgent("agent-a")); err != nil {
		t.Fatalf("register: %v", err)
	}
	start := time.Now()
	if got, ok := r.Poll("agent-a", 40*time.Millisecond, context.Background()); ok || got.TaskID != "" {
		t.Fatalf("timeout Poll() = (%+v, %v), want no task", got, ok)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("Poll() returned after %s, want it to wait for its timeout", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	if got, ok := r.Poll("agent-a", time.Second, ctx); ok || got.TaskID != "" {
		t.Fatalf("cancelled Poll() = (%+v, %v), want no task", got, ok)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("cancelled Poll() took %s", elapsed)
	}
}

func TestSubmitWaitRoundtrip(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(registryTestAgent("agent-a")); err != nil {
		t.Fatalf("register: %v", err)
	}
	task := registryTestTask("roundtrip")
	if err := r.Enqueue("agent-a", task); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	polled, ok := r.Poll("agent-a", 0, context.Background())
	if !ok || polled.TaskID != task.TaskID {
		t.Fatalf("Poll() = (%+v, %v)", polled, ok)
	}

	result := TaskResult{
		TaskID:  task.TaskID,
		AgentID: "agent-a",
		OK:      true,
		Kind:    TaskKindStatus,
		Report:  json.RawMessage(`{"status":"ok"}`),
	}
	if err := r.Submit(result); err != nil {
		t.Fatalf("Submit(): %v", err)
	}
	result.Report[0] = 'X'
	got, err := r.Wait(context.Background(), task.TaskID)
	if err != nil {
		t.Fatalf("Wait(): %v", err)
	}
	want := TaskResult{
		TaskID:  task.TaskID,
		AgentID: "agent-a",
		OK:      true,
		Kind:    TaskKindStatus,
		Report:  json.RawMessage(`{"status":"ok"}`),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Wait() = %+v, want %+v", got, want)
	}
	got.Report[0] = 'Y'
	gotAgain, err := r.Wait(context.Background(), task.TaskID)
	if err != nil {
		t.Fatalf("second Wait(): %v", err)
	}
	if !reflect.DeepEqual(gotAgain, want) {
		t.Fatalf("Wait() result was not copied: %+v, want %+v", gotAgain, want)
	}
	if err := r.Submit(result); err == nil {
		t.Fatal("duplicate Submit() succeeded")
	}
	if err := r.Submit(TaskResult{TaskID: "unknown"}); err == nil {
		t.Fatal("Submit() for unknown task succeeded")
	}
}

func TestTaskTTLExpiry(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(registryTestAgent("agent-a")); err != nil {
		t.Fatalf("register: %v", err)
	}
	task := registryTestTask("expired")
	task.CreatedAt = time.Now().Add(-TaskTTL - time.Second)
	if err := r.Enqueue("agent-a", task); err != nil {
		t.Fatalf("enqueue expired task: %v", err)
	}
	if got, ok := r.Poll("agent-a", 0, context.Background()); ok || got.TaskID != "" {
		t.Fatalf("expired Poll() = (%+v, %v), want no task", got, ok)
	}
	if err := r.Submit(TaskResult{TaskID: task.TaskID}); err == nil {
		t.Fatal("Submit() for expired task succeeded")
	}
	if _, err := r.Wait(context.Background(), task.TaskID); err == nil {
		t.Fatal("Wait() for expired task succeeded")
	}
}

func TestTouchAndStale(t *testing.T) {
	r := NewRegistry()
	old := time.Now().Add(-AgentStaleAfter - time.Second)
	if err := r.Register(AgentInfo{
		AgentID:  "old-agent",
		Hostname: "old-host",
		Mode:     AgentModeConnect,
		LastSeen: old,
	}); err != nil {
		t.Fatalf("register old agent: %v", err)
	}
	listed := r.List()
	if len(listed) != 1 || !listed[0].Stale {
		t.Fatalf("stale List() = %+v, want one stale agent", listed)
	}
	r.Touch("old-agent")
	info, ok := r.Get("old-agent")
	if !ok {
		t.Fatal("Get() reported missing touched agent")
	}
	if !info.LastSeen.After(old) {
		t.Fatalf("Touch() LastSeen = %s, want after %s", info.LastSeen, old)
	}
	listed = r.List()
	if listed[0].Stale {
		t.Fatalf("touched agent remained stale: %+v", listed[0])
	}
	r.Touch("missing")
}

func TestRegistryConcurrent(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(registryTestAgent("shared")); err != nil {
		t.Fatalf("register shared: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			agentID := "shared"
			if i%5 == 0 {
				agentID = fmt.Sprintf("agent-%02d", i)
				_ = r.Register(registryTestAgent(agentID))
			}
			_ = r.Register(registryTestAgent("shared"))
			_ = r.Enqueue(agentID, registryTestTask(fmt.Sprintf("concurrent-%02d", i)))
			_ = r.Enqueue("unknown", registryTestTask(fmt.Sprintf("unknown-%02d", i)))
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			task, ok := r.Poll(agentID, 20*time.Millisecond, ctx)
			cancel()
			if ok {
				_ = r.Submit(TaskResult{TaskID: task.TaskID, AgentID: agentID, Kind: task.Kind})
			}
			_, _ = r.Get(agentID)
			_ = r.List()
			if i%3 == 0 {
				r.Touch(agentID)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent registry operations did not finish; possible deadlock")
	}

	got := r.List()
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].AgentID < got[j].AgentID }) {
		t.Fatalf("List() returned agents out of order: %+v", got)
	}
}
