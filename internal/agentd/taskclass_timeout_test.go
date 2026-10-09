package agentd

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/web"
)

func TestWriteTaskTimeoutExplainsAndLogsStillRunningWriter(t *testing.T) {
	logger := &captureLogger{}
	started := make(chan struct{}, 2)
	releases := make(chan chan struct{}, 2)
	firstRelease := make(chan struct{})
	releases <- firstRelease
	exec := &taskGateTestExecutor{started: started, releases: releases}
	d := New(Config{AgentID: "task-gate-timeout", TaskTimeout: 40 * time.Millisecond, Stream: stream.Options{Logger: logger}}, exec)
	t.Cleanup(func() {
		closeTaskRelease(firstRelease)
	})

	_, err := d.executeTask(context.Background(), &stream.Request{
		Method: string(hub.TaskKindPush), Params: []byte(`{"adapters":["fixture"]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("first write task error = %v, want timeout", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background write task did not start")
	}

	lines := logger.snapshot()
	if len(lines) != 1 || !strings.Contains(lines[0], string(hub.TaskKindPush)) || !strings.Contains(lines[0], "still running") {
		t.Fatalf("timed-out background write log = %v, want one still-running entry naming the method", lines)
	}
	if !strings.Contains(lines[0], "held=") {
		t.Fatalf("timed-out background write log omits held duration: %q", lines[0])
	}
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 45*time.Millisecond)
	defer secondCancel()
	_, err = d.executeTask(secondCtx, &stream.Request{Method: string(hub.TaskKindPull), Params: []byte(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "仍在运行") || !strings.Contains(err.Error(), string(hub.TaskKindPush)) {
		t.Fatalf("queued pull error = %v, want running push explanation", err)
	}
	if !strings.Contains(err.Error(), "已运行") {
		t.Fatalf("queued pull error omits writer hold time: %v", err)
	}
	if !strings.Contains(fmt.Sprint(logger.snapshot()), string(hub.TaskKindPush)) {
		t.Fatalf("timeout logger lost the current writer method: %v", logger.snapshot())
	}

	closeTaskRelease(firstRelease)
	waitTaskGateReleased(t, d.writeGate)

	// A second background push times out while holding the same method lock;
	// the throttled logger must suppress a duplicate line in its one-minute window.
	secondRelease := make(chan struct{})
	t.Cleanup(func() { closeTaskRelease(secondRelease) })
	releases <- secondRelease
	_, err = d.executeTask(context.Background(), &stream.Request{
		Method: string(hub.TaskKindPush), Params: []byte(`{"adapters":["fixture"]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("second write task error = %v, want timeout", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("second background write task did not start")
	}
	if got := len(logger.snapshot()); got != 1 {
		t.Fatalf("repeated timed-out writer log count = %d, want one throttled line", got)
	}
	closeTaskRelease(secondRelease)
	waitTaskGateReleased(t, d.writeGate)
}

type taskGateTestExecutor struct {
	started  chan struct{}
	releases chan chan struct{}
}

func (e *taskGateTestExecutor) Status(context.Context) (commands.StatusReport, error) {
	return commands.StatusReport{}, nil
}
func (e *taskGateTestExecutor) Diff(context.Context, web.DiffParams) (string, error) { return "", nil }
func (e *taskGateTestExecutor) Push(context.Context, bool, []string, bool, bool) (commands.PushReport, error) {
	e.started <- struct{}{}
	release := <-e.releases
	<-release
	return commands.PushReport{OK: true}, nil
}
func (*taskGateTestExecutor) Pull(context.Context, bool, []string, bool) (commands.PullReport, error) {
	return commands.PullReport{OK: true}, nil
}

func closeTaskRelease(release chan struct{}) {
	select {
	case <-release:
	default:
		close(release)
	}
}

func waitTaskGateReleased(t *testing.T, gate *taskGate) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		held := gate.held
		gate.mu.Unlock()
		if !held {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed-out background command did not release the write gate after finishing")
}
