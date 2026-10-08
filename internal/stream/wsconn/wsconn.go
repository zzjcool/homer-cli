// Package wsconn adapts coder/websocket to stream.Conn.
package wsconn

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/zzjcool/homer-cli/internal/stream"
)

const (
	defaultMaxMessage = 8<<20 + 4096
	closeWait         = 250 * time.Millisecond
)

type DialOptions struct {
	Header     http.Header
	HTTPClient *http.Client
	MaxMessage int64
}
type AcceptOptions struct{ MaxMessage int64 }

type DialError struct {
	Status int
	Body   string
}

func (e *DialError) Error() string {
	if e == nil {
		return "wsconn: WebSocket dial failed"
	}
	if e.Body == "" {
		return fmt.Sprintf("wsconn: WebSocket dial returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("wsconn: WebSocket dial returned HTTP %d: %s", e.Status, e.Body)
}

var defaultHTTPClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 15 * time.Second,
}}

func Dial(ctx context.Context, rawURL string, o DialOptions) (stream.Conn, *http.Response, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("wsconn: parse URL: %w", err)
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return nil, nil, fmt.Errorf("wsconn: unsupported URL scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, nil, errors.New("wsconn: URL must include a host")
	}
	maxMessage := o.MaxMessage
	if maxMessage <= 0 {
		maxMessage = defaultMaxMessage
	}
	client := o.HTTPClient
	if client == nil {
		client = defaultHTTPClient
	}
	conn, resp, dialErr := websocket.Dial(ctx, parsed.String(), &websocket.DialOptions{
		HTTPClient: client, HTTPHeader: o.Header,
		Subprotocols: []string{stream.Subprotocol}, CompressionMode: websocket.CompressionDisabled,
	})
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		body := ""
		if resp.Body != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			body = string(b)
			_ = resp.Body.Close()
			resp.Body = http.NoBody
		}
		return nil, resp, &DialError{Status: resp.StatusCode, Body: body}
	}
	if dialErr != nil {
		return nil, resp, dialErr
	}
	if conn.Subprotocol() != stream.Subprotocol {
		_ = conn.CloseNow()
		return nil, resp, &stream.ProtocolError{Code: stream.CloseProtocol, Msg: "server did not negotiate " + stream.Subprotocol}
	}
	conn.SetReadLimit(maxMessage)
	return &wsConn{
		conn: conn,
		info: stream.ConnInfo{Transport: "ws", Remote: parsed.Host, Subprotocol: conn.Subprotocol(), MaxMessage: maxMessage},
	}, resp, nil
}

func Accept(w http.ResponseWriter, r *http.Request, o AcceptOptions) (stream.Conn, error) {
	if _, ok := w.(http.Hijacker); !ok {
		return nil, errors.New("wsconn: ResponseWriter does not implement http.Hijacker")
	}
	maxMessage := o.MaxMessage
	if maxMessage <= 0 {
		maxMessage = defaultMaxMessage
	}
	conn, err := websocket.Accept(deadlineClearingWriter{ResponseWriter: w}, r, &websocket.AcceptOptions{
		Subprotocols: []string{stream.Subprotocol}, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	if conn.Subprotocol() != stream.Subprotocol {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("wsconn: client did not offer %s", stream.Subprotocol)
	}
	conn.SetReadLimit(maxMessage)
	remote := r.RemoteAddr
	if remote == "" && r.URL != nil {
		remote = r.URL.Host
	}
	return &wsConn{
		conn: conn,
		info: stream.ConnInfo{Transport: "ws", Remote: remote, Subprotocol: conn.Subprotocol(), MaxMessage: maxMessage},
	}, nil
}

type deadlineClearingWriter struct{ http.ResponseWriter }

func (w deadlineClearingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("wsconn: ResponseWriter does not implement http.Hijacker")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("wsconn: clear hijacked connection deadline: %w", err)
	}
	return conn, rw, nil
}

func (w deadlineClearingWriter) WriteHeaderNow() {
	if now, ok := w.ResponseWriter.(interface{ WriteHeaderNow() }); ok {
		now.WriteHeaderNow()
	}
}

type wsConn struct {
	conn *websocket.Conn
	info stream.ConnInfo

	closeMu      sync.Mutex
	closeStarted bool
	closeDone    chan error
}

func (c *wsConn) Read(ctx context.Context) ([]byte, error) {
	typ, message, err := c.conn.Read(ctx)
	if err != nil {
		var closeErr websocket.CloseError
		if errors.As(err, &closeErr) {
			return nil, &stream.CloseError{Code: stream.CloseCode(closeErr.Code), Reason: closeErr.Reason, Remote: true}
		}
		if websocket.CloseStatus(err) == websocket.StatusMessageTooBig || strings.Contains(err.Error(), "read limited at") {
			return nil, &stream.ProtocolError{Code: stream.CloseTooBig, Msg: "WebSocket message exceeds read limit"}
		}
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, &stream.ProtocolError{Code: stream.CloseUnsupported, Msg: "binary WebSocket message"}
	}
	if !utf8.Valid(message) {
		return nil, &stream.ProtocolError{Code: stream.CloseProtocol, Msg: "text message is not valid UTF-8"}
	}
	return message, nil
}

func (c *wsConn) Write(ctx context.Context, message []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.conn.Write(ctx, websocket.MessageText, message)
}

func (c *wsConn) Close(code stream.CloseCode, reason string) error {
	done := c.startClose(func() error { return c.conn.Close(websocket.StatusCode(code), reason) })
	return waitClose(done)
}

func (c *wsConn) CloseNow() error {
	done := c.startClose(c.conn.CloseNow)
	return waitClose(done)
}

func (c *wsConn) startClose(closeFn func() error) <-chan error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if !c.closeStarted {
		c.closeStarted = true
		c.closeDone = make(chan error, 1)
		go func() { c.closeDone <- closeFn() }()
	}
	return c.closeDone
}

func waitClose(done <-chan error) error {
	timer := time.NewTimer(closeWait)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return nil
	}
}

func (c *wsConn) Info() stream.ConnInfo { return c.info }

var _ stream.Conn = (*wsConn)(nil)
