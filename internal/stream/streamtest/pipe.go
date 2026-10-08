// Package streamtest provides in-memory and socket-based test transports.
package streamtest

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
)

const defaultPipeBuffer = 256

type PipeDirection struct {
	Latency        time.Duration
	DropEvery      int
	Drop           bool
	HalfOpen       bool
	Reorder        bool
	ReadLimit      int64
	BytesPerSecond int64
}

type PipeOptions struct {
	Buffer int
	AtoB   PipeDirection
	BtoA   PipeDirection
}

type pipeMessage struct{ data []byte }

type pipeEnd struct {
	in          chan pipeMessage
	peer        *pipeEnd
	inOpts      PipeDirection
	outOpts     PipeDirection
	info        stream.ConnInfo
	localMu     sync.Mutex
	localErr    error
	localDone   chan struct{}
	remoteClose chan *stream.CloseError
	writeGate   chan struct{}
	writeSeq    atomic.Uint64
	closed      sync.Once
}

// Pipe creates an in-memory pair without background goroutines.
func Pipe(opts PipeOptions) (stream.Conn, stream.Conn) {
	capacity := opts.Buffer
	if capacity <= 0 {
		capacity = defaultPipeBuffer
	}
	left := &pipeEnd{
		in: make(chan pipeMessage, capacity), localDone: make(chan struct{}),
		remoteClose: make(chan *stream.CloseError, 1), writeGate: make(chan struct{}, 1),
		inOpts: opts.BtoA, outOpts: opts.AtoB,
		info: stream.ConnInfo{Transport: "pipe", Remote: "pipe-b", MaxMessage: opts.BtoA.ReadLimit},
	}
	right := &pipeEnd{
		in: make(chan pipeMessage, capacity), localDone: make(chan struct{}),
		remoteClose: make(chan *stream.CloseError, 1), writeGate: make(chan struct{}, 1),
		inOpts: opts.AtoB, outOpts: opts.BtoA,
		info: stream.ConnInfo{Transport: "pipe", Remote: "pipe-a", MaxMessage: opts.AtoB.ReadLimit},
	}
	left.peer, right.peer = right, left
	return left, right
}

func (p *pipeEnd) Read(ctx context.Context) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := p.currentErr(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.localDone:
		return nil, p.currentErr()
	case closeErr := <-p.remoteClose:
		p.setErr(closeErr)
		return nil, closeErr
	case message := <-p.in:
		if limit := p.inOpts.ReadLimit; limit > 0 && int64(len(message.data)) > limit {
			err := &stream.ProtocolError{Code: stream.CloseTooBig, Msg: "frame exceeds read limit"}
			_ = p.Close(stream.CloseTooBig, err.Msg)
			return nil, err
		}
		if rate := p.inOpts.BytesPerSecond; rate > 0 {
			delay := time.Duration(float64(len(message.data)) / float64(rate) * float64(time.Second))
			if delay > 0 {
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-p.localDone:
					return nil, p.currentErr()
				case <-timer.C:
				}
			}
		}
		if err := p.currentErr(); err != nil {
			return nil, err
		}
		return append([]byte(nil), message.data...), nil
	}
}

func (p *pipeEnd) Write(ctx context.Context, message []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !p.outOpts.Reorder {
		select {
		case p.writeGate <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		case <-p.localDone:
			return p.currentErr()
		case <-p.peer.localDone:
			return io.ErrClosedPipe
		}
		defer func() { <-p.writeGate }()
	}
	if err := p.currentErr(); err != nil {
		return err
	}
	if err := p.peer.currentErr(); err != nil {
		return io.ErrClosedPipe
	}
	seq := p.writeSeq.Add(1)
	if p.outOpts.Drop || p.outOpts.HalfOpen || (p.outOpts.DropEvery > 0 && int(seq)%p.outOpts.DropEvery == 0) {
		return nil
	}
	latency := p.outOpts.Latency
	if p.outOpts.Reorder {
		if seq%2 == 1 {
			if latency <= 0 {
				latency = time.Millisecond
			} else {
				latency *= 2
			}
		} else {
			latency = 0
		}
	}
	if latency > 0 {
		timer := time.NewTimer(latency)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.localDone:
			return p.currentErr()
		case <-p.peer.localDone:
			return io.ErrClosedPipe
		case <-timer.C:
		}
	}
	copy := append([]byte(nil), message...)
	select {
	case p.peer.in <- pipeMessage{data: copy}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.localDone:
		return p.currentErr()
	case <-p.peer.localDone:
		return io.ErrClosedPipe
	}
}

