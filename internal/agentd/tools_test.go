package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/toolctl"
)

// fakeTools stands in for the programs installed on a machine. Nothing here
// starts a process, so these tests do not depend on what the machine running
// them has installed.
type fakeTools struct {
	mu       sync.Mutex
	order    []string
	versions map[string]string // installed programs and their versions

	probes   int
	upgrades []string
	// gate, when set, holds every Probe until it is closed (or the context ends).
	gate chan struct{}

	// What Upgrade does.
	upgradeTo     string        // version a successful upgrade installs
	fail          bool          // fail instead of succeeding
	hold          chan struct{} // when set, Upgrade waits for it
	delay         time.Duration // when set, Upgrade takes this long
	upgradeCtxErr error         // the context's error when Upgrade finished
	upgradeDone   int
}

func newFakeTools(versions ...string) *fakeTools {
	f := &fakeTools{versions: map[string]string{}, upgradeTo: "0.90.2"}
	for i := 0; i+1 < len(versions); i += 2 {
		f.order = append(f.order, versions[i])
		f.versions[versions[i]] = versions[i+1]
	}
	return f
}

func (f *fakeTools) toolkit() Toolkit { return Toolkit{Probe: f.probe, Upgrade: f.upgrade} }

func (f *fakeTools) probe(ctx context.Context) []toolctl.Status {
	f.mu.Lock()
	f.probes++
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]toolctl.Status, 0, len(f.order))
	for _, id := range f.order {
		if version, ok := f.versions[id]; ok {
			out = append(out, toolctl.Status{ID: id, Adapter: id, Label: id, Version: version, Upgradable: true})
		}
	}
	return out
}

func (f *fakeTools) upgrade(ctx context.Context, id string) toolctl.UpgradeResult {
	f.mu.Lock()
	f.upgrades = append(f.upgrades, id)
	hold, delay, fail, to := f.hold, f.delay, f.fail, f.upgradeTo
	before, installed := f.versions[id]
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upgradeCtxErr = ctx.Err()
	f.upgradeDone++
	switch {
	case !installed:
		return toolctl.UpgradeResult{Status: "not-installed", Tool: id, Note: "这台机器上没有安装 " + id + "。"}
	case fail:
		return toolctl.UpgradeResult{Status: "failed", Tool: id, Label: id, Before: before,
			Note: id + " 升级没有成功：exit status 1", Output: "npm ERR! network", Manual: "curl -fsSL https://example.test/install.sh | sh"}
	}
	f.versions[id] = to
	return toolctl.UpgradeResult{OK: true, Status: "upgraded", Tool: id, Label: id, Before: before, After: to,
		Note: fmt.Sprintf("%s %s → %s", id, before, to)}
}

// holdProbes makes every later Probe wait until the returned channel is closed.
func (f *fakeTools) holdProbes() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = make(chan struct{})
	return f.gate
}

func (f *fakeTools) setVersion(id, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versions[id] = version
}

func (f *fakeTools) probeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probes
}

func (f *fakeTools) upgradeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.upgrades...)
}

func (f *fakeTools) finishedUpgrade() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upgradeDone, f.upgradeCtxErr
}

// toolTimings overrides the package's measurement timings for one test. Call
// it before starting any daemon; the old values return when the test ends,
// after the daemon has been stopped.
func toolTimings(t *testing.T, interval, firstWait time.Duration) {
	t.Helper()
	oldInterval, oldFirst := toolInterval, firstToolWait
	toolInterval, firstToolWait = interval, firstWait
	t.Cleanup(func() { toolInterval, firstToolWait = oldInterval, oldFirst })
}

func toolByID(statuses []toolctl.Status, id string) (toolctl.Status, bool) {
	for _, status := range statuses {
		if status.ID == id {
			return status, true
		}
	}
	return toolctl.Status{}, false
}

func stopDaemon(t *testing.T, cancel context.CancelFunc, runDone <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("Run() = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("daemon did not stop")
	}
}

