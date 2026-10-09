package agentd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/resolutions"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
)

func TestRunResolveRecord_RecordListClear(t *testing.T) {
	home := t.TempDir()
	d := New(Config{HomerHome: home, AgentID: "resolution-record"}, &testExecutor{})
	req := &stream.Request{Method: string(hub.TaskKindResolveRecord)}

	result, err := d.runTaskCommand(context.Background(), req, hub.TaskOptions{
		ResolutionAction: resolutions.ActionRecord,
		ResolutionChoice: resolutions.ChoiceCenter,
		Adapters:         []string{"pi"},
		CenterGeneration: 7,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	recorded, ok := result.(resolutions.Report)
	if !ok || !recorded.OK || recorded.Pending != 1 || len(recorded.Recorded) != 1 || recorded.Recorded[0].GenerationAtRecord != 7 {
		t.Fatalf("record report = %#v", result)
	}
	info, err := os.Stat(filepath.Join(home, resolutions.FileName))
	if err != nil {
		t.Fatalf("stat resolution file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("resolution file mode = %04o, want 0600", info.Mode().Perm())
	}

	result, err = d.runTaskCommand(context.Background(), req, hub.TaskOptions{ResolutionAction: resolutions.ActionList})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	listed, ok := result.(resolutions.Report)
	if !ok || !listed.OK || listed.Action != resolutions.ActionList || listed.Pending != 1 || len(listed.Entries) != 1 {
		t.Fatalf("list report = %#v", result)
	}

	result, err = d.runTaskCommand(context.Background(), req, hub.TaskOptions{
		ResolutionAction: resolutions.ActionClear,
		Adapters:         []string{"pi"},
	})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	cleared, ok := result.(resolutions.Report)
	if !ok || !cleared.OK || cleared.Pending != 0 || len(cleared.Removed) != 1 || cleared.Removed[0].Adapter != "pi" {
		t.Fatalf("clear report = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(home, resolutions.FileName)); !os.IsNotExist(err) {
		t.Fatalf("resolution file after clear: %v, want not-exist", err)
	}
}

func TestResolveRecordNotBlockedByWriteGate(t *testing.T) {
	d := New(Config{HomerHome: t.TempDir(), AgentID: "record-read-gate"}, &testExecutor{})
	release, _, err := d.writeGate.acquire(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	params, err := json.Marshal(hub.TaskOptions{
		ResolutionAction: resolutions.ActionRecord,
		ResolutionChoice: resolutions.ChoiceLocal,
		Adapters:         []string{"pi"},
		CenterGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	result, err := d.executeTask(ctx, &stream.Request{Method: string(hub.TaskKindResolveRecord), Params: params})
	if err != nil {
		t.Fatalf("resolve-record waited behind write gate: %v", err)
	}
	if time.Since(started) >= time.Second {
		t.Fatal("resolve-record did not complete within one second")
	}
	if report, ok := result.(resolutions.Report); !ok || !report.OK {
		t.Fatalf("resolve-record result = %#v", result)
	}
}

func TestResolveRecordInvalidatesDriftAndStatusFlight(t *testing.T) {
	d := New(Config{HomerHome: t.TempDir(), AgentID: "record-invalidation"}, &testExecutor{})
	d.setDrift(&hub.AgentDrift{Push: 2, Conflicts: 1})
	d.statusFlight.mu.Lock()
	writeBefore := d.statusFlight.writeGen
	epochBefore := d.statusFlight.scanEpoch
	d.statusFlight.mu.Unlock()

	_, err := d.runTaskCommand(context.Background(), &stream.Request{Method: string(hub.TaskKindResolveRecord)}, hub.TaskOptions{
		ResolutionAction: resolutions.ActionRecord,
		ResolutionChoice: resolutions.ChoiceCenter,
		Adapters:         []string{"pi"},
		CenterGeneration: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	d.statusFlight.mu.Lock()
	writeAfter := d.statusFlight.writeGen
	epochAfter := d.statusFlight.scanEpoch
	d.statusFlight.mu.Unlock()
	if writeAfter != writeBefore+1 || epochAfter != epochBefore+1 {
		t.Fatalf("write generation/status epoch = %d/%d, want %d/%d", writeAfter, epochAfter, writeBefore+1, epochBefore+1)
	}
	if d.cachedDrift() != nil {
		t.Fatal("cached drift was not invalidated after recording a decision")
	}
	select {
	case <-d.heartbeatWake:
	default:
		t.Fatal("recording a decision did not signal heartbeat")
	}
}

func TestHelloAdvertisesResolveRecord(t *testing.T) {
	home := t.TempDir()
	d := New(Config{HomerHome: home, Home: t.TempDir(), AgentID: "record-capability"}, &testExecutor{})
	d.setDrift(&hub.AgentDrift{}) // keep hello's cache refresh from starting a status scan
	agentConn, hubConn := streamtest.Pipe(streamtest.PipeOptions{})
	agentSession := stream.NewSession(agentConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	hubSession := stream.NewSession(hubConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	seen := make(chan hub.HelloParams, 1)
	hubSession.Handle(hub.MethodHello, func(_ context.Context, req *stream.Request) (any, error) {
		var params hub.HelloParams
		if err := req.Decode(&params); err != nil {
			return nil, err
		}
		seen <- params
		return hub.WelcomeResult{Proto: stream.ProtocolVersion}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = agentSession.Run(ctx) }()
	go func() { _ = hubSession.Run(ctx) }()
	if err := d.hello(ctx, agentSession); err != nil {
		t.Fatalf("hello: %v", err)
	}
	select {
	case params := <-seen:
		if !containsString(params.Caps, string(hub.TaskKindResolveRecord)) {
			t.Fatalf("hello capabilities = %v", params.Caps)
		}
	case <-time.After(time.Second):
		t.Fatal("hub did not receive hello")
	}
	agentSession.Close(stream.CloseNormal, "test complete")
	hubSession.Close(stream.CloseNormal, "test complete")
}

func TestHandlersRegisterResolveRecord(t *testing.T) {
	home := t.TempDir()
	d := New(Config{HomerHome: home, AgentID: "record-handler"}, &testExecutor{})
	agentConn, hubConn := streamtest.Pipe(streamtest.PipeOptions{})
	agentSession := stream.NewSession(agentConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	hubSession := stream.NewSession(hubConn, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	d.registerHandlers(agentSession)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() { _ = agentSession.Run(ctx); done <- struct{}{} }()
	go func() { _ = hubSession.Run(ctx); done <- struct{}{} }()
	callCtx, callCancel := context.WithTimeout(ctx, time.Second)
	payload, err := hubSession.Call(callCtx, string(hub.TaskKindResolveRecord), hub.TaskOptions{
		ResolutionAction: resolutions.ActionList,
	}, stream.WithBudget(time.Second))
	callCancel()
	if err != nil {
		t.Fatalf("resolve-record handler call: %v", err)
	}
	var report resolutions.Report
	if err := json.Unmarshal(payload, &report); err != nil {
		t.Fatalf("decode handler response %s: %v", payload, err)
	}
	if !report.OK || report.Action != resolutions.ActionList {
		t.Fatalf("handler report = %+v", report)
	}
	cancel()
	agentSession.Close(stream.CloseNormal, "test complete")
	hubSession.Close(stream.CloseNormal, "test complete")
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("test stream session did not stop")
		}
	}
}

func TestCountEffectiveResolutions(t *testing.T) {
	report := commands.StatusReport{
		Adapters: []commands.StatusAdapterReport{
			{ID: "pi", Conflicts: 2},
			{ID: "pad", Conflicts: 0},
			{ID: "vscode", Conflicts: 1},
		},
		Resolutions: []commands.StatusResolution{
			{Adapter: "pi"}, {Adapter: "pad"}, {Adapter: "missing"},
		},
	}
	if got := countEffectiveResolutions(report); got != 1 {
		t.Fatalf("countEffectiveResolutions() = %d, want 1", got)
	}
}

func TestDriftFromStatusIncludesResolutions(t *testing.T) {
	drift := driftFromStatus(commands.StatusReport{
		Adapters:    []commands.StatusAdapterReport{{ID: "pi", Push: 2, Pull: 1, Conflicts: 1}},
		Resolutions: []commands.StatusResolution{{Adapter: "pi"}},
	})
	if drift.Resolutions != 1 || drift.Push != 2 || drift.Pull != 1 || drift.Conflicts != 1 {
		t.Fatalf("driftFromStatus() = %+v", drift)
	}
}

func resolutionTestConfig(userHome string, adapters ...string) core.HomerConfig {
	config := core.HomerConfig{Version: 1, Adapters: make(map[string]core.AdapterConfig, len(adapters))}
	for _, adapter := range adapters {
		config.Adapters[adapter] = core.AdapterConfig{
			Root: filepath.Join(userHome, ".resolution-test-"+adapter),
			Categories: map[string]core.CategoryConfig{
				"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
			},
		}
	}
	return config
}

func TestStatusEmbedsResolutions(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	homerHome := t.TempDir()
	paths := homerPathsForHome(homerHome)
	config := resolutionTestConfig(userHome, "pi")
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	if _, err := resolutions.Record(paths, resolutions.ChoiceCenter, []string{"pi"}, 11, nil); err != nil {
		t.Fatal(err)
	}
	executor := &localExecutor{homerHome: homerHome}
	report, err := executor.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(report.Resolutions) != 1 || report.Resolutions[0].Adapter != "pi" || report.Resolutions[0].GenerationAtRecord != 11 {
		t.Fatalf("status resolutions = %+v", report.Resolutions)
	}
}

func TestStatusResolutionLoadErrorIsWarning(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	homerHome := t.TempDir()
	paths := homerPathsForHome(homerHome)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveConfig(paths, resolutionTestConfig(userHome, "pi")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homerHome, resolutions.FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := (&localExecutor{homerHome: homerHome}).Status(context.Background())
	if err != nil {
		t.Fatalf("status must fail open on corrupt decisions: %v", err)
	}
	if !containsSubstring(report.Warnings, "读取已记录决定失败") || len(report.Resolutions) != 0 {
		t.Fatalf("status warnings/resolutions = %v / %+v", report.Warnings, report.Resolutions)
	}
}

func TestStatusDoesNotAttachResolutionsToMissingConfigFallback(t *testing.T) {
	home := t.TempDir()
	paths := homerPathsForHome(home)
	if _, err := resolutions.Record(paths, resolutions.ChoiceLocal, []string{"pi"}, 1, nil); err != nil {
		t.Fatal(err)
	}
	report, err := (&localExecutor{homerHome: home}).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Resolutions) != 0 || !containsSubstring(report.Errors, "未找到 homer 配置") {
		t.Fatalf("fresh status unexpectedly attached decisions: %+v", report)
	}
}

func TestRunResolveRecordBadActionIsReported(t *testing.T) {
	d := New(Config{HomerHome: t.TempDir()}, &testExecutor{})
	result, err := d.runTaskCommand(context.Background(), &stream.Request{Method: string(hub.TaskKindResolveRecord)}, hub.TaskOptions{ResolutionAction: "bad"})
	if err != nil {
		t.Fatal(err)
	}
	report, ok := result.(resolutions.Report)
	if !ok || report.OK || len(report.Errors) == 0 || !strings.Contains(report.Errors[0], "unknown resolution action") {
		t.Fatalf("bad action result = %#v", result)
	}
}
