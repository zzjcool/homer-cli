package spike

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// TCPProxyConfig configures the temporary, source-side TCP proxy used for the
// half-open test. The HTTP control listener should remain bound to loopback.
type TCPProxyConfig struct {
	Addr        string
	Target      string
	ControlAddr string
}

type tcpProxy struct {
	cfg       TCPProxyConfig
	blackhole atomic.Bool
	active    atomic.Int64
	bytesIn   atomic.Int64
	bytesOut  atomic.Int64
	dropped   atomic.Int64
	conns     sync.Map // net.Conn -> struct{}
}

// ServeTCPProxy transparently forwards TCP, with local control endpoints that
// can silently drop data without closing either side. This is only used by the
// isolated spike, never by the application.
func ServeTCPProxy(ctx context.Context, cfg TCPProxyConfig) error {
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:17801"
	}
	if cfg.Target == "" {
		return fmt.Errorf("TCP proxy target is required")
	}
	if cfg.ControlAddr == "" {
		cfg.ControlAddr = "127.0.0.1:17803"
	}
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on proxy address %s: %w", cfg.Addr, err)
	}
	controlListener, err := net.Listen("tcp", cfg.ControlAddr)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("listen on proxy control address %s: %w", cfg.ControlAddr, err)
	}

	proxy := &tcpProxy{cfg: cfg}
	controlMux := http.NewServeMux()
	controlMux.HandleFunc("/healthz", proxy.handleStats)
	controlMux.HandleFunc("/stats", proxy.handleStats)
	controlMux.HandleFunc("/blackhole", proxy.handleBlackhole)
	controlMux.HandleFunc("/resume", proxy.handleResume)
	controlServer := &http.Server{
		Handler:           controlMux,
		ReadHeaderTimeout: 3 * time.Second,
		WriteTimeout:      3 * time.Second,
	}
	controlErr := make(chan error, 1)
	go func() {
		if err := controlServer.Serve(controlListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			controlErr <- err
			return
		}
		controlErr <- nil
	}()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- proxy.serve(ctx, listener)
	}()
	log.Printf("wsecho TCP proxy listening addr=%s target=%s control=%s", cfg.Addr, cfg.Target, cfg.ControlAddr)

	select {
	case <-ctx.Done():
		_ = listener.Close()
		proxy.closeConnections()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = controlServer.Shutdown(shutdownCtx)
		return nil
	case err := <-serveErr:
		_ = controlServer.Close()
		proxy.closeConnections()
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	case err := <-controlErr:
		_ = listener.Close()
		proxy.closeConnections()
		_ = controlServer.Close()
		return err
	}
}

func (p *tcpProxy) serve(ctx context.Context, listener net.Listener) error {
	for {
		client, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept proxy connection: %w", err)
		}
		go p.handleConnection(ctx, client)
	}
}

func (p *tcpProxy) handleConnection(ctx context.Context, client net.Conn) {
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	backend, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(dialCtx, "tcp", p.cfg.Target)
	if err != nil {
		_ = client.Close()
		log.Printf("TCP proxy dial target=%s failed: %v", p.cfg.Target, err)
		return
	}
	p.active.Add(1)
	p.conns.Store(client, struct{}{})
	p.conns.Store(backend, struct{}{})
	var once sync.Once
	closed := make(chan struct{})
	closePair := func() {
		once.Do(func() {
			close(closed)
			_ = client.Close()
			_ = backend.Close()
		})
	}
	go p.forward(backend, client, closePair)
	go p.forward(client, backend, closePair)
	<-closed
	p.conns.Delete(client)
	p.conns.Delete(backend)
	p.active.Add(-1)
}

func (p *tcpProxy) forward(dst, src net.Conn, closePair func()) {
	buffer := make([]byte, 32*1024)
	for {
		_ = src.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, readErr := src.Read(buffer)
		if n > 0 {
			p.bytesIn.Add(int64(n))
			if p.blackhole.Load() {
				p.dropped.Add(int64(n))
			} else {
				_ = dst.SetWriteDeadline(time.Now().Add(5 * time.Second))
				remaining := buffer[:n]
				for len(remaining) > 0 {
					written, writeErr := dst.Write(remaining)
					if written > 0 {
						p.bytesOut.Add(int64(written))
						remaining = remaining[written:]
					}
					if writeErr != nil {
						closePair()
						return
					}
					if written == 0 {
						closePair()
						return
					}
				}
			}
		}
		if readErr == nil {
			continue
		}
		if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
			continue
		}
		closePair()
		return
	}
}

func (p *tcpProxy) closeConnections() {
	p.conns.Range(func(key, _ any) bool {
		if conn, ok := key.(net.Conn); ok {
			_ = conn.Close()
		}
		return true
	})
}

func (p *tcpProxy) handleBlackhole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	p.blackhole.Store(true)
	p.handleStats(w, r)
}

func (p *tcpProxy) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	p.blackhole.Store(false)
	p.handleStats(w, r)
}

func (p *tcpProxy) handleStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"blackhole":          p.blackhole.Load(),
		"active_connections": p.active.Load(),
		"bytes_in":           p.bytesIn.Load(),
		"bytes_out":          p.bytesOut.Load(),
		"bytes_dropped":      p.dropped.Load(),
	})
}
