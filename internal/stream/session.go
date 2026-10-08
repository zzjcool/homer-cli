package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultMaxFrame      = 8 << 20
	DefaultPingInterval  = 25 * time.Second
	DefaultPingTimeout   = 75 * time.Second
	DefaultWriteTimeout  = 15 * time.Second
	DefaultSendQueue     = 256
	DefaultMaxInflightIn = 128
	progressQueueSize    = 64
)

type outbound struct {
	frame *Frame
	order uint64
}

type pendingCall struct {
	result   chan callResult
	progress chan json.RawMessage
	callback func(json.RawMessage)
}

type callResult struct {
	payload json.RawMessage
	err     error
}

type inboundCall struct {
	cancel context.CancelFunc
	state  atomic.Uint32 // 0 running, 1 handler completed, 2 cancellation completed
	seq    atomic.Uint64
}

type Session struct {
	conn  Conn
	opts  Options
	clock Clock

	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	runMu         sync.Mutex
	running       bool
	explicitClose atomic.Bool
	closeRequest  *CloseError

	mu       sync.Mutex
	err      error
	cause    error
	handlers map[string]Handler
	events   map[string]EventHandler
	pending  map[string]*pendingCall
	inflight map[string]*inboundCall
	nextID   atomic.Uint64

	queueMu    sync.Mutex
	urgentQ    []outbound
	normalQ    []outbound
	progressQ  []outbound
	queueCount int
	queueSeq   atomic.Uint64
	queueSpace chan struct{}
	wakeQ      chan struct{}
	flushQ     chan chan struct{}

	heartbeatChanged chan struct{}
	lastRecvMu       sync.Mutex
	lastRecv         time.Time
	pingRTT          atomic.Int64
	pingMu           sync.Mutex
	pings            map[string]time.Time

	framesIn        atomic.Uint64
	framesOut       atomic.Uint64
	progressDropped atomic.Uint64
	droppedLog      *ThrottledLogger
}