// startConnectAgent runs a connect-mode daemon against a real hub registry.
func startConnectAgent(t *testing.T, id string, fake *fakeTools) (*hub.Registry, *Daemon, func()) {
	t.Helper()
	registry := hub.NewRegistry()
	hubServer := httptest.NewServer(hub.NewAgentAPI(registry, "token"))
	cfg := Config{ConnectURL: hubServer.URL, Token: "token", AgentID: id, PollWait: time.Second, PollInterval: 5 * time.Millisecond, ReportTimeout: time.Second}
	daemon := New(cfg, newRecordingExecutor())
	if fake != nil {
		daemon.SetToolkit(fake.toolkit())
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- daemon.Run(ctx) }()
	stop := func() {
		stopDaemon(t, cancel, runDone)
		hubServer.Close()
	}
	return registry, daemon, stop
}

func TestAgentdRegistersWithToolVersions(t *testing.T) {
	fake := newFakeTools("pi", "0.80.0", "herdr", "0.9.1")
	registry, _, stop := startConnectAgent(t, "tools-agent", fake)
	defer stop()

	waitFor(t, 3*time.Second, func() bool {
		info, ok := registry.Get("tools-agent")
		return ok && len(info.Tools) == 2
	})
	info, _ := registry.Get("tools-agent")
	pi, ok := toolByID(info.Tools, "pi")
	if !ok || pi.Version != "0.80.0" || !pi.Upgradable || pi.Label != "pi" {
		t.Fatalf("pi = %+v (found=%v)", pi, ok)
	}
	if herdr, ok := toolByID(info.Tools, "herdr"); !ok || herdr.Version != "0.9.1" {
		t.Fatalf("herdr = %+v (found=%v)", herdr, ok)
	}
}

func TestAgentdWithoutAToolkitReportsNoTools(t *testing.T) {
	registry, _, stop := startConnectAgent(t, "plain-agent", nil)
	defer stop()
	waitFor(t, 2*time.Second, func() bool { _, ok := registry.Get("plain-agent"); return ok })
	time.Sleep(150 * time.Millisecond) // a few polls
	info, _ := registry.Get("plain-agent")
	if info.Tools != nil {
		t.Fatalf("a daemon with no toolkit reported %+v", info.Tools)
	}
}

func TestAgentdHeartbeatsCarryFreshVersions(t *testing.T) {
	toolTimings(t, 40*time.Millisecond, 2*time.Second)
	fake := newFakeTools("pi", "0.80.0")
	registry, _, stop := startConnectAgent(t, "fresh-agent", fake)
	defer stop()
	waitFor(t, 3*time.Second, func() bool {
		info, _ := registry.Get("fresh-agent")
		pi, ok := toolByID(info.Tools, "pi")
		return ok && pi.Version == "0.80.0"
	})

	// The user upgrades pi by hand on the machine; the next measurement shows it.
	fake.setVersion("pi", "0.85.0")
	waitFor(t, 5*time.Second, func() bool {
		info, _ := registry.Get("fresh-agent")
		pi, ok := toolByID(info.Tools, "pi")
		return ok && pi.Version == "0.85.0"
	})
}

func TestAgentdReportsAnUninstall(t *testing.T) {
	toolTimings(t, 40*time.Millisecond, 2*time.Second)
	fake := newFakeTools("pi", "0.80.0", "herdr", "0.9.1")
	registry, _, stop := startConnectAgent(t, "gone-agent", fake)
	defer stop()
	waitFor(t, 3*time.Second, func() bool {
		info, _ := registry.Get("gone-agent")
		return len(info.Tools) == 2
	})
	fake.mu.Lock()
	delete(fake.versions, "herdr")
	fake.mu.Unlock()
	waitFor(t, 5*time.Second, func() bool {
		info, _ := registry.Get("gone-agent")
		_, still := toolByID(info.Tools, "herdr")
		return len(info.Tools) == 1 && !still
	})
	// Nothing left at all is a statement too, and it must clear the hub's list.
	fake.mu.Lock()
	delete(fake.versions, "pi")
	fake.mu.Unlock()
	waitFor(t, 5*time.Second, func() bool {
		info, _ := registry.Get("gone-agent")
		return info.Tools != nil && len(info.Tools) == 0
	})
}

func newBareDaemon(t *testing.T, fake *fakeTools) *Daemon {
	t.Helper()
	daemon := New(Config{AgentID: "bare", ConnectURL: "http://127.0.0.1:1", Token: "t"}, newRecordingExecutor())
	if fake != nil {
		daemon.SetToolkit(fake.toolkit())
	}
	return daemon
}

