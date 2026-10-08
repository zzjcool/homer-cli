package agentd

import (
	"context"
	"time"

	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/toolctl"
	"github.com/zzjcool/homer-cli/internal/web"
)

const heartbeatJitter = 0.2

// heartbeatLoop has its own goroutine and session event queue. It does not
// acquire task semaphores, and sends only immutable cached snapshots. Status
// and tool refreshes run in independent background work.
func (d *Daemon) heartbeatLoop(ctx context.Context, session *stream.Session) {
	if ctx == nil {
		ctx = context.Background()
	}
	if session == nil {
		return
	}
	timer := time.NewTimer(d.nextHeartbeatDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-session.Done():
			return
		case <-d.heartbeatWake:
			d.sendHeartbeat(ctx, session)
			d.refreshHeartbeatCaches(ctx)
			timer.Reset(d.nextHeartbeatDelay())
		case <-timer.C:
			d.sendHeartbeat(ctx, session)
			d.refreshHeartbeatCaches(ctx)
			timer.Reset(d.nextHeartbeatDelay())
		}
	}
}

func (d *Daemon) nextHeartbeatDelay() time.Duration {
	interval := d.heartbeatEvery
	if interval <= 0 {
		interval = DriftInterval
	}
	factor := 1 - heartbeatJitter + 2*heartbeatJitter*randomFloat64()
	jittered := time.Duration(float64(interval) * factor)
	if jittered <= 0 {
		return time.Nanosecond
	}
	return jittered
}

func (d *Daemon) sendHeartbeat(ctx context.Context, session *stream.Session) {
	if d == nil || session == nil {
		return
	}
	params := hub.HeartbeatParams{
		Version: web.Version,
		Drift:   d.cachedDrift(),
		Host:    d.cachedHost(),
		Tools:   d.cachedTools(),
	}
	if err := session.Notify(ctx, hub.MethodHeartbeat, params); err != nil && ctx.Err() == nil {
		// The Session owns transport-error reporting/reconnect. Do not stall the
		// heartbeat goroutine trying a different path or waiting for a task.
		d.logf("心跳事件发送失败（随会话重连）: %v", err)
	}
}

func (d *Daemon) refreshHeartbeatCaches(ctx context.Context) {
	if d == nil {
		return
	}
	d.hostMu.Lock()
	if !d.hostProbing && (d.lastHost == nil || time.Since(d.lastHostAt) >= DriftInterval) {
		d.hostProbing = true
		d.hostMu.Unlock()
		go func() {
			d.hostSnapshot()
			d.hostMu.Lock()
			d.hostProbing = false
			d.hostMu.Unlock()
			d.signalHeartbeat()
		}()
	} else {
		d.hostMu.Unlock()
	}
	if d.toolkit.Probe != nil {
		// reportedTools(wait=0) returns immediately and starts at most one
		// background measurement for a stale/missing tool cache.
		_ = d.reportedTools(ctx, 0)
	}
	if cached := d.cachedDrift(); cached == nil || time.Since(d.driftTimestamp()) >= DriftInterval {
		go func() {
			_ = d.driftSummary(ctx)
		}()
	}
}

func (d *Daemon) driftTimestamp() time.Time {
	if d == nil {
		return time.Time{}
	}
	d.driftMu.Lock()
	defer d.driftMu.Unlock()
	return d.lastDriftAt
}

func (d *Daemon) cachedTools() *[]toolctl.Status {
	if d == nil {
		return nil
	}
	state := &d.tools
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.known {
		return nil
	}
	statuses := append([]toolctl.Status(nil), state.statuses...)
	if statuses == nil {
		statuses = []toolctl.Status{}
	}
	return &statuses
}
