// Package pairtest contains in-process transports used by the pairing tests.
// It is intentionally outside the production pair package's runtime path.
package pairtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/zzjcool/homer-cli/internal/pair"
)

const pipeAddress = "pipe-test"

var (
	errPipeAlreadyServed = errors.New("pipe transport already served")
	errPipeAlreadyUsed   = errors.New("pipe transport already connected")
	errPipeClosed        = errors.New("pipe transport is closed")
	errPipeAddress       = errors.New("unknown pipe-test address")
	errPipeAcceptUsed    = errors.New("pipe server Accept is one-shot")
)

type pipeTransport struct {
	mu sync.Mutex

	peer *pipeTransport

	serveReady chan struct{}
	connReady  chan net.Conn
	closed     chan struct{}
	closeOnce  sync.Once

	served bool
	used   bool

	connMu sync.Mutex
	conns  map[net.Conn]struct{}
}

// NewPipePair returns two PairTransport values wired to each other. Calling
// Serve on either endpoint exposes the fixed pipe-test address; Connect on the
// opposite endpoint rendezvouses with that server's Accept through an
// unbuffered channel and the two callers receive opposite net.Pipe ends.
func NewPipePair() (left, right pair.PairTransport) {
	leftTransport := newPipeTransport()
	rightTransport := newPipeTransport()
	leftTransport.peer = rightTransport
	rightTransport.peer = leftTransport
	return leftTransport, rightTransport
}

func newPipeTransport() *pipeTransport {
	return &pipeTransport{
		serveReady: make(chan struct{}),
		connReady:  make(chan net.Conn),
		closed:     make(chan struct{}),
		conns:      make(map[net.Conn]struct{}),
	}
}

func (t *pipeTransport) Serve(ctx context.Context) (pair.PairServer, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	t.mu.Lock()
	if t.served {
		t.mu.Unlock()
		return nil, errPipeAlreadyServed
	}
	t.served = true
	close(t.serveReady)
	t.mu.Unlock()
	return &pipeServer{transport: t}, nil
}

func (t *pipeTransport) Connect(ctx context.Context, addr string) (io.ReadWriteCloser, error) {
	if addr != pipeAddress {
		return nil, errPipeAddress
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	peer := t.peer
	if peer == nil {
		return nil, errPipeClosed
	}

	// Connect before Serve is supported, but it waits only until the peer's
	// server has made its one-shot address available.
	select {
	case <-peer.serveReady:
	case <-peer.closed:
		return nil, errPipeClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	peer.mu.Lock()
	if peer.used {
		peer.mu.Unlock()
		return nil, errPipeAlreadyUsed
	}
	peer.used = true
	peer.mu.Unlock()

	client, server := net.Pipe()
	select {
	case peer.connReady <- server:
		t.register(client)
		return client, nil
	case <-peer.closed:
		_ = client.Close()
		_ = server.Close()
		return nil, errPipeClosed
	case <-ctx.Done():
		_ = client.Close()
		_ = server.Close()
		return nil, ctx.Err()
	}
}

func (t *pipeTransport) register(conn net.Conn) {
	t.connMu.Lock()
	t.conns[conn] = struct{}{}
	t.connMu.Unlock()
}

func (t *pipeTransport) unregister(conn net.Conn) {
	t.connMu.Lock()
	delete(t.conns, conn)
	t.connMu.Unlock()
}

func (t *pipeTransport) close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		t.connMu.Lock()
		for conn := range t.conns {
			_ = conn.Close()
		}
		t.conns = make(map[net.Conn]struct{})
		t.connMu.Unlock()
	})
	return nil
}

type pipeServer struct {
	transport *pipeTransport
	mu        sync.Mutex
	accepted  bool
}

func (s *pipeServer) Addr() string { return pipeAddress }

func (s *pipeServer) Accept(ctx context.Context) (io.ReadWriteCloser, error) {
	s.mu.Lock()
	if s.accepted {
		s.mu.Unlock()
		return nil, errPipeAcceptUsed
	}
	s.accepted = true
	s.mu.Unlock()

	select {
	case conn := <-s.transport.connReady:
		s.transport.register(conn)
		return &trackedConn{Conn: conn, owner: s.transport}, nil
	case <-s.transport.closed:
		return nil, errPipeClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *pipeServer) Close() error { return s.transport.close() }

type trackedConn struct {
	net.Conn
	owner *pipeTransport
	once  sync.Once
}

func (c *trackedConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		c.owner.unregister(c.Conn)
	})
	return err
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("nil context")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