func TestReportedToolsNeverBlocksAHeartbeatOnASlowProgram(t *testing.T) {
	toolTimings(t, time.Hour, time.Second)
	fake := newFakeTools("pi", "0.80.0")
	gate := fake.holdProbes()
	daemon := newBareDaemon(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(gate)

	started := time.Now()
	if got := daemon.reportedTools(ctx, 0); got != nil {
		t.Fatalf("reported %+v before any measurement finished", *got)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("a heartbeat waited %v for a slow program", elapsed)
	}
	// A second heartbeat while the first measurement runs must not start another.
	_ = daemon.reportedTools(ctx, 0)
	_ = daemon.reportedTools(ctx, 0)
	waitFor(t, time.Second, func() bool { return fake.probeCount() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if n := fake.probeCount(); n != 1 {
		t.Fatalf("%d measurements started, want exactly 1 while one is running", n)
	}
}

func TestReportedToolsWaitsOnlyAsLongAsItIsTold(t *testing.T) {
	toolTimings(t, time.Hour, time.Second)
	fake := newFakeTools("pi", "0.80.0")
	gate := fake.holdProbes()
	daemon := newBareDaemon(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(gate)

	started := time.Now()
	if got := daemon.reportedTools(ctx, 120*time.Millisecond); got != nil {
		t.Fatalf("reported %+v from a measurement that has not finished", *got)
	}
	elapsed := time.Since(started)
	if elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatalf("waited %v, want about 120ms", elapsed)
	}
}

func TestReportedToolsReturnsTheFirstMeasurementWhenItIsQuick(t *testing.T) {
	fake := newFakeTools("pi", "0.80.0", "herdr", "0.9.1")
	daemon := newBareDaemon(t, fake)
	got := daemon.reportedTools(context.Background(), 2*time.Second)
	if got == nil || len(*got) != 2 || (*got)[0].ID != "pi" || (*got)[0].Version != "0.80.0" {
		t.Fatalf("first report = %v", got)
	}
	// And it is a copy: editing it does not edit the daemon's memory.
	(*got)[0].Version = "tampered"
	again := daemon.reportedTools(context.Background(), 0)
	if (*again)[0].Version != "0.80.0" {
		t.Fatalf("the daemon's cache was edited through a report: %+v", *again)
	}
}

func TestReportedToolsReusesAFreshMeasurement(t *testing.T) {
	toolTimings(t, time.Hour, time.Second)
	fake := newFakeTools("pi", "0.80.0")
	daemon := newBareDaemon(t, fake)
	_ = daemon.reportedTools(context.Background(), time.Second)
	for i := 0; i < 5; i++ {
		_ = daemon.reportedTools(context.Background(), 0)
	}
	if n := fake.probeCount(); n != 1 {
		t.Fatalf("%d measurements for six heartbeats inside one interval, want 1", n)
	}
}

func TestReportedToolsServesTheLastResultWhileRefreshing(t *testing.T) {
	toolTimings(t, 20*time.Millisecond, time.Second)
	fake := newFakeTools("pi", "0.80.0")
	daemon := newBareDaemon(t, fake)
	if got := daemon.reportedTools(context.Background(), time.Second); got == nil {
		t.Fatal("no first measurement")
	}
	time.Sleep(60 * time.Millisecond) // the measurement is now stale
	fake.setVersion("pi", "0.85.0")
	gate := fake.holdProbes() // the refresh will hang
	defer close(gate)

	started := time.Now()
	got := daemon.reportedTools(context.Background(), time.Second)
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("a stale cache made the heartbeat wait %v", time.Since(started))
	}
	if got == nil || (*got)[0].Version != "0.80.0" {
		t.Fatalf("while refreshing, report = %v, want the last known 0.80.0", got)
	}
}

func TestReportedToolsEmptyListIsAReport(t *testing.T) {
	daemon := newBareDaemon(t, newFakeTools())
	got := daemon.reportedTools(context.Background(), time.Second)
	if got == nil || len(*got) != 0 {
		t.Fatalf("nothing installed = %v, want a non-nil empty list", got)
	}
	raw, err := json.Marshal(struct {
		Tools *[]toolctl.Status `json:"tools,omitempty"`
	}{got})
	if err != nil || string(raw) != `{"tools":[]}` {
		t.Fatalf("wire form = %s, %v; an empty list must still be sent", raw, err)
	}
}

func TestReportedToolsDropsAMeasurementThatWasCutOff(t *testing.T) {
	fake := newFakeTools("pi", "0.80.0", "herdr", "0.9.1")
	gate := fake.holdProbes()
	daemon := newBareDaemon(t, fake)
	ctx, cancel := context.WithCancel(context.Background())

	if got := daemon.reportedTools(ctx, 0); got != nil {
		t.Fatalf("unexpected early report %v", *got)
	}
	waitFor(t, time.Second, func() bool { return fake.probeCount() == 1 })
	cancel() // the daemon is shutting down mid-measurement
	waitFor(t, time.Second, func() bool {
		daemon.tools.mu.Lock()
		defer daemon.tools.mu.Unlock()
		return daemon.tools.refreshing == nil
	})
	close(gate)

	// Whatever the cut-off measurement returned may be half a list; recording
	// it would read as "the rest was uninstalled".
	daemon.tools.mu.Lock()
	known := daemon.tools.known
	daemon.tools.mu.Unlock()
	if known {
		t.Fatal("a measurement that was cut off was recorded")
	}
}

func TestNoteToolVersionKeepsTheUpgradeAgainstAnOlderMeasurement(t *testing.T) {
	toolTimings(t, time.Hour, time.Second)
	fake := newFakeTools("pi", "0.80.0")
	daemon := newBareDaemon(t, fake)
	if got := daemon.reportedTools(context.Background(), time.Second); got == nil {
		t.Fatal("no first measurement")
	}

	// The upgrade finishes: the machine reports 0.90.2 at once...
	daemon.noteToolVersion("pi", "0.90.2")
	// ...and the next measurement hangs (a cold start), so the heartbeat has
	// to go out with the patched value, never the old one.
	gate := fake.holdProbes()
	defer close(gate)
	got := daemon.reportedTools(context.Background(), 0)
	if got == nil || (*got)[0].Version != "0.90.2" {
		t.Fatalf("heartbeat after an upgrade = %v, want 0.90.2", got)
	}
	waitFor(t, time.Second, func() bool { return fake.probeCount() == 2 })
}

func TestAMeasurementThatBeganBeforeAnUpgradeCannotOverwriteIt(t *testing.T) {
	toolTimings(t, 20*time.Millisecond, time.Second)
	fake := newFakeTools("pi", "0.80.0")
	daemon := newBareDaemon(t, fake)
	if got := daemon.reportedTools(context.Background(), time.Second); got == nil {
		t.Fatal("no first measurement")
	}
	time.Sleep(50 * time.Millisecond)

	// A refresh begins and reads the old version, then stalls...
	gate := fake.holdProbes()
	_ = daemon.reportedTools(context.Background(), 0)
	waitFor(t, time.Second, func() bool { return fake.probeCount() == 2 })
	// ...while the upgrade finishes and is recorded...
	daemon.noteToolVersion("pi", "0.90.2")
	// ...and only then does the old measurement come back, still saying 0.80.0.
	close(gate)
	waitFor(t, time.Second, func() bool {
		daemon.tools.mu.Lock()
		defer daemon.tools.mu.Unlock()
		return daemon.tools.refreshing == nil
	})
	daemon.tools.mu.Lock()
	version := daemon.tools.statuses[0].Version
	daemon.tools.mu.Unlock()
	if version != "0.90.2" {
		t.Fatalf("a measurement that began before the upgrade put %q back", version)
	}
}

func upgradeVia(t *testing.T, registry *hub.Registry, agentID, tool string) (map[string]any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := hub.NewDispatcher(registry, "token").AgentToolUpgrade(ctx, agentID, tool)
	if err != nil {
		return nil, err
	}
	var report map[string]any
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("report %s: %v", raw, err)
	}
	return report, nil
}

func TestAgentdUpgradesAToolOnRequest(t *testing.T) {
	toolTimings(t, time.Hour, 2*time.Second)
	fake := newFakeTools("pi", "0.80.0", "herdr", "0.9.1")
	registry, _, stop := startConnectAgent(t, "up-agent", fake)
	defer stop()
	waitFor(t, 3*time.Second, func() bool {
		info, _ := registry.Get("up-agent")
		return len(info.Tools) == 2
	})

	report, err := upgradeVia(t, registry, "up-agent", "pi")
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if report["ok"] != true || report["status"] != "upgraded" || report["before"] != "0.80.0" || report["after"] != "0.90.2" {
		t.Fatalf("report = %v", report)
	}
	if calls := fake.upgradeCalls(); len(calls) != 1 || calls[0] != "pi" {
		t.Fatalf("upgrade calls = %v, want only pi", calls)
	}

	// The hub shows the new version at once, and the machine's own heartbeats
	// must not put the old one back.
	for i := 0; i < 30; i++ {
		info, _ := registry.Get("up-agent")
		pi, _ := toolByID(info.Tools, "pi")
		herdr, _ := toolByID(info.Tools, "herdr")
		if pi.Version != "0.90.2" || herdr.Version != "0.9.1" {
			t.Fatalf("after %d checks: pi=%q herdr=%q, want 0.90.2 and 0.9.1", i, pi.Version, herdr.Version)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgentdUpgradeFailureReachesTheHubWithItsDetail(t *testing.T) {
	toolTimings(t, time.Hour, 2*time.Second)
	fake := newFakeTools("pi", "0.80.0")
	fake.fail = true
	registry, _, stop := startConnectAgent(t, "bad-agent", fake)
	defer stop()
	waitFor(t, 3*time.Second, func() bool {
		info, _ := registry.Get("bad-agent")
		return len(info.Tools) == 1
	})
	report, err := upgradeVia(t, registry, "bad-agent", "pi")
	if err != nil {
		t.Fatalf("a machine that tried and failed must still answer: %v", err)
	}
	if report["ok"] != false || report["status"] != "failed" ||
		report["note"] != "pi 升级没有成功：exit status 1" || report["output"] != "npm ERR! network" ||
		!strings.Contains(fmt.Sprint(report["manual"]), "install.sh") {
		t.Fatalf("failure report = %v", report)
	}
	info, _ := registry.Get("bad-agent")
	if pi, _ := toolByID(info.Tools, "pi"); pi.Version != "0.80.0" {
		t.Fatalf("a failed upgrade changed the version to %q", pi.Version)
	}
}

func TestAgentdAsksAboutAToolItDoesNotHave(t *testing.T) {
	toolTimings(t, time.Hour, 2*time.Second)
	fake := newFakeTools("pi", "0.80.0")
	registry, _, stop := startConnectAgent(t, "miss-agent", fake)
	defer stop()
	waitFor(t, 3*time.Second, func() bool { info, _ := registry.Get("miss-agent"); return len(info.Tools) == 1 })
	report, err := upgradeVia(t, registry, "miss-agent", "opencode")
	if err != nil {
		t.Fatal(err)
	}
	if report["ok"] != false || report["status"] != "not-installed" {
		t.Fatalf("report = %v", report)
	}
}

func TestAgentdWithoutAToolkitRefusesUpgrades(t *testing.T) {
	registry, _, stop := startConnectAgent(t, "old-agent", nil)
	defer stop()
	waitFor(t, 2*time.Second, func() bool { _, ok := registry.Get("old-agent"); return ok })
	report, err := upgradeVia(t, registry, "old-agent", "pi")
	if err != nil {
		t.Fatal(err)
	}
	if report["ok"] != false || report["status"] != "unsupported" || !strings.Contains(fmt.Sprint(report["note"]), "更新 homer") {
		t.Fatalf("report = %v", report)
	}
}

// An upgrade takes much longer than the budget a status or diff gets. A task
// that is cut off at the ordinary limit would report a failure for work the
// machine goes on to finish.
func TestToolUpgradeTaskIsNotHeldToTheOrdinaryReportBudget(t *testing.T) {
	fake := newFakeTools("pi", "0.80.0")
	fake.delay = 200 * time.Millisecond
	daemon := New(Config{AgentID: "slow", ConnectURL: "http://127.0.0.1:1", Token: "t", ReportTimeout: 30 * time.Millisecond}, newRecordingExecutor())
	daemon.SetToolkit(fake.toolkit())

	result := daemon.execute(context.Background(), hub.Task{TaskID: "t1", Kind: hub.TaskKindToolUpgrade, Options: hub.TaskOptions{Tool: "pi"}})
	if !result.OK || result.Error != "" {
		t.Fatalf("result = %+v; the upgrade was cut off at the ordinary budget", result)
	}
	var report toolctl.UpgradeResult
	if err := json.Unmarshal(result.Report, &report); err != nil || report.After != "0.90.2" {
		t.Fatalf("report = %s (%v)", result.Report, err)
	}
	// The control: an ordinary task with the same budget does time out.
	exec := newRecordingExecutor()
	exec.blockStatus = true
	control := New(Config{AgentID: "slow", ConnectURL: "http://127.0.0.1:1", Token: "t", ReportTimeout: 30 * time.Millisecond}, exec)
	if res := control.execute(context.Background(), hub.Task{TaskID: "t2", Kind: hub.TaskKindStatus}); res.OK || !strings.Contains(res.Error, "timeout") {
		t.Fatalf("control status task = %+v, want a timeout", res)
	}
}

func TestToolUpgradeBudgetFitsInsideTheHubsPatience(t *testing.T) {
	if ToolUpgradeBudget <= toolctl.UpgradeTimeout {
		t.Fatalf("budget %v leaves no room for the version checks around a %v upgrade", ToolUpgradeBudget, toolctl.UpgradeTimeout)
	}
	if ToolUpgradeBudget >= hub.ToolUpgradeWait {
		t.Fatalf("budget %v is not below the hub's wait of %v: the machine's own timeout report would arrive too late", ToolUpgradeBudget, hub.ToolUpgradeWait)
	}
}

// If the hub, or the browser behind it, hangs up halfway, an installer that
// is killed with it can leave the program broken. The listen-mode handler
// therefore runs the upgrade detached from the request.
func TestUpgradeToolLocalSurvivesTheCallerHangingUp(t *testing.T) {
	fake := newFakeTools("pi", "0.80.0")
	fake.hold = make(chan struct{})
	daemon := newBareDaemon(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		raw json.RawMessage
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		raw, err := daemon.upgradeToolLocal(ctx, "pi")
		done <- outcome{raw, err}
	}()
	waitFor(t, time.Second, func() bool { return len(fake.upgradeCalls()) == 1 })
	cancel() // the caller hangs up
	time.Sleep(50 * time.Millisecond)
	close(fake.hold)

	select {
	case got := <-done:
		if got.err != nil || !strings.Contains(string(got.raw), `"upgraded"`) {
			t.Fatalf("result = %s, %v", got.raw, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upgradeToolLocal never returned")
	}
	if n, ctxErr := fake.finishedUpgrade(); n != 1 || ctxErr != nil {
		t.Fatalf("upgrade finished %d time(s) with context error %v; it must run to completion", n, ctxErr)
	}
}

func TestListenAgentReportsToolsAndServesUpgrades(t *testing.T) {
	toolTimings(t, time.Hour, 2*time.Second)
	registry := hub.NewRegistry()
	hubServer := httptest.NewServer(hub.NewAgentAPI(registry, "token"))
	defer hubServer.Close()
	listenAddr := freeTCPAddress(t)
	fake := newFakeTools("pi", "0.80.0")
	cfg := Config{HomerHome: t.TempDir(), Token: "token", ListenAddr: listenAddr, AdvertiseURL: "http://" + listenAddr, HubURL: hubServer.URL, AgentID: "listen-tools"}
	daemon := New(cfg, newRecordingExecutor())
	daemon.SetToolkit(fake.toolkit())
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- daemon.Run(ctx) }()
	defer stopDaemon(t, cancel, runDone)

	waitFor(t, 3*time.Second, func() bool {
		info, ok := registry.Get(cfg.AgentID)
		return ok && len(info.Tools) == 1
	})

	// The hub dials the machine: through the real dispatcher, which finds the
	// listen address in the registry.
	report, err := upgradeVia(t, registry, cfg.AgentID, "pi")
	if err != nil {
		t.Fatalf("upgrade over the listen port: %v", err)
	}
	if report["ok"] != true || report["after"] != "0.90.2" {
		t.Fatalf("report = %v", report)
	}
	info, _ := registry.Get(cfg.AgentID)
	if pi, _ := toolByID(info.Tools, "pi"); pi.Version != "0.90.2" {
		t.Fatalf("pi version after the upgrade = %q", pi.Version)
	}

	// The listen port refuses a caller without the machine credential.
	request, err := http.NewRequest(http.MethodPost, "http://"+listenAddr+"/api/tools/upgrade", strings.NewReader(`{"tool":"pi"}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous upgrade = %d, want 401", response.StatusCode)
	}
	if n := len(fake.upgradeCalls()); n != 1 {
		t.Fatalf("an unauthenticated request ran an upgrade (%d calls)", n)
	}
}
