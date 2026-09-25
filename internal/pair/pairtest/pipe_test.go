package pairtest

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestPipePairRendezvousAndOneShotAccept(t *testing.T) {
	left, right := NewPipePair()
	server, err := left.Serve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if server.Addr() != "pipe-test" {
		t.Fatalf("address = %q, want pipe-test", server.Addr())
	}

	connectCh := make(chan struct {
		conn io.ReadWriteCloser
		err  error
	}, 1)
	go func() {
		conn, err := right.Connect(context.Background(), server.Addr())
		connectCh <- struct {
			conn io.ReadWriteCloser
			err  error
		}{conn: conn, err: err}
	}()

	acceptCh := make(chan struct {
		conn io.ReadWriteCloser
		err  error
	}, 1)
	go func() {
		conn, err := server.Accept(context.Background())
		acceptCh <- struct {
			conn io.ReadWriteCloser
			err  error
		}{conn: conn, err: err}
	}()

	var client io.ReadWriteCloser
	select {
	case result := <-connectCh:
		if result.err != nil {
			t.Fatal(result.err)
		}
		client = result.conn
	case <-time.After(time.Second):
		t.Fatal("Connect did not rendezvous")
	}
	defer client.Close()

	var accepted io.ReadWriteCloser
	select {
	case result := <-acceptCh:
		if result.err != nil {
			t.Fatal(result.err)
		}
		accepted = result.conn
	case <-time.After(time.Second):
		t.Fatal("Accept did not rendezvous")
	}
	defer accepted.Close()

	writeCh := make(chan error, 1)
	go func() {
		_, err := client.Write([]byte("hello"))
		writeCh <- err
	}()
	buffer := make([]byte, len("hello"))
	if _, err := io.ReadFull(accepted, buffer); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "hello" {
		t.Fatalf("accepted bytes = %q", buffer)
	}
	if err := <-writeCh; err != nil {
		t.Fatal(err)
	}

	secondCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := server.Accept(secondCtx); err == nil {
		t.Fatal("second Accept unexpectedly succeeded")
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("second Accept waited instead of enforcing one-shot semantics")
	}
}

func TestPipePairConnectMayWaitForServe(t *testing.T) {
	left, right := NewPipePair()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	connectCh := make(chan error, 1)
	go func() {
		conn, err := right.Connect(ctx, "pipe-test")
		if conn != nil {
			_ = conn.Close()
		}
		connectCh <- err
	}()
	select {
	case <-time.After(10 * time.Millisecond):
		// The server has not been made available yet; Connect should still be
		// waiting rather than returning a spurious address error.
	case err := <-connectCh:
		t.Fatalf("Connect completed before Serve: %v", err)
	}

	server, err := left.Serve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	acceptCh := make(chan error, 1)
	go func() {
		conn, err := server.Accept(ctx)
		if conn != nil {
			_ = conn.Close()
		}
		acceptCh <- err
	}()
	select {
	case err := <-connectCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Connect did not complete after Serve")
	}
	select {
	case err := <-acceptCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Accept did not complete after Serve")
	}
}
