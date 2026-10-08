package stream_test

import (
	"math"
	"sync"
	"testing"
	"time"

	. "github.com/zzjcool/homer-cli/internal/stream"
)

func TestBackoff(t *testing.T) {
	backoff := Backoff{Base: time.Second, Max: 8 * time.Second, Factor: 2, Jitter: 0.5}
	if got := backoff.Next(0, func() float64 { return 0 }); got != time.Second {
		t.Fatalf("base backoff = %v", got)
	}
	if got := backoff.Next(2, func() float64 { return 1 }); got != 2*time.Second {
		t.Fatalf("jittered backoff = %v, want 2s", got)
	}
	if got := backoff.Next(10, func() float64 { return 0 }); got != 8*time.Second {
		t.Fatalf("capped backoff = %v", got)
	}
	if got := backoff.Next(1, func() float64 { return math.NaN() }); got != 2*time.Second {
		t.Fatalf("NaN random backoff = %v", got)
	}
}

func TestThrottledLogger(t *testing.T) {
	logger := &testLogger{}
	throttled := NewThrottledLogger(logger, time.Minute)
	if !throttled.Log("same", "first") {
		t.Fatal("first message was suppressed")
	}
	if throttled.Log("same", "second") {
		t.Fatal("same key was not throttled")
	}
	if !throttled.Log("other", "third") {
		t.Fatal("different key was suppressed")
	}
	if logger.count() != 2 {
		t.Fatalf("logger count = %d, want 2", logger.count())
	}
}

type testLogger struct {
	mu sync.Mutex
	n  int
}

func (l *testLogger) Printf(string, ...any) { l.mu.Lock(); l.n++; l.mu.Unlock() }
func (l *testLogger) count() int            { l.mu.Lock(); defer l.mu.Unlock(); return l.n }
