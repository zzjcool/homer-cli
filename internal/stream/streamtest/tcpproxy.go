package streamtest

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	proxyForwarding int32 = iota
	proxyBlackhole
	proxyCut
	proxyClosed
)

// TCPProxy is a user-space TCP forwarder with togglable network faults. Addr
// is the local listener address; target is supplied to NewTCPProxy.
type TCPProxy struct {
	listener net.Listener
	target   string

	mode     atomic.Int32
	delay    atomic.Int64
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	once     sync.Once
	stopOnce sync.Once
	done     chan struct{}
}

// NewTCPProxy starts a proxy listening on an ephemeral localhost port.
func NewTCPProxy(target string) (*TCPProxy, error) {
	if target == "" {
		return nil, errors.New("streamtest: TCPProxy target is required")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	proxy := &TCPProxy{listener: listener, target: target, conns: make(map[net.Conn]struct{}), done: make(chan struct{})}
	proxy.wg.Add(1)
	go proxy.acceptLoop()
	return proxy, nil
}

// StartTCPProxy is an alias for NewTCPProxy.
func StartTCPProxy(target string) (*TCPProxy, error) { return NewTCPProxy(target) }

func (p *TCPProxy) Addr() string {
	if p == nil || p.listener == nil {
		return ""
	}
	return p.listener.Addr().String()
}

func (p *TCPProxy) URL() string { return "tcp://" + p.Addr() }

func (p *TCPProxy) acceptLoop() {
	defer p.wg.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		if p.mode.Load() == proxyCut || p.mode.Load() == proxyClosed {
			rstClose(client)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		upstream, dialErr := (&net.Dialer{}).DialContext(ctx, "tcp", p.target)
		cancel()
		if dialErr != nil {
			rstClose(client)
			continue
		}
		p.mu.Lock()
		if p.mode.Load() == proxyCut || p.mode.Load() == proxyClosed {
			p.mu.Unlock()
			rstClose(client)
			rstClose(upstream)
			continue
		}
		p.conns[client] = struct{}{}
		p.conns[upstream] = struct{}{}
		p.wg.Add(2)
		p.mu.Unlock()
		go p.forward(client, upstream)
		go p.forward(upstream, client)
	}
}

func (p *TCPProxy) forward(source, destination net.Conn) {
	defer p.wg.Done()
	defer p.removePair(source, destination)
	buffer := make([]byte, 32*1024)
	for {
		n, err := source.Read(buffer)
		if n > 0 {
			mode := p.mode.Load()
			if mode == proxyCut || mode == proxyClosed {
				return
			}
			if mode != proxyBlackhole {
				if delay := time.Duration(p.delay.Load()); delay > 0 {
					timer := time.NewTimer(delay)
					select {
					case <-timer.C:
					case <-p.done:
						if !timer.Stop() {
							select {
							case <-timer.C:
							default:
							}
						}
						return
					}
				}
				if writeAll(destination, buffer[:n]) != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func writeAll(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func (p *TCPProxy) removePair(a, b net.Conn) {
	p.mu.Lock()
	delete(p.conns, a)
	delete(p.conns, b)
	p.mu.Unlock()
	_ = a.Close()
	_ = b.Close()
}

// Cut immediately closes all established sockets with RST and refuses new
// downstream connections until CloseGraceful.
func (p *TCPProxy) Cut() {
	if p == nil {
		return
	}
	p.mode.Store(proxyCut)
	p.stopOnce.Do(func() { close(p.done) })
	p.closeConnections(true)
}

// Blackhole drops bytes in both directions while keeping established sockets
// open, simulating a half-open network path.
func (p *TCPProxy) Blackhole() {
	if p == nil || p.mode.Load() == proxyCut || p.mode.Load() == proxyClosed {
		return
	}
	p.mode.Store(proxyBlackhole)
}

// Delay adds d to each forwarded read chunk. A non-positive duration clears
// the delay.
func (p *TCPProxy) Delay(d time.Duration) {
	if p == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	p.delay.Store(int64(d))
}

// CloseGraceful stops accepting and gracefully closes established sockets.
func (p *TCPProxy) CloseGraceful() error {
	if p == nil {
		return nil
	}
	var closeErr error
	p.once.Do(func() {
		p.mode.Store(proxyClosed)
		p.stopOnce.Do(func() { close(p.done) })
		closeErr = p.listener.Close()
		p.closeConnections(false)
	})
	p.wg.Wait()
	if errors.Is(closeErr, net.ErrClosed) {
		return nil
	}
	return closeErr
}

func (p *TCPProxy) closeConnections(reset bool) {
	p.mu.Lock()
	connections := make([]net.Conn, 0, len(p.conns))
	for conn := range p.conns {
		connections = append(connections, conn)
	}
	p.mu.Unlock()
	for _, conn := range connections {
		if reset {
			rstClose(conn)
		} else {
			_ = conn.Close()
		}
	}
}

func rstClose(conn net.Conn) {
	if conn == nil {
		return
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

var _ io.Reader = (*net.TCPConn)(nil)