// NewSession creates a multiplexed session. Call Handle and OnEvent before
// Run; callbacks registered after Run starts panic.
func NewSession(conn Conn, opts Options) *Session {
	if opts.MaxFrame <= 0 {
		opts.MaxFrame = DefaultMaxFrame
	}
	if opts.PingInterval <= 0 {
		opts.PingInterval = DefaultPingInterval
	}
	if opts.PingTimeout <= 0 {
		opts.PingTimeout = DefaultPingTimeout
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = DefaultWriteTimeout
	}
	if opts.SendQueue <= 0 {
		opts.SendQueue = DefaultSendQueue
	}
	if opts.MaxInflightIn <= 0 {
		opts.MaxInflightIn = DefaultMaxInflightIn
	}
	clock := opts.Clock
	if clock == nil {
		clock = realClock{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{
		conn:             conn,
		opts:             opts,
		clock:            clock,
		ctx:              ctx,
		cancel:           cancel,
		done:             make(chan struct{}),
		handlers:         make(map[string]Handler),
		events:           make(map[string]EventHandler),
		pending:          make(map[string]*pendingCall),
		inflight:         make(map[string]*inboundCall),
		queueSpace:       make(chan struct{}, 1),
		wakeQ:            make(chan struct{}, 1),
		flushQ:           make(chan chan struct{}, 1),
		heartbeatChanged: make(chan struct{}, 1),
		lastRecv:         clock.Now(),
		pings:            make(map[string]time.Time),
		droppedLog:       NewThrottledLogger(opts.Logger, time.Minute),
	}
}

func (s *Session) Handle(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running || s.isDone() {
		panic("stream: Handle called after Run")
	}
	if method == "" || h == nil {
		panic("stream: Handle requires a method and handler")
	}
	s.handlers[method] = h
}

func (s *Session) OnEvent(method string, h EventHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running || s.isDone() {
		panic("stream: OnEvent called after Run")
	}
	if method == "" || h == nil {
		panic("stream: OnEvent requires a method and handler")
	}
	s.events[method] = h
}

// Run starts the read, single-writer and heartbeat loops and blocks until any
// loop exits. It then cancels all calls and handlers before returning.
func (s *Session) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.runMu.Lock()
	if s.running {
		s.runMu.Unlock()
		return errors.New("stream: Session.Run called more than once")
	}
	if s.isDone() {
		s.runMu.Unlock()
		return s.Err()
	}
	childCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.isDone() {
		alreadyClosed := s.err
		s.mu.Unlock()
		cancel()
		s.runMu.Unlock()
		return alreadyClosed
	}
	s.running = true
	s.ctx = childCtx
	s.cancel = cancel
	s.mu.Unlock()
	s.lastRecvMu.Lock()
	s.lastRecv = s.clock.Now()
	s.lastRecvMu.Unlock()
	s.runMu.Unlock()
	defer cancel()

	finished := make(chan error, 3)
	var loops sync.WaitGroup
	loops.Add(3)
	go func() { defer loops.Done(); finished <- s.readLoop() }()
	go func() { defer loops.Done(); finished <- s.writerLoop() }()
	go func() { defer loops.Done(); finished <- s.pingLoop() }()

	err := <-finished
	s.terminate(err)
	loops.Wait()
	if s.explicitClose.Load() {
		return nil
	}
	return s.Err()
}

func (s *Session) readLoop() error {
	for {
		msg, err := s.conn.Read(s.ctx)
		if err != nil {
			var protocolErr *ProtocolError
			if errors.As(err, &protocolErr) {
				_ = s.conn.Close(protocolErr.Code, protocolErr.Msg)
				return &CloseError{Code: protocolErr.Code, Reason: protocolErr.Msg}
			}
			if errors.Is(err, context.Canceled) && s.isDone() {
				return nil
			}
			return err
		}
		s.lastRecvMu.Lock()
		s.lastRecv = s.clock.Now()
		s.lastRecvMu.Unlock()
		s.framesIn.Add(1)
		frame, err := DecodeFrame(msg, s.maxFrame())
		if err != nil {
			var protocolErr *ProtocolError
			if errors.As(err, &protocolErr) {
				_ = s.conn.Close(protocolErr.Code, protocolErr.Msg)
				return &CloseError{Code: protocolErr.Code, Reason: protocolErr.Msg}
			}
			return err
		}
		if err := s.handleFrame(frame); err != nil {
			return err
		}
	}
}

func (s *Session) handleFrame(frame *Frame) error {
	switch frame.T {
	case TReq:
		s.handleRequest(frame)
	case TRes:
		s.handleResponse(frame)
	case TProg:
		s.handleProgress(frame)
	case TCancel:
		s.handleCancel(frame.ID)
	case TEvt:
		s.handleEvent(frame)
	case TPing:
		return s.enqueue(&Frame{T: TPong, ID: frame.ID}, true, false)
	case TPong:
		s.handlePong(frame.ID)
	default:
		if s.droppedLog != nil {
			s.droppedLog.Log("unknown-frame-type", "stream: dropping unknown frame type "+string(frame.T))
		}
	}
	return nil
}

func (s *Session) handleRequest(frame *Frame) {
	s.mu.Lock()
	if _, exists := s.inflight[frame.ID]; exists {
		s.mu.Unlock()
		s.sendResponse(frame.ID, nil, &Error{Code: CodeDuplicateID, Message: "request ID is already in flight"})
		return
	}
	if len(s.inflight) >= s.maxInflight() {
		s.mu.Unlock()
		s.sendResponse(frame.ID, nil, &Error{Code: CodeOverloaded, Message: "too many in-flight requests", Retryable: true})
		return
	}
	handler := s.handlers[frame.M]
	if handler == nil {
		s.mu.Unlock()
		s.sendResponse(frame.ID, nil, &Error{Code: CodeUnknownMethod, Message: "unknown method: " + frame.M})
		return
	}
	baseCtx := s.ctx
	var ctx context.Context
	var cancel context.CancelFunc
	if frame.DL > 0 {
		budget := maxDurationFromMillis(frame.DL)
		ctx, cancel = context.WithTimeout(baseCtx, budget)
	} else {
		ctx, cancel = context.WithCancel(baseCtx)
	}
	in := &inboundCall{cancel: cancel}
	s.inflight[frame.ID] = in
	s.mu.Unlock()

	req := &Request{
		ID: frame.ID, Method: frame.M, Params: append(json.RawMessage(nil), frame.P...),
		Budget: maxDurationFromMillis(frame.DL), Session: s,
		progress: func(v any) { s.enqueueProgress(frame.ID, in, v) },
	}
	go s.runHandler(ctx, cancel, in, req, handler)
}

func (s *Session) runHandler(ctx context.Context, cancel context.CancelFunc, in *inboundCall, req *Request, handler Handler) {
	var result any
	var err error
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
				result = nil
				err = &Error{Code: CodeInternal, Message: "handler panic"}
			}
		}()
		result, err = handler(ctx, req)
	}()
	if !panicked && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = &Error{Code: CodeTimeout, Message: "request budget expired"}
	}
	if !in.state.CompareAndSwap(0, 1) {
		cancel()
		s.mu.Lock()
		if s.inflight[req.ID] == in {
			delete(s.inflight, req.ID)
		}
		s.mu.Unlock()
		return
	}
	cancel()

	var remoteErr *Error
	if err != nil {
		if !errors.As(err, &remoteErr) {
			remoteErr = &Error{Code: CodeExecFailed, Message: err.Error()}
		}
	}
	var payload json.RawMessage
	if remoteErr == nil {
		encoded, encodeErr := json.Marshal(result)
		if encodeErr != nil {
			remoteErr = &Error{Code: CodeInternal, Message: "failed to encode handler result"}
		} else {
			payload = encoded
		}
	}
	response := &Frame{T: TRes, ID: req.ID, OK: remoteErr == nil, P: payload, E: remoteErr}
	if _, encodeErr := EncodeFrame(response, s.maxFrame()); encodeErr != nil {
		response = &Frame{T: TRes, ID: req.ID, E: &Error{Code: CodeFrameTooLarge, Message: "handler result exceeds max frame"}}
	}
	if err := s.enqueue(response, false, false); err != nil {
		s.Close(ClosePolicy, "slow-consumer")
	}
	s.mu.Lock()
	if s.inflight[req.ID] == in {
		delete(s.inflight, req.ID)
	}
	s.mu.Unlock()
}

