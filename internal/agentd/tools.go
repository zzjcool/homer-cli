package agentd

import (
	"context"
	"sync"
	"time"

	"github.com/zzjcool/homer-cli/internal/toolctl"
)

// Toolkit is how the daemon looks at, and upgrades, the programs the adapters
// drive on this machine (pi, herdr, opencode...). The zero Toolkit does
// neither: such a daemon reports no versions and refuses upgrade requests.
// Production wires LocalToolkit; tests wire fakes so they never run, or
// depend on, whatever happens to be installed on the machine they run on.
type Toolkit struct {
	// Probe measures every registered program this machine has.
	Probe func(ctx context.Context) []toolctl.Status
	// Upgrade upgrades the program registered under id.
	Upgrade func(ctx context.Context, id string) toolctl.UpgradeResult
}

// LocalToolkit looks at the programs on this machine's login PATH.
func LocalToolkit() Toolkit {
	return Toolkit{Probe: toolctl.ProbeLocal, Upgrade: toolctl.UpgradeLocal}
}

// SetToolkit chooses how this daemon measures and upgrades programs. Call it
// before Run.
func (d *Daemon) SetToolkit(toolkit Toolkit) {
	if d == nil {
		return
	}
	d.toolkit = toolkit
}

var (
	// toolInterval is how long a measured version is reused. Measuring starts
	// a few Node based programs; once every few minutes is plenty for a
	// console that shows "which version is this machine on".
	toolInterval = 5 * time.Minute
	// toolProbeBudget bounds one measurement of every program.
	toolProbeBudget = toolctl.ProbeTimeout + 10*time.Second
)

// ToolUpgradeBudget bounds one tool-upgrade task on the agent: the version
// check before, the upgrade command, and the version check after. It stays
// below hub.ToolUpgradeWait so the machine's own report of a timeout reaches
// the hub before the hub stops waiting.
const ToolUpgradeBudget = toolctl.UpgradeTimeout + 2*toolctl.ProbeTimeout + 20*time.Second

// toolState is the daemon's memory of the last measurement.
type toolState struct {
	mu       sync.Mutex
	statuses []toolctl.Status
	// known is true once a measurement finished, even one that found nothing:
	// "nothing installed" is a result, not an absence of one.
	known    bool
	probedAt time.Time
	// generation counts the upgrades noted so far. A measurement that began
	// before an upgrade finished may have read the program mid-change, so it
	// is dropped rather than allowed to overwrite what the upgrade recorded.
	generation int
	// refreshing is non-nil while a measurement runs, and closed when it ends.
	refreshing chan struct{}
}

// reportedTools is the tool list to put in a heartbeat, or nil when there is
// nothing to say yet.
//
// It never blocks a heartbeat on a slow program: a measurement older than
// toolInterval is refreshed in the background and the last result is sent
// meanwhile. Only when there is no result at all does it wait, and then for
// no longer than wait.
func (d *Daemon) reportedTools(ctx context.Context, wait time.Duration) *[]toolctl.Status {
	if d == nil || d.toolkit.Probe == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	state := &d.tools
	state.mu.Lock()
	fresh := state.known && time.Since(state.probedAt) < toolInterval
	var running chan struct{}
	if !fresh {
		if state.refreshing == nil {
			state.refreshing = make(chan struct{})
			go d.measureTools(ctx, state.refreshing, state.generation)
		}
		running = state.refreshing
	}
	known := state.known
	state.mu.Unlock()

	if !fresh && !known && wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-running:
		case <-timer.C:
		case <-ctx.Done():
		}
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.known {
		return nil
	}
	out := make([]toolctl.Status, 0, len(state.statuses))
	out = append(out, state.statuses...)
	return &out
}

// measureTools runs one measurement and records it. A measurement cut short
// by shutdown or by its budget is dropped: half a list would read as
// "the rest was uninstalled". So is one that an upgrade overtook.
func (d *Daemon) measureTools(parent context.Context, done chan struct{}, generation int) {
	ctx, cancel := context.WithTimeout(parent, toolProbeBudget)
	statuses := d.toolkit.Probe(ctx)
	complete := ctx.Err() == nil
	cancel()

	state := &d.tools
	state.mu.Lock()
	defer state.mu.Unlock()
	if complete && generation == state.generation {
		state.statuses = append(make([]toolctl.Status, 0, len(statuses)), statuses...)
		state.known = true
		state.probedAt = time.Now()
	}
	state.refreshing = nil
	close(done)
	updated := complete && generation == state.generation
	if updated {
		d.signalHeartbeat()
	}
}

// noteToolVersion records the version a tool has just been measured at, so the
// next heartbeat does not put the old one back, and schedules a fresh
// measurement of everything.
func (d *Daemon) noteToolVersion(id, version string) {
	if d == nil {
		return
	}
	state := &d.tools
	state.mu.Lock()
	defer state.mu.Unlock()
	state.generation++
	if version != "" {
		for i := range state.statuses {
			if state.statuses[i].ID == id {
				state.statuses[i].Version = version
				state.statuses[i].Error = ""
			}
		}
	}
	// Keep the patched list to send meanwhile, but ask for a re-measurement:
	// the upgrade may have installed something that was not there before.
	state.probedAt = time.Time{}
	d.signalHeartbeat()
}

// runToolUpgrade upgrades one program on this machine. It reports in the same
// shape whether or not it could try.
func (d *Daemon) runToolUpgrade(ctx context.Context, id string) toolctl.UpgradeResult {
	if d == nil || d.toolkit.Upgrade == nil {
		return toolctl.UpgradeResult{
			Status: "unsupported",
			Tool:   id,
			Note:   "这台机器上的 homer 不能升级应用，请先更新 homer。",
		}
	}
	result := d.toolkit.Upgrade(ctx, id)
	d.noteToolVersion(result.Tool, result.After)
	return result
}