func (p *pipeEnd) Close(code stream.CloseCode, reason string) error {
	p.closed.Do(func() {
		p.setErr(&stream.CloseError{Code: code, Reason: reason})
		remote := &stream.CloseError{Code: code, Reason: reason, Remote: true}
		p.peer.setErr(remote)
		select {
		case p.peer.remoteClose <- remote:
		default:
		}
	})
	return nil
}

func (p *pipeEnd) CloseNow() error {
	p.closed.Do(func() {
		p.setErr(io.ErrClosedPipe)
		p.peer.setErr(io.ErrClosedPipe)
	})
	return nil
}

func (p *pipeEnd) setErr(err error) {
	p.localMu.Lock()
	defer p.localMu.Unlock()
	if p.localErr != nil {
		return
	}
	p.localErr = err
	close(p.localDone)
}

func (p *pipeEnd) currentErr() error {
	p.localMu.Lock()
	defer p.localMu.Unlock()
	return p.localErr
}

func (p *pipeEnd) Info() stream.ConnInfo { return p.info }

// FaultConn provides one-shot failures and bounded delays around a Conn.
type FaultConn struct {
	Conn stream.Conn

	ReadDelay  time.Duration
	WriteDelay time.Duration
	ReadError  error
	WriteError error
	DropWrites bool
	ReadLimit  int64

	readCount  atomic.Uint64
	writeCount atomic.Uint64
	readOnce   sync.Once
	writeOnce  sync.Once
}

func (c *FaultConn) Read(ctx context.Context) ([]byte, error) {
	if err := waitContext(ctx, c.ReadDelay); err != nil {
		return nil, err
	}
	if c.ReadError != nil {
		var err error
		c.readOnce.Do(func() { err = c.ReadError })
		if err != nil {
			return nil, err
		}
	}
	data, err := c.Conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	c.readCount.Add(1)
	if c.ReadLimit > 0 && int64(len(data)) > c.ReadLimit {
		return nil, &stream.ProtocolError{Code: stream.CloseTooBig, Msg: "fault connection read limit exceeded"}
	}
	return data, nil
}

func (c *FaultConn) Write(ctx context.Context, message []byte) error {
	if err := waitContext(ctx, c.WriteDelay); err != nil {
		return err
	}
	if c.WriteError != nil {
		var err error
		c.writeOnce.Do(func() { err = c.WriteError })
		if err != nil {
			return err
		}
	}
	c.writeCount.Add(1)
	if c.DropWrites {
		return nil
	}
	return c.Conn.Write(ctx, message)
}

func waitContext(ctx context.Context, delay time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *FaultConn) Close(code stream.CloseCode, reason string) error {
	return c.Conn.Close(code, reason)
}
func (c *FaultConn) CloseNow() error                { return c.Conn.CloseNow() }
func (c *FaultConn) Info() stream.ConnInfo          { return c.Conn.Info() }
func (c *FaultConn) Counts() (reads, writes uint64) { return c.readCount.Load(), c.writeCount.Load() }
func (c *FaultConn) String() string {
	if c == nil || c.Conn == nil {
		return "streamtest.FaultConn(nil)"
	}
	return fmt.Sprintf("streamtest.FaultConn(%s)", c.Conn.Info().Transport)
}

var _ stream.Conn = (*pipeEnd)(nil)
var _ stream.Conn = (*FaultConn)(nil)
