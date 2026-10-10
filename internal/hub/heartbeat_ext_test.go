package hub

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/zzjcool/homer-cli/internal/stream"
)

func TestMachineReportedAdapters(t *testing.T) {
	tests := []struct {
		name    string
		status  json.RawMessage
		want    []string
		wantNil bool
	}{
		{
			name:   "report.adapters envelope",
			status: json.RawMessage(`{"report":{"adapters":[{"id":"pi"},{"id":" herdr "},{"id":"pi"},{"id":""}]}}`),
			want:   []string{"herdr", "pi"},
		},
		{
			name:   "top-level adapters",
			status: json.RawMessage(`{"adapters":[{"id":"opencode"},{"id":"vscode"}],"errors":[]}`),
			want:   []string{"opencode", "vscode"},
		},
		{
			name:   "explicit empty adapters",
			status: json.RawMessage(`{"report":{"adapters":[]}}`),
			want:   []string{},
		},
		{
			name:    "missing adapters",
			status:  json.RawMessage(`{}`),
			wantNil: true,
		},
		{
			name:    "invalid JSON",
			status:  json.RawMessage(`{"report":`),
			wantNil: true,
		},
		{
			// 真实 PullReport 形状：无 adapters 字段，applied.written/deleted
			// 携带实际触及的 adapterId（F4 修复后的抽取路径）。
			name:   "pull report applied refs",
			status: json.RawMessage(`{"ok":true,"status":"synced","applied":{"written":[{"adapterId":"pi","category":"settings","relPath":"settings.json"},{"adapterId":"pi","category":"models","relPath":"models.json"}],"deleted":[{"adapterId":"opencode","category":"config","relPath":"config.json"}]}}`),
			want: []string{"opencode", "pi"},
		},
		{
			// 空 applied（noop pull）：没有触及任何 adapter，返回 nil——
		// 保留之前 status 已记录的集合，不把覆盖数清零。
			name:    "pull report empty applied keeps previous report",
			status:  json.RawMessage(`{"ok":true,"status":"noop","applied":{"written":[],"deleted":[]}}`),
			wantNil: true,
		},
		{
			name:    "pull report no applied field",
			status:  json.RawMessage(`{"ok":true,"status":"noop"}`),
			wantNil: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := MachineReportedAdapters(test.status)
			if test.wantNil {
				if got != nil {
					t.Fatalf("MachineReportedAdapters() = %#v, want nil", got)
				}
				return
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("MachineReportedAdapters() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRegistryNoteReportedAdapters(t *testing.T) {
	registry := NewRegistry()
	registry.Attach(AgentInfo{AgentID: "agent-a"}, nil)

	registry.NoteReportedAdapters("agent-a", []string{"herdr", "pi"})
	got := registry.AgentReportedAdapters("agent-a")
	if want := []string{"herdr", "pi"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AgentReportedAdapters() = %#v, want %#v", got, want)
	}

	got[0] = "mutated"
	if again := registry.AgentReportedAdapters("agent-a"); !reflect.DeepEqual(again, []string{"herdr", "pi"}) {
		t.Fatalf("AgentReportedAdapters() returned registry-owned data: %#v", again)
	}

	registry.NoteReportedAdapters("agent-a", []string{})
	if cleared := registry.AgentReportedAdapters("agent-a"); cleared == nil || len(cleared) != 0 {
		t.Fatalf("explicit empty report = %#v, want a non-nil empty slice", cleared)
	}
	registry.NoteReportedAdapters("missing", []string{"ignored"})
	if got := registry.AgentReportedAdapters("missing"); got != nil {
		t.Fatalf("missing agent adapters = %#v, want nil", got)
	}

	var nilRegistry *Registry
	nilRegistry.NoteReportedAdapters("agent-a", []string{"ignored"})
}

func TestDispatcherStatusReportedAdaptersIntegration(t *testing.T) {
	const agentID = "agent-a"
	registry := NewRegistry()
	agent := newDispatcherAgent(t, registry, agentID, MethodStatus)
	agent.Handle(MethodStatus, func(_ context.Context, _ *stream.Request) (any, error) {
		return json.RawMessage(`{"report":{"adapters":[{"id":"pi"},{"id":"herdr"}]}}`), nil
	})
	agent.Run()

	raw, err := NewDispatcher(registry, "").AgentStatus(context.Background(), agentID)
	if err != nil {
		t.Fatalf("AgentStatus(): %v", err)
	}
	registry.NoteReportedAdapters(agentID, MachineReportedAdapters(raw))
	if got, want := registry.AgentReportedAdapters(agentID), []string{"herdr", "pi"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AgentReportedAdapters() after status = %#v, want %#v", got, want)
	}
}
