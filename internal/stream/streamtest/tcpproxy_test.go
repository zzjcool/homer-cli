package streamtest

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func startTCPEcho(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("echo listener did not stop")
		}
	})
	return listener.Addr().String(), done
}

func TestTCPProxyRoundTripDelayAndGracefulClose(t *testing.T) {
	target, _ := startTCPEcho(t)
	proxy, err := NewTCPProxy(target)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.CloseGraceful()
	proxy.Delay(25 * time.Millisecond)
	conn, err := net.DialTimeout("tcp", proxy.Addr(), time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	started := time.Now()
	message := "round-trip"
	if _, err := io.WriteString(conn, message); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := make([]byte, len(message))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(got) != message {
		t.Fatalf("echo = %q", got)
	}
	if time.Since(started) < 20*time.Millisecond {
		t.Fatalf("Delay was not applied: elapsed %v", time.Since(started))
	}
	if err := proxy.CloseGraceful(); err != nil {
		t.Fatalf("CloseGraceful: %v", err)
	}
}

func TestTCPProxyBlackhole(t *testing.T) {
	target, _ := startTCPEcho(t)
	proxy, err := NewTCPProxy(target)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.CloseGraceful()
	conn, err := net.DialTimeout("tcp", proxy.Addr(), time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	proxy.Blackhole()
	_ = conn.SetDeadline(time.Now().Add(40 * time.Millisecond))
	if _, err := io.WriteString(conn, "dropped"); err != nil {
		t.Fatalf("Write to blackhole: %v", err)
	}
	buffer := make([]byte, 7)
	if _, err := io.ReadFull(conn, buffer); err == nil || !strings.Contains(strings.ToLower(err.Error()), "timeout") {
		t.Fatalf("Read from blackhole error = %v, want timeout", err)
	}
}

func TestTCPProxyCut(t *testing.T) {
	target, _ := startTCPEcho(t)
	proxy, err := NewTCPProxy(target)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.CloseGraceful()
	conn, err := net.DialTimeout("tcp", proxy.Addr(), time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, "before cut"); err != nil {
		t.Fatalf("Write before Cut: %v", err)
	}
	buffer := make([]byte, len("before cut"))
	if _, err := io.ReadFull(conn, buffer); err != nil {
		t.Fatalf("Read before Cut: %v", err)
	}
	proxy.Cut()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, "after cut"); err == nil {
		read := make([]byte, 1)
		_, err = conn.Read(read)
		if err == nil {
			t.Fatal("connection remained open after Cut")
		}
	}
}
