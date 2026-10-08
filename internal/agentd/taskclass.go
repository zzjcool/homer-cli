package agentd

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/hub"
)

type taskClass uint8

const (
	taskClassRead taskClass = iota + 1
	taskClassWrite
)

func classifyTask(method string, options hub.TaskOptions) taskClass {
	if options.Resolve == "local" || options.Resolve == "center" {
		return taskClassWrite
	}
	switch method {
	case string(hub.TaskKindStatus), string(hub.TaskKindDiff), hub.MethodInspect:
		return taskClassRead
	case string(hub.TaskKindSecret):
		switch options.SecretAction {
		case "list", "exists", "status":
			return taskClassRead
		default:
			return taskClassWrite
		}
	default:
		return taskClassWrite
	}
}

type taskWaiter struct {
	ready     chan struct{}
	granted   bool
	onGranted func()
}

// taskGate is a context-cancellable FIFO mutex for machine-mutating tasks.
type taskGate struct {
	mu      sync.Mutex
	held    bool
	waiters []*taskWaiter
}

func newTaskGate() *taskGate { return &taskGate{} }

func (g *taskGate) acquire(ctx context.Context, queued func()) (func(), bool, error) {
	return g.acquireWithGrant(ctx, queued, nil)
}

func (g *taskGate) acquireWithGrant(ctx context.Context, queued, onGranted func()) (func(), bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	g.mu.Lock()
	if !g.held && len(g.waiters) == 0 {
		g.held = true
		if onGranted != nil {
			onGranted()
		}
		g.mu.Unlock()
		return g.releaseFunc(), false, nil
	}
	waiter := &taskWaiter{ready: make(chan struct{}), onGranted: onGranted}
	g.waiters = append(g.waiters, waiter)
	g.mu.Unlock()
	if queued != nil {
		queued()
	}

	select {
	case <-waiter.ready:
		if err := ctx.Err(); err != nil {
			g.release()
			return nil, true, err
		}
		return g.releaseFunc(), true, nil
	case <-ctx.Done():
		g.mu.Lock()
		if waiter.granted {
			g.mu.Unlock()
			g.release()
			return nil, true, ctx.Err()
		}
		for i, pending := range g.waiters {
			if pending == waiter {
				copy(g.waiters[i:], g.waiters[i+1:])
				g.waiters[len(g.waiters)-1] = nil
				g.waiters = g.waiters[:len(g.waiters)-1]
				break
			}
		}
		g.mu.Unlock()
		return nil, true, ctx.Err()
	}
}

func (g *taskGate) releaseFunc() func() {
	var once sync.Once
	return func() { once.Do(g.release) }
}

func (g *taskGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for len(g.waiters) > 0 {
		waiter := g.waiters[0]
		copy(g.waiters, g.waiters[1:])
		g.waiters[len(g.waiters)-1] = nil
		g.waiters = g.waiters[:len(g.waiters)-1]
		if waiter == nil {
			continue
		}
		waiter.granted = true
		if waiter.onGranted != nil {
			waiter.onGranted()
		}
		close(waiter.ready)
		return
	}
	g.held = false
}

func (g *taskGate) waiting() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.waiters)
}

// statusFlight shares concurrent scans only within the same write generation.
// A completed write increments writeGen, so new readers cannot join an older
// scan that may have observed pre-write state.
type statusFlight struct {
	mu        sync.Mutex
	writeGen  uint64
	scanEpoch uint64
	flights   map[uint64]*statusFlightCall
}

type statusFlightCall struct {
	done       chan struct{}
	waiters    int
	generation uint64
	report     commands.StatusReport
	err        error
}

func (s *statusFlight) waiting() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int
	for _, call := range s.flights {
		total += call.waiters
	}
	return total
}

func (d *Daemon) beginWriteGen() {
	d.statusFlight.mu.Lock()
	// A scan that started before this write cannot be joined once the write
	// begins. scanEpoch is separate so writeGen still increments only when the
	// write has finished.
	d.statusFlight.scanEpoch++
	d.statusFlight.mu.Unlock()
}

func (d *Daemon) bumpWriteGen() {
	d.statusFlight.mu.Lock()
	d.statusFlight.writeGen++
	d.statusFlight.scanEpoch++
	d.statusFlight.mu.Unlock()
}

func (d *Daemon) statusReport(ctx context.Context) (commands.StatusReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	d.statusFlight.mu.Lock()
	generation := d.statusFlight.writeGen
	epoch := d.statusFlight.scanEpoch
	if d.statusFlight.flights == nil {
		d.statusFlight.flights = make(map[uint64]*statusFlightCall)
	}
	call := d.statusFlight.flights[epoch]
	if call == nil {
		call = &statusFlightCall{done: make(chan struct{}), generation: generation}
		d.statusFlight.flights[epoch] = call
		go d.runStatusFlight(generation, epoch, call)
	} else if call.generation != generation {
		// Do not let a post-write reader join a scan from an earlier write
		// generation, even when both scans share the same begin-write epoch.
		call = &statusFlightCall{done: make(chan struct{}), generation: generation}
		d.statusFlight.flights[epoch] = call
		go d.runStatusFlight(generation, epoch, call)
	}
	call.waiters++
	d.statusFlight.mu.Unlock()

	select {
	case <-call.done:
		d.statusFlight.mu.Lock()
		call.waiters--
		d.statusFlight.mu.Unlock()
		return cloneStatusReport(call.report), call.err
	case <-ctx.Done():
		d.statusFlight.mu.Lock()
		call.waiters--
		d.statusFlight.mu.Unlock()
		return commands.StatusReport{}, ctx.Err()
	}
}

func (d *Daemon) runStatusFlight(generation, epoch uint64, call *statusFlightCall) {
	ctx, cancel := context.WithTimeout(context.Background(), DriftTimeout)
	defer cancel()
	report, err := d.executorStatus(ctx)
	if ctx.Err() != nil && err == nil {
		err = ctx.Err()
	}
	if err == nil {
		drift := driftFromStatus(report)
		d.statusFlight.mu.Lock()
		current := d.statusFlight.writeGen == generation && d.statusFlight.scanEpoch == epoch
		if current {
			d.driftMu.Lock()
			d.lastDrift = cloneDrift(drift)
			d.lastDriftAt = time.Now()
			d.driftMu.Unlock()
		}
		d.statusFlight.mu.Unlock()
		if current {
			d.signalHeartbeat()
		}
	}
	d.statusFlight.mu.Lock()
	call.report = cloneStatusReport(report)
	call.err = err
	if d.statusFlight.flights[epoch] == call {
		delete(d.statusFlight.flights, epoch)
	}
	close(call.done)
	d.statusFlight.mu.Unlock()
}

func cloneStatusReport(report commands.StatusReport) commands.StatusReport {
	encoded, err := json.Marshal(report)
	if err != nil {
		return report
	}
	var clone commands.StatusReport
	if json.Unmarshal(encoded, &clone) != nil {
		return report
	}
	return clone
}