func (s *Session) handleCancel(id string) {
	s.mu.Lock()
	in := s.inflight[id]
	cancelled := in != nil && in.state.CompareAndSwap(0, 2)
	if cancelled {
		delete(s.inflight, id)
	}
	s.mu.Unlock()
	if cancelled {
		in.cancel()
	}
}

func (s *Session) handleResponse(frame *Frame) {
	s.mu.Lock()
	pending := s.pending[frame.ID]
	if pending != nil {
		delete(s.pending, frame.ID)
		if frame.OK {
			pending.result <- callResult{payload: append(json.RawMessage(nil), frame.P...)}
		} else if frame.E != nil {
			pending.result <- callResult{err: frame.E}
		} else {
			pending.result <- callResult{err: &ProtocolError{Code: CloseProtocol, Msg: "failure response missing error"}}
		}
	}
	s.mu.Unlock()
}

func (s *Session) handleProgress(frame *Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending[frame.ID]
	if pending == nil || pending.callback == nil {
		return
	}
	payload := append(json.RawMessage(nil), frame.P...)
	select {
	case pending.progress <- payload:
	default:
		// Keep the newest progress under callback backpressure while retaining
		// order for all frames which fit in the per-call ring.
		select {
		case <-pending.progress:
			s.progressDropped.Add(1)
		default:
		}
		select {
		case pending.progress <- payload:
		default:
			s.progressDropped.Add(1)
		}
	}
}

