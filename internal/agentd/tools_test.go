package agentd

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/toolctl"
)

type fakeTools struct {
	mu           sync.Mutex
	versions     map[string]string
	upgrades     []string
	probeN       int
	gate         chan struct{}
	upgradeTo    string
	upgradeDelay time.Duration
}

func newFakeTools(versions ...string) *fakeTools {
	fake := &fakeTools{versions: make(map[string]string), upgradeTo: "0.90.2"}
	for i := 0; i+1 < len(versions); i += 2 {
		fake.versions[versions[i]] = versions[i+1]
	}
	return fake
}

func (f *fakeTools) toolkit() Toolkit { return Toolkit{Probe: f.probe, Upgrade: f.upgrade} }
func (f *fakeTools) probe(ctx context.Context) []toolctl.Status {
	f.mu.Lock()
	f.probeN++
	gate := f.gate
	versions := make(map[string]string, len(f.versions))
	for id, version := range f.versions {
		versions[id] = version
	}
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil
		}
	}
	out := make([]toolctl.Status, 0, len(versions))
	for id, version := range versions {
		out = append(out, toolctl.Status{ID: id, Adapter: id, Label: id, Version: version, Upgradable: true})
	}
	return out
}
func (f *fakeTools) upgrade(ctx context.Context, id string) toolctl.UpgradeResult {
	f.mu.Lock()
	f.upgrades = append(f.upgrades, id)
	before, installed := f.versions[id]
	delay, to := f.upgradeDelay, f.upgradeTo
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-ctx.Done():
			return toolctl.UpgradeResult{Tool: id, Status: "canceled"}
		case <-time.After(delay):
		}
	}
	if !installed {
		return toolctl.UpgradeResult{Tool: id, Status: "not-installed"}
	}
	f.mu.Lock()
	f.versions[id] = to
	f.mu.Unlock()
	return toolctl.UpgradeResult{OK: true, Tool: id, Status: "upgraded", Before: before, After: to, Note: fmt.Sprintf("%s upgraded", id)}
}
func (f *fakeTools) setVersion(id, version string) {
	f.mu.Lock()
	f.versions[id] = version
	f.mu.Unlock()
}
func (f *fakeTools) probeCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.probeN }
func (f *fakeTools) upgradeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.upgrades...)
}

func toolTimings(t *testing.T, interval time.Duration) {
	t.Helper()
	oldInterval := toolInterval
	toolInterval = interval
	t.Cleanup(func() { toolInterval = oldInterval })
}

func toolByID(statuses []toolctl.Status, id string) (toolctl.Status, bool) {
	for _, status := range statuses {
		if status.ID == id {
			return status, true
		}
	}
	return toolctl.Status{}, false
}

func TestDaemonToolCacheAndCompletionHeartbeat(t *testing.T) {
	toolTimings(t, time.Hour)
	fake := newFakeTools("pi", "0.80.0")
	d := New(Config{AgentID: "tools"}, &testExecutor{})
	d.SetToolkit(fake.toolkit())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := d.reportedTools(ctx, time.Second)
	if first == nil || len(*first) != 1 || (*first)[0].Version != "0.80.0" {
		t.Fatalf("first tool report = %v", first)
	}
	if count := fake.probeCount(); count != 1 {
		t.Fatalf("probes = %d, want one", count)
	}
	fake.setVersion("pi", "0.85.0")
	if got := d.reportedTools(ctx, 0); got == nil || (*got)[0].Version != "0.80.0" {
		t.Fatalf("fresh tool cache unexpectedly changed: %v", got)
	}
	d.noteToolVersion("pi", "0.90.2")
	if got := d.reportedTools(ctx, 0); got == nil || (*got)[0].Version != "0.90.2" {
		t.Fatalf("patched tool cache = %v, want immediate upgrade version", got)
	}
	if d.heartbeatWake == nil {
		t.Fatal("tool update must wake the independent heartbeat loop")
	}
}

func TestDaemonToolProbeSingleflight(t *testing.T) {
	toolTimings(t, 0)
	fake := newFakeTools("pi", "0.80.0")
	fake.gate = make(chan struct{})
	d := New(Config{AgentID: "probe-flight"}, &testExecutor{})
	d.SetToolkit(fake.toolkit())
	for i := 0; i < 8; i++ {
		if got := d.reportedTools(context.Background(), 0); got != nil {
			t.Fatalf("blocked initial probe returned %v", got)
		}
	}
	waitFor(t, time.Second, func() bool { return fake.probeCount() == 1 })
	close(fake.gate)
	waitFor(t, time.Second, func() bool { return d.cachedTools() != nil })
}
