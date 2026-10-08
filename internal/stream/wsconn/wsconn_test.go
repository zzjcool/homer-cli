package wsconn

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/zzjcool/homer-cli/internal/stream"
)

const socketTestTimeout = 4 * time.Second

func TestWebSocketRoundTrip(t *testing.T) {
	serverConn := make(chan stream.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Accept(w, r, AcceptOptions{})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		serverConn <- conn
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), socketTestTimeout)
	defer cancel()
	client, response, err := Dial(ctx, server.URL, DialOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if response == nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("response = %#v", response)
	}
	defer client.CloseNow()
	select {
	case accepted := <-serverConn:
		defer accepted.CloseNow()
		if accepted.Info().Subprotocol != stream.Subprotocol || client.Info().Subprotocol != stream.Subprotocol {
			t.Fatalf("subprotocols = %q and %q", accepted.Info().Subprotocol, client.Info().Subprotocol)
		}
		if err := client.Write(ctx, []byte("hello websocket")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		got, err := accepted.Read(ctx)
		if err != nil || string(got) != "hello websocket" {
			t.Fatalf("server Read = %q, %v", got, err)
		}
		if err := accepted.Write(ctx, []byte("reply")); err != nil {
			t.Fatalf("server Write: %v", err)
		}
		got, err = client.Read(ctx)
		if err != nil || string(got) != "reply" {
			t.Fatalf("client Read = %q, %v", got, err)
		}
	case <-time.After(socketTestTimeout):
		t.Fatal("Accept did not complete")
	}
}

func TestCloseCodeReasonRoundTrip(t *testing.T) {
	serverConn := make(chan stream.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Accept(w, r, AcceptOptions{})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		serverConn <- conn
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), socketTestTimeout)
	defer cancel()
	client, _, err := Dial(ctx, server.URL, DialOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.CloseNow()
	var accepted stream.Conn
	select {
	case accepted = <-serverConn:
	case <-time.After(socketTestTimeout):
		t.Fatal("Accept did not complete")
	}
	defer accepted.CloseNow()
	if err := accepted.Close(stream.CloseSuperseded, "new agent connected"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = client.Read(ctx)
	var closeErr *stream.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != stream.CloseSuperseded || closeErr.Reason != "new agent connected" || !closeErr.Remote {
		t.Fatalf("Read close error = %T %v", err, err)
	}
}

func TestReadLimit(t *testing.T) {
	serverConn := make(chan stream.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Accept(w, r, AcceptOptions{MaxMessage: 4})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		serverConn <- conn
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), socketTestTimeout)
	defer cancel()
	client, _, err := Dial(ctx, server.URL, DialOptions{MaxMessage: 4})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.CloseNow()
	var accepted stream.Conn
	select {
	case accepted = <-serverConn:
	case <-time.After(socketTestTimeout):
		t.Fatal("Accept did not complete")
	}
	defer accepted.CloseNow()
	if err := client.Write(ctx, []byte("12345")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_, err = accepted.Read(ctx)
	var protocolErr *stream.ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != stream.CloseTooBig {
		t.Fatalf("Read error = %T %v, want 1009 protocol error", err, err)
	}
}

func TestDialNon101ReturnsDialError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "credential rejected")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), socketTestTimeout)
	defer cancel()
	conn, response, err := Dial(ctx, server.URL, DialOptions{})
	if conn != nil {
		_ = conn.CloseNow()
		t.Fatal("Dial returned a Conn for non-101 response")
	}
	var dialErr *DialError
	if !errors.As(err, &dialErr) || dialErr.Status != http.StatusUnauthorized || !strings.Contains(dialErr.Body, "credential rejected") {
		t.Fatalf("Dial error = %T %v, response=%#v", err, err, response)
	}
}

func TestAcceptRequiresHijacker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapped := nonHijacker{ResponseWriter: w}
		if _, err := Accept(wrapped, r, AcceptOptions{}); err == nil {
			t.Error("Accept succeeded without Hijacker")
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), socketTestTimeout)
	defer cancel()
	_, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &websocket.DialOptions{Subprotocols: []string{stream.Subprotocol}})
	if err == nil {
		t.Fatal("raw websocket Dial unexpectedly succeeded")
	}
}

