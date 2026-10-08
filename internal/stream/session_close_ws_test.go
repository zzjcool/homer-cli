package stream_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/wsconn"
)

func TestSessionLocalCloseSendsWebSocketCloseCode(t *testing.T) {
	serverSessions := make(chan *stream.Session, 1)
	runResults := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsconn.Accept(w, r, wsconn.AcceptOptions{})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		session := stream.NewSession(conn, stream.Options{PingInterval: time.Hour})
		serverSessions <- session
		runResults <- session.Run(r.Context())
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := wsconn.Dial(ctx, server.URL, wsconn.DialOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.CloseNow()

	var session *stream.Session
	select {
	case session = <-serverSessions:
	case <-ctx.Done():
		t.Fatal("server session was not created")
	}

	// Complete one session-level ping round-trip so Close cannot race ahead of
	// Run starting its read loop (which is the context canceled by the bug).
	ping, err := stream.EncodeFrame(&stream.Frame{T: stream.TPing, ID: "read-loop-ready"}, stream.DefaultMaxFrame)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if err := client.Write(ctx, ping); err != nil {
		t.Fatalf("Write readiness ping: %v", err)
	}
	pongMessage, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("Read readiness pong: %v", err)
	}
	pong, err := stream.DecodeFrame(pongMessage, stream.DefaultMaxFrame)
	if err != nil || pong.T != stream.TPong || pong.ID != "read-loop-ready" {
		t.Fatalf("readiness pong = %#v, %v", pong, err)
	}

	session.Close(stream.ClosePolicy, "slow-consumer")
	select {
	case <-session.Done():
	case <-ctx.Done():
		t.Fatal("session did not close")
	}

	_, err = client.Read(ctx)
	var closeErr *stream.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("client Read error = %T %v, want *stream.CloseError", err, err)
	}
	if closeErr.Code != stream.ClosePolicy || closeErr.Reason != "slow-consumer" || !closeErr.Remote {
		t.Fatalf("client close error = %#v, want remote code %d and reason slow-consumer", closeErr, stream.ClosePolicy)
	}

	select {
	case err := <-runResults:
		if err != nil {
			t.Fatalf("server session Run: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("server session Run did not return")
	}
}
