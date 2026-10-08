// Package stream implements a transport-independent, bidirectional framed
// stream with request/response multiplexing.
package stream

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

const (
	ProtocolVersion = 1
	Subprotocol     = "homer.stream.v1"
)

// Conn is a transport-independent, ordered, message-framed connection.
// Read returns one complete text message. Write is safe for concurrent use and
// honors the context deadline/cancellation. Close performs a bounded close
// handshake; CloseNow drops the transport immediately.
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, msg []byte) error
	Close(code CloseCode, reason string) error
	CloseNow() error
	Info() ConnInfo
}

type ConnInfo struct {
	Transport   string
	Remote      string
	Subprotocol string
	MaxMessage  int64
}

type CloseCode int

const (
	CloseNormal      CloseCode = 1000
	CloseGoingAway   CloseCode = 1001
	CloseProtocol    CloseCode = 1002
	CloseUnsupported CloseCode = 1003
	ClosePolicy      CloseCode = 1008
	CloseTooBig      CloseCode = 1009
	CloseInternal    CloseCode = 1011
	CloseHeartbeat   CloseCode = 4000
	CloseSuperseded  CloseCode = 4001
	CloseRevoked     CloseCode = 4401
	CloseRemoved     CloseCode = 4403
)

type CloseError struct {
	Code   CloseCode
	Reason string
	Remote bool
}

func (e *CloseError) Error() string {
	if e == nil {
		return "stream: connection closed"
	}
	if e.Reason == "" {
		return "stream: connection closed (code " + strconv.Itoa(int(e.Code)) + ")"
	}
	return "stream: connection closed (code " + strconv.Itoa(int(e.Code)) + "): " + e.Reason
}

type ProtocolError struct {
	Code CloseCode
	Msg  string
}

func (e *ProtocolError) Error() string {
	if e == nil {
		return "stream: protocol error"
	}
	return "stream: protocol error: " + e.Msg
}

type FrameType string

const (
	TReq    FrameType = "req"
	TRes    FrameType = "res"
	TProg   FrameType = "prog"
	TCancel FrameType = "cancel"
	TEvt    FrameType = "evt"
	TPing   FrameType = "ping"
	TPong   FrameType = "pong"
)

type Frame struct {
	T   FrameType       `json:"t"`
	ID  string          `json:"id,omitempty"`
	M   string          `json:"m,omitempty"`
	P   json.RawMessage `json:"p,omitempty"`
	OK  bool            `json:"ok,omitempty"`
	E   *Error          `json:"e,omitempty"`
	DL  int64           `json:"dl,omitempty"`
	Seq uint64          `json:"seq,omitempty"`
}

type Error struct {
	Code      string `json:"code"`
	Message   string `json:"msg,omitempty"`
	Retryable bool   `json:"retry,omitempty"` // reserved: no reader yet
}

func (e *Error) Error() string {
	if e == nil {
		return "stream: remote error"
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

const (
	CodeUnknownMethod = "unknown-method"
	CodeBadRequest    = "bad-request"
	CodeDuplicateID   = "duplicate-id"
	CodeOverloaded    = "overloaded"
	CodeCanceled      = "canceled"
	CodeTimeout       = "timeout"
	CodeInternal      = "internal"
	CodeFrameTooLarge = "frame-too-large"
	CodeExecFailed    = "exec-failed"
	CodeUnauthorized  = "unauthorized"
	CodeUnsupported   = "unsupported-version"
)

var ErrFrameTooLarge = errors.New("stream: frame exceeds max size")

// SessionClosedError is returned when a Session ends, including by every
// outstanding Call at the instant the session is closed.
type SessionClosedError struct{ Cause error }

func (e *SessionClosedError) Error() string {
	if e == nil || e.Cause == nil {
		return "stream: session closed"
	}
	return "stream: session closed: " + e.Cause.Error()
}

func (e *SessionClosedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type Logger interface {
	Printf(format string, args ...any)
}

type Options struct {
	Logger        Logger
	MaxFrame      int
	PingInterval  time.Duration
	PingTimeout   time.Duration
	WriteTimeout  time.Duration
	SendQueue     int
	MaxInflightIn int
	Clock         Clock
}

type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type Handler func(ctx context.Context, req *Request) (result any, err error)
type EventHandler func(ctx context.Context, method string, params json.RawMessage)

type Request struct {
	ID       string
	Method   string
	Params   json.RawMessage
	Budget   time.Duration
	Session  *Session
	progress func(any)
}

func (r *Request) Decode(v any) error {
	if r == nil {
		return errors.New("stream: cannot decode a nil request")
	}
	if len(r.Params) == 0 {
		return nil
	}
	return json.Unmarshal(r.Params, v)
}

// Progress is non-blocking. Progress frames are droppable and never fail a
// handler.
func (r *Request) Progress(v any) {
	if r == nil || r.progress == nil {
		return
	}
	r.progress(v)
}

type CallOption func(*callOpts)

type callOpts struct {
	budget   time.Duration
	progress func(json.RawMessage)
}

// WithBudget puts a relative call budget, in milliseconds, on the request
// frame. A positive value is rounded up to at least one millisecond.
func WithBudget(d time.Duration) CallOption {
	return func(o *callOpts) {
		if d > 0 {
			o.budget = d
		}
	}
}

// WithProgress registers a callback invoked by the goroutine executing Call.
// Progress notifications preserve their order and may drop oldest entries if
// the callback cannot keep up.
func WithProgress(fn func(json.RawMessage)) CallOption {
	return func(o *callOpts) { o.progress = fn }
}

type Stats struct {
	FramesIn        uint64
	FramesOut       uint64
	ProgressDropped uint64
	Pending         int
	InflightIn      int
	PingRTT         time.Duration
}
