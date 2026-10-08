package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
	"github.com/zzjcool/homer-cli/internal/toolctl"
)

func toolStatus(id, version string) toolctl.Status {
	return toolctl.Status{ID: id, Adapter: id, Label: strings.ToUpper(id), Version: version, Upgradable: true}
}

func TestNormalizeToolsKeepsNilAndEmptyApart(t *testing.T) {
	if got := normalizeTools(nil); got != nil {
		t.Fatalf("nil list became %#v; not-reported must stay distinguishable", got)
	}
	got := normalizeTools([]toolctl.Status{})
	if got == nil || len(got) != 0 {
		t.Fatalf("empty list = %#v, want an empty non-nil list", got)
	}
}

func TestNormalizeToolsCleansWhatAnAgentSends(t *testing.T) {
	long := strings.Repeat("9", 500)
	in := []toolctl.Status{
		{ID: "  pi\x1b[31m  ", Adapter: "pi", Label: "pi\x00", Version: "0.87.1", Upgradable: true},
		{ID: "pi", Version: "1.0.0"},
		{ID: "", Version: "2.0.0"},
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
		t.Fatal("normalizing edited the caller's list")
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

func TestRegistryAttachAndUpdateTools(t *testing.T) {
	r := NewRegistry()
	session, peer := registryTestSession()
	defer session.Close(stream.CloseNormal, "test complete")
	defer peer.CloseNow()
	first := AgentInfo{AgentID: "agent-a", Tools: []toolctl.Status{toolStatus("pi", "0.87.1")}}
	r.Attach(first, session)
	r.Attach(AgentInfo{AgentID: "agent-a"}, session)
	info, _ := r.Get("agent-a")
	if len(info.Tools) != 1 || info.Tools[0].Version != "0.87.1" {
		t.Fatalf("an omitted tools list erased the previous report: %+v", info.Tools)
	}
	r.UpdateTools("agent-a", []toolctl.Status{})
	info, _ = r.Get("agent-a")
	if info.Tools == nil || len(info.Tools) != 0 {
		t.Fatalf("an explicit empty tools list was not retained: %#v", info.Tools)
	}
	r.UpdateTools("agent-a", []toolctl.Status{toolStatus("pi", "0.90.2"), toolStatus("herdr", "0.9.1")})
	r.UpdateTools("agent-a", []toolctl.Status{toolStatus("pi", "0.91.0")})
	info, _ = r.Get("agent-a")
	if len(info.Tools) != 1 || info.Tools[0].ID != "pi" || info.Tools[0].Version != "0.91.0" {
		t.Fatalf("tools were not replaced: %+v", info.Tools)
	}
	r.UpdateTools("missing", []toolctl.Status{toolStatus("pi", "1.0.0")})
}

func TestRegistryHandsOutCopiesOfTools(t *testing.T) {
	r := NewRegistry()
	session, peer := registryTestSession()
	defer session.Close(stream.CloseNormal, "test complete")
	defer peer.CloseNow()
	agent := AgentInfo{AgentID: "agent-a", Tools: []toolctl.Status{toolStatus("pi", "0.87.1")}}
	r.Attach(agent, session)
	got, _ := r.Get("agent-a")
	got.Tools[0].Version = "tampered"
	listed := r.List()
	listed[0].Tools[0].ID = "tampered"
	again, _ := r.Get("agent-a")
	if again.Tools[0].Version != "0.87.1" || again.Tools[0].ID != "pi" {
		t.Fatalf("a caller edited the registry through a copy: %+v", again.Tools)
	}
	agent.Tools[0].Version = "tampered-after-attach"
	again, _ = r.Get("agent-a")
	if again.Tools[0].Version != "0.87.1" {
		t.Fatalf("registry kept the caller's slice: %+v", again.Tools)
	}
}

func TestNoteToolVersion(t *testing.T) {
	r := NewRegistry()
	session, peer := registryTestSession()
	defer session.Close(stream.CloseNormal, "test complete")
	defer peer.CloseNow()
	r.Attach(AgentInfo{AgentID: "agent-a", Tools: []toolctl.Status{
		{ID: "pi", Version: "0.80.0", Error: "读不出版本：旧的错误"}, toolStatus("herdr", "0.9.1"),
	}}, session)
	r.NoteToolVersion("agent-a", "pi", " 0.90.2 ")
	info, _ := r.Get("agent-a")
	if info.Tools[0].Version != "0.90.2" || info.Tools[0].Error != "" || info.Tools[1].Version != "0.9.1" {
		t.Fatalf("tools after NoteToolVersion = %+v", info.Tools)
	}
	r.NoteToolVersion("agent-a", "missing", "1.0")
	r.NoteToolVersion("agent-a", "pi", "")
	r.NoteToolVersion("nobody", "pi", "1.0")
	var nilRegistry *Registry
	nilRegistry.NoteToolVersion("agent-a", "pi", "1.0")
	info, _ = r.Get("agent-a")
	if info.Tools[0].Version != "0.90.2" {
		t.Fatalf("no-op notes changed tools: %+v", info.Tools)
	}
}

func TestDispatcherListAgentsMapsTools(t *testing.T) {
	registry := NewRegistry()
	registry.Attach(AgentInfo{AgentID: "agent-a", Tools: []toolctl.Status{toolStatus("pi", "0.87.1")}}, nil)
	registry.Attach(AgentInfo{AgentID: "agent-b"}, nil)
	listed := NewDispatcher(registry, "").ListAgents()
	if len(listed) != 2 || listed[0].AgentID != "agent-a" {
		t.Fatalf("listed agents = %+v", listed)
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

func TestAgentToolUpgradeAndNotesVersion(t *testing.T) {
	registry := NewRegistry()
	hubConn, agentConn := streamtest.Pipe(streamtest.PipeOptions{})
	agentSession := stream.NewSession(agentConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	agentSession.Handle(string(TaskKindToolUpgrade), func(ctx context.Context, req *stream.Request) (any, error) {
		var options TaskOptions
		if err := req.Decode(&options); err != nil {
			return nil, err
		}
		if options.Tool != "pi" {
			t.Errorf("tool-upgrade options = %+v", options)
		}
		return json.RawMessage(upgradeReportJSON("pi", "0.80.0", "0.90.2")), nil
	})
	go func() { _ = agentSession.Run(context.Background()) }()
	hubSession := stream.NewSession(hubConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	registry.Attach(AgentInfo{AgentID: "agent-a", Tools: []toolctl.Status{toolStatus("pi", "0.80.0")}, caps: []string{string(TaskKindToolUpgrade)}}, hubSession)
	go func() { _ = hubSession.Run(context.Background()) }()
	defer func() {
		agentSession.Close(stream.CloseNormal, "test complete")
		hubSession.Close(stream.CloseNormal, "test complete")
	}()

	dispatcher := NewDispatcher(registry, "")
	report, err := dispatcher.AgentToolUpgrade(context.Background(), "agent-a", "pi")
	if err != nil || !strings.Contains(string(report), `"after":"0.90.2"`) {
		t.Fatalf("AgentToolUpgrade = %s, %v", report, err)
	}
	info, _ := registry.Get("agent-a")
	if info.Tools[0].Version != "0.90.2" {
		t.Fatalf("pi version after upgrade = %q", info.Tools[0].Version)
	}
	if _, err := dispatcher.AgentToolUpgrade(context.Background(), "agent-a", " "); err == nil {
		t.Fatal("empty tool ID was accepted")
	}
}
