package streamtest

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
)

func TestPipeOptionsAndClose(t *testing.T) {
	a, b := Pipe(PipeOptions{AtoB: PipeDirection{Latency: time.Millisecond, ReadLimit: 4}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Write(ctx, []byte("ok")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, err := b.Read(ctx); err != nil || string(got) != "ok" {
		t.Fatalf("Read = %q, %v", got, err)
	}
	if err := a.Write(ctx, []byte("oversized")); err != nil {
		t.Fatalf("oversize Write: %v", err)
	}
	if _, err := b.Read(ctx); err == nil {
		t.Fatal("oversize Read succeeded")
	} else {
		var protocolErr *stream.ProtocolError
		if !errors.As(err, &protocolErr) || protocolErr.Code != stream.CloseTooBig {
			t.Fatalf("oversize Read error = %T %v", err, err)
		}
		_ = a.CloseNow()
		a, b = Pipe(PipeOptions{})
	}
	if err := a.Close(stream.CloseSuperseded, "taken over"); err != nil {
		t.Fatal(err)
	}
	_, err := b.Read(ctx)
	var closeErr *stream.CloseError
	if !errors.As(err, &closeErr) || !closeErr.Remote || closeErr.Code != stream.CloseSuperseded || closeErr.Reason != "taken over" {
		t.Fatalf("remote close = %T %v", err, err)
	}
	_ = a.CloseNow()
	_ = b.CloseNow()
}

func TestPipeDropAndHalfOpen(t *testing.T) {
	for name, option := range map[string]PipeDirection{
		"drop":        {Drop: true},
		"half-open":   {HalfOpen: true},
		"every-other": {DropEvery: 2},
	} {
		t.Run(name, func(t *testing.T) {
			a, b := Pipe(PipeOptions{AtoB: option})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if err := a.Write(ctx, []byte("lost")); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if name == "every-other" {
				if got, err := b.Read(ctx); err != nil || string(got) != "lost" {
					t.Fatalf("first Read = %q, %v", got, err)
				}
				if err := a.Write(ctx, []byte("dropped")); err != nil {
					t.Fatalf("second Write: %v", err)
				}
				if _, err := b.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("second Read error = %v, want context deadline", err)
				}
				return
			}
			if _, err := b.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Read error = %v, want context deadline", err)
			}
			_ = a.CloseNow()
			_ = b.CloseNow()
		})
	}
}

func TestFaultConn(t *testing.T) {
	a, b := Pipe(PipeOptions{})
	wrapped := &FaultConn{Conn: a, WriteDelay: time.Millisecond, ReadLimit: 2}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := wrapped.Write(ctx, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Read(ctx); err != nil || string(got) != "ok" {
		t.Fatalf("peer Read = %q, %v", got, err)
	}
	if err := b.Write(ctx, []byte("long")); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Read(ctx); err == nil {
		t.Fatal("FaultConn read limit did not fail")
	}
	if _, writes := wrapped.Counts(); writes != 1 {
		t.Fatalf("write count = %d", writes)
	}
	if err := wrapped.CloseNow(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("CloseNow: %v", err)
	}
	_ = b.CloseNow()
}

func TestFakeClock(t *testing.T) {
	clock := NewFakeClock(time.Unix(100, 0))
	timer := clock.NewTimer(5 * time.Second)
	ticker := clock.NewTicker(2 * time.Second)
	clock.Advance(4 * time.Second)
	select {
	case <-timer.C():
		t.Fatal("timer fired before due")
	default:
	}
	if got := <-ticker.C(); !got.Equal(time.Unix(102, 0)) {
		t.Fatalf("ticker time = %v", got)
	}
	clock.Advance(time.Second)
	if got := <-timer.C(); !got.Equal(time.Unix(105, 0)) {
		t.Fatalf("timer time = %v", got)
	}
	if timer.Stop() {
		t.Fatal("already fired timer Stop returned true")
	}
	ticker.Stop()
}

func TestFakeClockTimerReset(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	timer := clock.NewTimer(time.Second)
	clock.Advance(time.Second)
	<-timer.C()
	timer.Reset(time.Second)
	clock.Advance(time.Second)
	select {
	case <-timer.C():
	default:
		t.Fatal("reset timer did not fire")
	}
}

func TestPipeOperationsHonorContext(t *testing.T) {
	a, b := Pipe(PipeOptions{Buffer: 2})
	if err := a.Write(context.Background(), []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := a.Write(context.Background(), []byte("second")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := a.Write(ctx, []byte("blocked")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked Write error = %v", err)
	}
	_ = a.CloseNow()
	_ = b.CloseNow()
}
