package stream

import (
	"sync"
	"time"
)

// ThrottledLogger emits at most one message for each key in a window.
type ThrottledLogger struct {
	logger Logger
	window time.Duration
	mu     sync.Mutex
	last   map[string]time.Time
	clock  Clock
}

func NewThrottledLogger(logger Logger, window time.Duration) *ThrottledLogger {
	if window <= 0 {
		window = time.Minute
	}
	return &ThrottledLogger{logger: logger, window: window, last: make(map[string]time.Time), clock: realClock{}}
}

// Log prints line if key has not been emitted in the current window. If logger
// is nil it records the key but emits nothing and still returns false.
func (t *ThrottledLogger) Log(key, line string) bool {
	if t == nil {
		return false
	}
	now := t.clock.Now()
	t.mu.Lock()
	last, ok := t.last[key]
	if ok && now.Sub(last) < t.window {
		t.mu.Unlock()
		return false
	}
	t.last[key] = now
	t.mu.Unlock()
	if t.logger == nil {
		return false
	}
	t.logger.Printf("%s", line)
	return true
}