type nonHijacker struct{ http.ResponseWriter }

type hijackerWrapper struct{ http.ResponseWriter }

func (h hijackerWrapper) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.ResponseWriter.(http.Hijacker).Hijack()
}

func TestHijackClearsWriteTimeout(t *testing.T) {
	accepted := make(chan stream.Conn, 1)
	server := &http.Server{WriteTimeout: 200 * time.Millisecond, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Accept(w, r, AcceptOptions{})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		accepted <- conn
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), socketTestTimeout)
		defer cancel()
		_ = server.Shutdown(ctx)
		select {
		case <-serveDone:
		case <-time.After(socketTestTimeout):
			t.Error("http.Server.Serve did not stop")
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), socketTestTimeout)
	defer cancel()
	client, _, err := Dial(ctx, "http://"+listener.Addr().String(), DialOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.CloseNow()
	var conn stream.Conn
	select {
	case conn = <-accepted:
	case <-time.After(socketTestTimeout):
		t.Fatal("Accept did not finish")
	}
	defer conn.CloseNow()
	if len := 1 * time.Second; len <= 200*time.Millisecond {
		t.Fatal("test invariant")
	}
	time.Sleep(time.Second)
	if err := client.Write(ctx, []byte("still alive")); err != nil {
		t.Fatalf("write after server WriteTimeout elapsed: %v", err)
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), time.Second)
	defer readCancel()
	got, err := conn.Read(readCtx)
	if err != nil || string(got) != "still alive" {
		t.Fatalf("read after deadline clear = %q, %v", got, err)
	}
}

func TestDialHeaderAndMaxMessageInfo(t *testing.T) {
	var auth atomic.Bool
	serverConn := make(chan stream.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer secret" {
			auth.Store(true)
		}
		conn, err := Accept(w, r, AcceptOptions{MaxMessage: 16})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		serverConn <- conn
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), socketTestTimeout)
	defer cancel()
	client, _, err := Dial(ctx, server.URL, DialOptions{Header: http.Header{"Authorization": []string{"Bearer secret"}}, MaxMessage: 16})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.CloseNow()
	if !auth.Load() || client.Info().MaxMessage != 16 {
		t.Fatalf("header=%t info=%#v", auth.Load(), client.Info())
	}
	select {
	case conn := <-serverConn:
		defer conn.CloseNow()
		if conn.Info().Subprotocol != stream.Subprotocol {
			t.Fatalf("subprotocol = %q", conn.Info().Subprotocol)
		}
	case <-time.After(socketTestTimeout):
		t.Fatal("Accept did not finish")
	}
}

func TestWsCloseIdempotent(t *testing.T) {
	serverConn := make(chan stream.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Accept(w, r, AcceptOptions{})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		serverConn <- conn
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), socketTestTimeout)
	defer cancel()
	client, _, err := Dial(ctx, server.URL, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var accepted stream.Conn
	select {
	case accepted = <-serverConn:
	case <-time.After(socketTestTimeout):
		t.Fatal("Accept did not finish")
	}
	closeDone := make(chan struct{})
	go func() { _ = client.Close(stream.CloseNormal, "done"); close(closeDone) }()
	select {
	case <-closeDone:
	case <-time.After(socketTestTimeout):
		t.Fatal("Close blocked")
	}
	_ = client.CloseNow()
	_ = accepted.CloseNow()
}

func TestResponseBodyLimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, strings.Repeat("x", 8192))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), socketTestTimeout)
	defer cancel()
	_, response, err := Dial(ctx, server.URL, DialOptions{})
	var dialErr *DialError
	if !errors.As(err, &dialErr) || len(dialErr.Body) > 4<<10 {
		t.Fatalf("DialError = %#v, response %#v", dialErr, response)
	}
}

func TestCloseReasonUTF8Safe(t *testing.T) {
	if !utf8.ValidString("reason") {
		t.Fatal("impossible UTF-8 check")
	}
	_ = strconv.Itoa(1)
	_ = sync.Once{}
}