func (s *Session) handleEvent(frame *Frame) {
	s.mu.Lock()
	handler := s.events[frame.M]
	ctx := s.ctx
	s.mu.Unlock()
	if handler == nil {
		return
	}
	params := append(json.RawMessage(nil), frame.P...)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil && s.droppedLog != nil {
				s.droppedLog.Log("event-handler-panic:"+frame.M, fmt.Sprintf("stream: event handler panic method=%q: %v", frame.M, recovered))
			}
		}()
		handler(ctx, frame.M, params)
	}()
}

func (s *Session) handlePong(id string) {
	s.pingMu.Lock()
	sent, ok := s.pings[id]
	if ok {
		delete(s.pings, id)
	}
	s.pingMu.Unlock()
	if ok {
		d := s.clock.Now().Sub(sent)
		if d < 0 {
			d = 0
		}
		s.pingRTT.Store(int64(d))
	}
}

func (s *Session) enqueueProgress(id string, in *inboundCall, value any) {
	if in.state.Load() != 0 || s.isDone() {
		return
	}
	payload, err := json.Marshal(value)
	if err != nil {
		s.progressDropped.Add(1)
		return
	}
	frame := &Frame{T: TProg, ID: id, Seq: in.seq.Add(1), P: payload}
	if _, err := EncodeFrame(frame, s.maxFrame()); err != nil {
		s.progressDropped.Add(1)
		return
	}
	_ = s.enqueue(frame, false, true)
}

func (s *Session) sendResponse(id string, payload json.RawMessage, err *Error) {
	frame := &Frame{T: TRes, ID: id, P: payload}
	if err == nil {
		frame.OK = true
	} else {
		frame.E = err
	}
	if encodeErr := s.frameFits(frame); encodeErr != nil {
		frame = &Frame{T: TRes, ID: id, E: &Error{Code: CodeFrameTooLarge, Message: "response exceeds max frame"}}
	}
	if queueErr := s.enqueue(frame, false, false); queueErr != nil {
		s.Close(ClosePolicy, "slow-consumer")
	}
}

func (s *Session) enqueue(frame *Frame, urgent, progress bool) error {
	return s.enqueueContext(s.ctx, frame, urgent, progress)
}

func (s *Session) enqueueContext(ctx context.Context, frame *Frame, urgent, progress bool) error {
	if ctx == nil {
		ctx = s.ctx
	}
	if _, err := EncodeFrame(frame, s.maxFrame()); err != nil {
		return err
	}
	if s.isDone() {
		return s.closedError()
	}
	if progress {
		s.queueMu.Lock()
		if s.isDone() {
			s.queueMu.Unlock()
			return s.closedError()
		}
		if len(s.progressQ) == progressQueueSize {
			copy(s.progressQ, s.progressQ[1:])
			s.progressQ[len(s.progressQ)-1] = outbound{frame: frame, order: s.queueSeq.Add(1)}
			s.progressDropped.Add(1)
		} else {
			s.progressQ = append(s.progressQ, outbound{frame: frame, order: s.queueSeq.Add(1)})
		}
		s.queueMu.Unlock()
		signal(s.wakeQ)
		return nil
	}

	timer := time.NewTimer(s.opts.WriteTimeout)
	defer timer.Stop()
	for {
		s.queueMu.Lock()
		if s.isDone() {
			s.queueMu.Unlock()
			return s.closedError()
		}
		if s.queueCount < s.opts.SendQueue {
			item := outbound{frame: frame, order: s.queueSeq.Add(1)}
			if urgent {
				s.urgentQ = append(s.urgentQ, item)
			} else {
				s.normalQ = append(s.normalQ, item)
			}
			s.queueCount++
			s.queueMu.Unlock()
			signal(s.wakeQ)
			return nil
		}
		s.queueMu.Unlock()
		select {
		case <-s.queueSpace:
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			s.Close(ClosePolicy, "slow-consumer")
			return &SessionClosedError{Cause: &CloseError{Code: ClosePolicy, Reason: "slow-consumer"}}
		case <-s.done:
			return s.closedError()
		}
	}
}

