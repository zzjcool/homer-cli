package streamtest

import (
	"sync"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
)

type fakeTimer struct {
	clock   *FakeClock
	channel chan time.Time
	due     time.Time
	active  bool
}

type fakeTicker struct {
	clock   *FakeClock
	channel chan time.Time
	next    time.Time
	period  time.Duration
	active  bool
}

// FakeClock is a manually advanced clock. Advance delivers all due timer and
// ticker notifications synchronously without creating goroutines.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  map[*fakeTimer]struct{}
	tickers map[*fakeTicker]struct{}
}

func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start, timers: make(map[*fakeTimer]struct{}), tickers: make(map[*fakeTicker]struct{})}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) NewTimer(delay time.Duration) stream.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeTimer{clock: c, channel: make(chan time.Time, 1), due: c.now.Add(maxDuration(delay, 0)), active: true}
	c.timers[timer] = struct{}{}
	if !timer.due.After(c.now) {
		timer.channel <- c.now
		timer.active = false
		delete(c.timers, timer)
	}
	return timer
}

func (c *FakeClock) NewTicker(period time.Duration) stream.Ticker {
	if period <= 0 {
		period = time.Nanosecond
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ticker := &fakeTicker{clock: c, channel: make(chan time.Time, 1), period: period, next: c.now.Add(period), active: true}
	c.tickers[ticker] = struct{}{}
	return ticker
}

// PendingTimers reports the number of timers which have not fired or stopped.
// It is primarily useful for synchronizing deterministic heartbeat tests.
func (c *FakeClock) PendingTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for timer := range c.timers {
		if timer.active {
			count++
		}
	}
	return count
}

func (c *FakeClock) Advance(d time.Duration) {
	if d < 0 {
		return
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	for timer := range c.timers {
		if timer.active && !timer.due.After(now) {
			timer.active = false
			delete(c.timers, timer)
			select {
			case timer.channel <- timer.due:
			default:
			}
		}
	}
	for ticker := range c.tickers {
		if !ticker.active || ticker.next.After(now) {
			continue
		}
		tick := ticker.next
		select {
		case ticker.channel <- tick:
		default:
		}
		elapsed := now.Sub(ticker.next)
		steps := elapsed/ticker.period + 1
		ticker.next = ticker.next.Add(steps * ticker.period)
	}
	c.mu.Unlock()
}

func (t *fakeTimer) C() <-chan time.Time { return t.channel }
func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	t.active = false
	delete(t.clock.timers, t)
	return wasActive
}
func (t *fakeTimer) Reset(delay time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	select {
	case <-t.channel:
	default:
	}
	t.due = t.clock.now.Add(maxDuration(delay, 0))
	t.active = true
	if !t.due.After(t.clock.now) {
		t.channel <- t.clock.now
		t.active = false
		delete(t.clock.timers, t)
	} else {
		t.clock.timers[t] = struct{}{}
	}
	return wasActive
}

func (t *fakeTicker) C() <-chan time.Time { return t.channel }
func (t *fakeTicker) Stop() {
	t.clock.mu.Lock()
	t.active = false
	delete(t.clock.tickers, t)
	t.clock.mu.Unlock()
}

func maxDuration(value, minimum time.Duration) time.Duration {
	if value < minimum {
		return minimum
	}
	return value
}

var _ stream.Clock = (*FakeClock)(nil)
var _ stream.Timer = (*fakeTimer)(nil)
var _ stream.Ticker = (*fakeTicker)(nil)
