package stream

import "time"

type realClock struct{}

type realTimer struct{ timer *time.Timer }
type realTicker struct{ ticker *time.Ticker }

func (realClock) Now() time.Time                 { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer { return &realTimer{timer: time.NewTimer(d)} }
func (realClock) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		d = time.Nanosecond
	}
	return &realTicker{ticker: time.NewTicker(d)}
}
func (t *realTimer) C() <-chan time.Time        { return t.timer.C }
func (t *realTimer) Stop() bool                 { return t.timer.Stop() }
func (t *realTimer) Reset(d time.Duration) bool { return t.timer.Reset(d) }
func (t *realTicker) C() <-chan time.Time       { return t.ticker.C }
func (t *realTicker) Stop()                     { t.ticker.Stop() }

func signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