func (s *Session) writerLoop() error {
	for {
		if item, ok := s.dequeue(); ok {
			if err := s.write(item.frame); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					s.Close(ClosePolicy, "slow-consumer")
				}
				return err
			}
			continue
		}
		select {
		case <-s.ctx.Done():
			for {
				select {
				case finished := <-s.flushQ:
					close(finished)
				default:
					return nil
				}
			}
		case finished := <-s.flushQ:
			for {
				item, ok := s.dequeue()
				if !ok {
					close(finished)
					break
				}
				if err := s.write(item.frame); err != nil {
					return err
				}
			}
		case <-s.wakeQ:
		}
	}
}

func (s *Session) dequeue() (outbound, bool) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	var item outbound
	switch {
	case len(s.urgentQ) > 0:
		item = s.urgentQ[0]
		s.urgentQ[0] = outbound{}
		s.urgentQ = s.urgentQ[1:]
	case len(s.normalQ) > 0 && len(s.progressQ) > 0 && s.normalQ[0].order < s.progressQ[0].order:
		item = s.normalQ[0]
		s.normalQ[0] = outbound{}
		s.normalQ = s.normalQ[1:]
	case len(s.normalQ) > 0 && len(s.progressQ) == 0:
		item = s.normalQ[0]
		s.normalQ[0] = outbound{}
		s.normalQ = s.normalQ[1:]
	case len(s.progressQ) > 0:
		item = s.progressQ[0]
		s.progressQ[0] = outbound{}
		s.progressQ = s.progressQ[1:]
	default:
		return outbound{}, false
	}
	if item.frame.T != TProg {
		s.queueCount--
		signal(s.queueSpace)
	}
	return item, true
}

func (s *Session) write(frame *Frame) error {
	encoded, err := EncodeFrame(frame, s.maxFrame())
	if err != nil {
		if frame.T == TProg {
			s.progressDropped.Add(1)
			return nil
		}
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.opts.WriteTimeout)
	defer cancel()
	if err := s.conn.Write(ctx, encoded); err != nil {
		return err
	}
	s.framesOut.Add(1)
	return nil
}

func (s *Session) pingLoop() error {
	now := s.clock.Now()
	s.mu.Lock()
	interval, timeout := s.opts.PingInterval, s.opts.PingTimeout
	s.mu.Unlock()
	nextPing := now.Add(interval)
	for {
		now = s.clock.Now()
		s.mu.Lock()
		interval, timeout = s.opts.PingInterval, s.opts.PingTimeout
		s.mu.Unlock()
		s.lastRecvMu.Lock()
		last := s.lastRecv
		s.lastRecvMu.Unlock()
		untilTimeout := timeout - now.Sub(last)
		if untilTimeout < 0 {
			closeErr := &CloseError{Code: CloseHeartbeat, Reason: "ping timeout"}
			_ = s.conn.Close(CloseHeartbeat, "ping timeout")
			return closeErr
		}
		if untilTimeout == 0 {
			untilTimeout = time.Nanosecond
		}
		untilPing := nextPing.Sub(now)
		if untilPing < 0 {
			untilPing = 0
		}
		wait := untilPing
		if untilTimeout < wait {
			wait = untilTimeout
		}
		timer := s.clock.NewTimer(wait)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return nil
		case <-s.heartbeatChanged:
			timer.Stop()
			nextPing = s.clock.Now()
			continue
		case <-timer.C():
			timer.Stop()
		}
		if s.isDone() {
			return nil
		}
		now = s.clock.Now()
		s.lastRecvMu.Lock()
		last = s.lastRecv
		s.lastRecvMu.Unlock()
		if now.Sub(last) > timeout {
			closeErr := &CloseError{Code: CloseHeartbeat, Reason: "ping timeout"}
			_ = s.conn.Close(CloseHeartbeat, "ping timeout")
			return closeErr
		}
		if !now.Before(nextPing) {
			id := fmt.Sprintf("ping-%d", s.nextID.Add(1))
			s.pingMu.Lock()
			s.pings = map[string]time.Time{id: now}
			s.pingMu.Unlock()
			if err := s.enqueue(&Frame{T: TPing, ID: id}, true, false); err != nil {
				return err
			}
			nextPing = now.Add(interval)
		}
	}
}

func (s *Session) Call(ctx context.Context, method string, params any, opts ...CallOption) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var options callOpts
	for _, option := range opts {
		if option != nil {
			option(&options)
		}
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("call-%d", s.nextID.Add(1))
	dl := int64(0)
	if options.budget > 0 {
		dl = options.budget.Milliseconds()
		if dl < 1 {
			dl = 1
		}
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline).Milliseconds()
		if remaining < 1 {
			remaining = 1
		}
		if dl == 0 || remaining < dl {
			dl = remaining
		}
	}
	frame := &Frame{T: TReq, ID: id, M: method, P: payload, DL: dl}
	if _, err := EncodeFrame(frame, s.maxFrame()); err != nil {
		return nil, ErrFrameTooLarge
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pending := &pendingCall{result: make(chan callResult, 1), callback: options.progress}
	if options.progress != nil {
		pending.progress = make(chan json.RawMessage, progressQueueSize)
	}
	s.mu.Lock()
	if err := s.sessionErrorLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.pending[id] = pending
	s.mu.Unlock()
	if err := s.enqueueContext(ctx, frame, false, false); err != nil {
		s.removePending(id, pending)
		if s.isDone() {
			return nil, s.closedError()
		}
		return nil, err
	}

	for {
		var progress <-chan json.RawMessage
		if options.progress != nil {
			progress = pending.progress
		}
		select {
		case result := <-pending.result:
			if options.progress != nil {
				for {
					select {
					case progress := <-pending.progress:
						options.progress(progress)
					default:
						if result.err != nil {
							return nil, result.err
						}
						return result.payload, nil
					}
				}
			}
			if result.err != nil {
				return nil, result.err
			}
			return result.payload, nil
		case data := <-progress:
			if options.progress != nil {
				options.progress(data)
			}
		case <-ctx.Done():
			if s.removePending(id, pending) {
				s.enqueueCancelBestEffort(id)
				return nil, ctx.Err()
			}
			select {
			case result := <-pending.result:
				return result.payload, result.err
			default:
				return nil, ctx.Err()
			}
		case <-s.done:
			select {
			case result := <-pending.result:
				return result.payload, result.err
			default:
				return nil, s.closedError()
			}
		}
	}
}

func (s *Session) enqueueCancelBestEffort(id string) {
	frame := &Frame{T: TCancel, ID: id}
	if _, err := EncodeFrame(frame, s.maxFrame()); err != nil {
		return
	}
	s.queueMu.Lock()
	if s.queueCount >= s.opts.SendQueue || s.isDone() {
		s.queueMu.Unlock()
		return
	}
	s.urgentQ = append(s.urgentQ, outbound{frame: frame, order: s.queueSeq.Add(1)})
	s.queueCount++
	s.queueMu.Unlock()
	signal(s.wakeQ)
}

func (s *Session) Notify(ctx context.Context, method string, params any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return err
	}
	frame := &Frame{T: TEvt, M: method, P: payload}
	if _, err := EncodeFrame(frame, s.maxFrame()); err != nil {
		return ErrFrameTooLarge
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.closedError()
	default:
	}
	return s.enqueueContext(ctx, frame, false, false)
}

// SetHeartbeat adopts the peer's negotiated heartbeat values.
func (s *Session) SetHeartbeat(interval, timeout time.Duration) {
	if interval <= 0 || timeout <= 0 {
		return
	}
	s.mu.Lock()
	s.opts.PingInterval = interval
	s.opts.PingTimeout = timeout
	s.mu.Unlock()
	signal(s.heartbeatChanged)
}

// Flush waits until all frames queued before or during the drain have been
// written, or ctx/session termination occurs.
func (s *Session) Flush(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.isDone() {
		return s.closedError()
	}
	finished := make(chan struct{})
	select {
	case s.flushQ <- finished:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.closedError()
	}
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.closedError()
	}
}

// Close ends the session idempotently and fails all outstanding operations.
func (s *Session) Close(code CloseCode, reason string) {
	closeErr := &CloseError{Code: code, Reason: reason}
	s.mu.Lock()
	if s.isDone() {
		s.mu.Unlock()
		return
	}
	if s.closeRequest == nil {
		s.closeRequest = closeErr
		s.explicitClose.Store(true)
	}
	s.mu.Unlock()
	s.terminate(closeErr)
}

func (s *Session) terminate(err error) {
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return
	default:
	}
	cause := err
	localClose := s.closeRequest != nil
	closeFrame := s.closeRequest
	if localClose {
		cause = closeFrame
	}
	s.err = cause
	if localClose {
		s.err = nil
	}
	s.cause = cause
	closedErr := &SessionClosedError{Cause: cause}
	for id, call := range s.pending {
		delete(s.pending, id)
		call.result <- callResult{err: closedErr}
	}
	for id, in := range s.inflight {
		delete(s.inflight, id)
		in.state.CompareAndSwap(0, 2)
		in.cancel()
	}
	close(s.done)
	cancel := s.cancel
	s.mu.Unlock()
	if localClose {
		// Keep the read context alive until Conn.Close has had a chance to send
		// its close frame. coder/websocket cancels a Read immediately when its
		// context is canceled, which would otherwise abort the close handshake.
		_ = s.conn.Close(closeFrame.Code, closeFrame.Reason)
	} else {
		_ = s.conn.CloseNow()
	}
	if cancel != nil {
		cancel()
	}
	s.queueMu.Lock()
	s.urgentQ = nil
	s.normalQ = nil
	s.progressQ = nil
	s.queueCount = 0
	signal(s.queueSpace)
	s.queueMu.Unlock()
}

func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Session) Info() ConnInfo { return s.conn.Info() }

func (s *Session) Stats() Stats {
	s.mu.Lock()
	pending := len(s.pending)
	inflight := len(s.inflight)
	s.mu.Unlock()
	return Stats{
		FramesIn: s.framesIn.Load(), FramesOut: s.framesOut.Load(),
		ProgressDropped: s.progressDropped.Load(), Pending: pending,
		InflightIn: inflight, PingRTT: time.Duration(s.pingRTT.Load()),
	}
}

func (s *Session) sessionErrorLocked() error {
	select {
	case <-s.done:
		return &SessionClosedError{Cause: s.cause}
	default:
		return nil
	}
}

func (s *Session) closedError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &SessionClosedError{Cause: s.cause}
}

func (s *Session) removePending(id string, call *pendingCall) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[id] != call {
		return false
	}
	delete(s.pending, id)
	return true
}

func (s *Session) frameFits(frame *Frame) error {
	_, err := EncodeFrame(frame, s.maxFrame())
	return err
}

func (s *Session) maxFrame() int {
	return s.opts.MaxFrame
}

func (s *Session) maxInflight() int {
	return s.opts.MaxInflightIn
}

func maxDurationFromMillis(milliseconds int64) time.Duration {
	if milliseconds <= 0 {
		return 0
	}
	maxMilliseconds := int64((time.Duration(1<<63 - 1)) / time.Millisecond)
	if milliseconds > maxMilliseconds {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(milliseconds) * time.Millisecond
}
