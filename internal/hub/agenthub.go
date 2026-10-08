package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/wsconn"
	"github.com/zzjcool/homer-cli/internal/toolctl"
	"github.com/zzjcool/homer-cli/internal/web"
)

const (
	agentStreamPath       = "/agent/v1/stream"
	agentHelloMessage     = "agent 版本过旧或过新，请 homer upgrade 或重新安装"
	protocolRemovedDetail = "agent 版本过旧，请升级 homer 或重新安装"
)

type HubOptions struct {
	Logger       stream.Logger
	Session      stream.Options
	HelloTimeout time.Duration
	Version      string
}

type AgentHub struct {
	registry   *Registry
	auth       *Authenticator
	enrollment *EnrollmentManager
	opts       HubOptions
	throttle   *stream.ThrottledLogger
	instanceID string

	mu      sync.Mutex
	closing bool
	active  map[*agentConnection]struct{}
	wg      sync.WaitGroup
}

type agentConnection struct {
	conn        stream.Conn
	session     *stream.Session
	sessionConn *touchConn
	agentID     string
}

func NewAgentHub(reg *Registry, auth *Authenticator, enr *EnrollmentManager, o HubOptions) *AgentHub {
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	if o.HelloTimeout <= 0 {
		o.HelloTimeout = 10 * time.Second
	}
	if o.Version == "" {
		o.Version = web.Version
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	if o.Session.Logger == nil {
		o.Session.Logger = o.Logger
	}
	h := &AgentHub{
		registry: reg, auth: auth, enrollment: enr, opts: o,
		throttle:   stream.NewThrottledLogger(o.Logger, time.Minute),
		instanceID: newInstanceID(), active: make(map[*agentConnection]struct{}),
	}
	if h.auth == nil {
		h.auth = &Authenticator{Enrollment: enr}
	} else if h.auth.Enrollment == nil {
		h.auth.Enrollment = enr
	}
	if h.enrollment == nil && h.auth != nil {
		h.enrollment = h.auth.Enrollment
	}
	if h.enrollment != nil {
		h.auth.Enrollment = h.enrollment
	}
	if h.enrollment != nil {
		h.enrollment.SetRevokeHook(func(agentID string) {
			if !h.Kick(agentID, stream.CloseRevoked, "agent secret revoked") {
				h.log("agent secret revoked agent=%q (no live session)", agentID)
			}
		})
	}
	return h
}

func newInstanceID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err == nil {
		return hex.EncodeToString(id[:])
	}
	return fmt.Sprintf("dev-%d", time.Now().UnixNano())
}

func (h *AgentHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.registry == nil {
		writeHubJSONError(w, http.StatusInternalServerError, "internal", "hub registry 未初始化")
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch path {
	case agentStreamPath:
		if r.Method != http.MethodGet {
			h.throttle.Log("agent-stream-method-rejected", fmt.Sprintf("hub: rejected agent stream method=%q remote=%q", r.Method, r.RemoteAddr))
			w.Header().Set("Allow", http.MethodGet)
			writeHubJSONError(w, http.StatusMethodNotAllowed, "method-not-allowed", "不支持的请求方法")
			return
		}
		h.serveStream(w, r)
	case "/agent/v1/enroll", "/agent/v1/register", "/agent/v1/poll", "/agent/v1/report":
		h.throttle.Log("agent-legacy-protocol", fmt.Sprintf("hub: returned protocol-removed for legacy agent endpoint=%q remote=%q", path, r.RemoteAddr))
		writeHubJSONError(w, http.StatusGone, "protocol-removed", protocolRemovedDetail)
	default:
		if strings.HasPrefix(path, "/agent/") || path == "/agent" {
			h.throttle.Log("agent-unknown-endpoint", fmt.Sprintf("hub: rejected unknown agent endpoint=%q remote=%q", path, r.RemoteAddr))
			writeHubJSONError(w, http.StatusNotFound, "not-found", "请求的资源不存在")
			return
		}
		http.NotFound(w, r)
	}
}

func (h *AgentHub) serveStream(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.auth.Authenticate(authBearerToken(r))
	if !ok {
		h.throttle.Log("agent-auth-failed", fmt.Sprintf("hub: agent stream authentication failed remote=%q", r.RemoteAddr))
		writeHubJSONError(w, http.StatusUnauthorized, "unauthorized", "未授权：请提供有效的 Bearer token")
		return
	}
	if !supportsStreamSubprotocol(r.Header.Values("Sec-WebSocket-Protocol")) {
		h.throttle.Log("agent-subprotocol-rejected", fmt.Sprintf("hub: agent stream rejected unsupported WebSocket subprotocol remote=%q", r.RemoteAddr))
		writeHubJSONError(w, http.StatusBadRequest, stream.CodeUnsupported, "unsupported-version: agent must offer "+stream.Subprotocol)
		return
	}
	if _, ok := w.(http.Hijacker); !ok {
		h.throttle.Log("agent-hijacker-unavailable", fmt.Sprintf("hub: cannot upgrade agent stream remote=%q: ResponseWriter does not implement http.Hijacker", r.RemoteAddr))
		writeHubJSONError(w, http.StatusInternalServerError, "websocket-unavailable", "HTTP ResponseWriter does not implement http.Hijacker")
		return
	}
	if !h.beginRequest() {
		writeHubJSONError(w, http.StatusServiceUnavailable, "hub-shutting-down", "hub 正在关闭，请稍后重试")
		return
	}
	defer h.wg.Done()

	conn, err := wsconn.Accept(w, r, wsconn.AcceptOptions{MaxMessage: int64(h.maxFrame()) + 4096})
	if err != nil {
		h.throttle.Log("agent-upgrade-failed", fmt.Sprintf("hub: agent stream WebSocket upgrade failed remote=%q: %v", r.RemoteAddr, err))
		return
	}
	connection := &agentConnection{conn: conn}
	if !h.track(connection) {
		_ = conn.Close(stream.CloseGoingAway, "hub restarting")
		return
	}
	defer h.untrack(connection)

	frame, hello, err := h.readHello(r.Context(), conn)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			h.throttle.Log("agent-hello-timeout", fmt.Sprintf("hub: agent stream hello timed out remote=%q", r.RemoteAddr))
		} else {
			h.throttle.Log("agent-hello-rejected", fmt.Sprintf("hub: agent stream rejected hello remote=%q: %v", r.RemoteAddr, err))
		}
		return
	}
	if hello.Proto != stream.ProtocolVersion {
		h.rejectHello(conn, frame.ID, stream.CodeUnsupported, agentHelloMessage, stream.CloseProtocol)
		h.throttle.Log("agent-version-rejected", fmt.Sprintf("hub: agent stream protocol mismatch remote=%q agent_proto=%d hub_proto=%d", r.RemoteAddr, hello.Proto, stream.ProtocolVersion))
		return
	}
	if principal.Kind == PrincipalSecret && principal.AgentID != hello.AgentID {
		h.rejectHello(conn, frame.ID, "agent-id-mismatch", "the bearer secret is bound to a different agent ID", stream.CloseRevoked)
		h.throttle.Log("agent-id-mismatch", fmt.Sprintf("hub: agent stream identity mismatch remote=%q credential_agent=%q hello_agent=%q", r.RemoteAddr, principal.AgentID, hello.AgentID))
		return
	}
	connectionSecret := ""
	if principal.Kind == PrincipalSecret {
		connectionSecret = authBearerToken(r)
		if h.enrollment == nil || !h.enrollment.VerifySecret(hello.AgentID, connectionSecret) {
			h.rejectHello(conn, frame.ID, stream.CodeUnauthorized, "agent secret was revoked during hello", stream.CloseRevoked)
			h.throttle.Log("agent-secret-revoked-during-hello", fmt.Sprintf("hub: agent stream secret was revoked during hello remote=%q agent=%q", r.RemoteAddr, hello.AgentID))
			return
		}
	}

	agentSecret := ""
	if principal.Kind == PrincipalEnrollCode {
		if h.enrollment == nil {
			h.rejectHello(conn, frame.ID, stream.CodeUnauthorized, "enrollment is disabled", stream.CloseRevoked)
			h.throttle.Log("agent-enroll-disabled", fmt.Sprintf("hub: agent stream enrollment disabled remote=%q agent=%q", r.RemoteAddr, hello.AgentID))
			return
		}
		var redeemErr error
		agentSecret, redeemErr = h.enrollment.Redeem(principal.Code)
		if redeemErr != nil {
			h.rejectHello(conn, frame.ID, stream.CodeUnauthorized, redeemErr.Error(), stream.CloseRevoked)
			h.throttle.Log("agent-enroll-rejected", fmt.Sprintf("hub: agent stream enrollment rejected remote=%q agent=%q: %v", r.RemoteAddr, hello.AgentID, redeemErr))
			return
		}
		// Bind before welcome: if the agent immediately reconnects after
		// receiving its secret, its next handshake must already verify.
		h.enrollment.BindAgent(hello.AgentID, agentSecret)
		connectionSecret = agentSecret
	}

	welcome := WelcomeResult{
		Proto: stream.ProtocolVersion, HubVersion: h.opts.Version,
		InstanceID: h.instanceID, AgentSecret: agentSecret,
		PingIntervalMs: int(h.pingInterval().Milliseconds()),
		PingTimeoutMs:  int(h.pingTimeout().Milliseconds()),
		MaxFrame:       h.maxFrame(),
	}
	if err := h.writeHelloResult(r.Context(), conn, frame.ID, welcome); err != nil {
		h.throttle.Log("agent-welcome-failed", fmt.Sprintf("hub: failed to write agent welcome remote=%q agent=%q: %v", r.RemoteAddr, hello.AgentID, err))
		return
	}

	sessionConn := &touchConn{
		Conn: conn, hub: h, registry: h.registry, agentID: hello.AgentID,
		closeDone: make(chan struct{}), readReady: make(chan struct{}),
	}
	session := stream.NewSession(sessionConn, h.opts.Session)
	sessionConn.session = session
	session.SetHeartbeat(h.pingInterval(), h.pingTimeout())
	session.OnEvent(MethodHeartbeat, h.heartbeatHandler(hello.AgentID, session))

	info := AgentInfo{
		AgentID: hello.AgentID, Hostname: hello.Hostname, Version: hello.Version,
		Drift: hello.Drift, Host: hello.Host, Tools: reportedHelloTools(hello.Tools),
		caps: hello.Caps, LastSeen: time.Now(),
	}
	// Start the session before publishing it in Registry so a concurrent Kick
	// can close it. Gate inbound reads until after Attach completes so a fast
	// first heartbeat cannot update a missing record and then be overwritten by
	// the hello snapshot.
	runDone := make(chan error, 1)
	go func() { runDone <- session.Run(r.Context()) }()
	previous, attached := h.attachSession(connection, info, session, sessionConn)
	if !attached {
		session.Close(stream.CloseGoingAway, "hub restarting")
		<-runDone
		<-sessionConn.closeDone
		return
	}
	sessionConn.startRead()
	if previous != nil && previous != session {
		h.log("hub: agent stream superseding prior connection agent=%q remote=%q", hello.AgentID, r.RemoteAddr)
		h.closeSession(previous, stream.CloseSuperseded, "same agent ID connected elsewhere")
	}
	// Close a secret-authenticated session if revocation won a race with the
	// successful hello write or registry attach. A later revoke is handled by
	// the manager's hook, which sees this attached session.
	if connectionSecret != "" && (h.enrollment == nil || !h.enrollment.VerifySecret(hello.AgentID, connectionSecret)) {
		h.throttle.Log("agent-secret-revoked-during-attach", fmt.Sprintf("hub: agent stream secret revoked while attaching agent=%q", hello.AgentID))
		h.closeSession(session, stream.CloseRevoked, "agent secret revoked")
	}

	h.log("hub: agent stream connected agent=%q hostname=%q remote=%q transport=%q", hello.AgentID, clipText(hello.Hostname, 80), r.RemoteAddr, conn.Info().Transport)
	runErr := <-runDone
	// Session.Close initiates the WebSocket close handshake concurrently with
	// the Run loop's return. Keep the HTTP handler alive until the transport has
	// finished sending that close frame; otherwise net/http would close the
	// hijacked socket first and peers would see EOF instead of the close code.
	<-sessionConn.closeDone
	h.registry.Detach(hello.AgentID, session)
	h.log("hub: agent stream disconnected agent=%q remote=%q: %v", hello.AgentID, r.RemoteAddr, runErr)
}

func (h *AgentHub) readHello(parent context.Context, conn stream.Conn) (*stream.Frame, HelloParams, error) {
	type helloReadResult struct {
		message []byte
		err     error
	}
	readDone := make(chan helloReadResult, 1)
	readCtx, cancelRead := context.WithCancel(parent)
	defer cancelRead()
	// coder/websocket treats a context deadline on Read as an immediate
	// transport abort. Use a cancelable request context and implement the
	// handshake timeout by sending a close frame before canceling the read.
	go func() {
		message, err := conn.Read(readCtx)
		readDone <- helloReadResult{message: message, err: err}
	}()
	timer := time.NewTimer(h.opts.HelloTimeout)
	defer timer.Stop()
	var message []byte
	select {
	case result := <-readDone:
		if result.err != nil {
			var protocolErr *stream.ProtocolError
			if errors.As(result.err, &protocolErr) {
				_ = conn.Close(protocolErr.Code, protocolErr.Msg)
			}
			return nil, HelloParams{}, result.err
		}
		message = result.message
	case <-parent.Done():
		_ = conn.CloseNow()
		cancelRead()
		return nil, HelloParams{}, parent.Err()
	case <-timer.C:
		_ = conn.Close(stream.ClosePolicy, "hello timeout")
		cancelRead()
		return nil, HelloParams{}, context.DeadlineExceeded
	}
	frame, err := stream.DecodeFrame(message, h.maxFrame())
	if err != nil {
		var protocolErr *stream.ProtocolError
		if errors.As(err, &protocolErr) {
			_ = conn.Close(protocolErr.Code, protocolErr.Msg)
		}
		return nil, HelloParams{}, err
	}
	if frame.T != stream.TReq || frame.M != MethodHello {
		_ = conn.Close(stream.ClosePolicy, "first frame must be hello")
		return nil, HelloParams{}, errors.New("first frame must be a hello request")
	}
	if frame.ID == "" {
		_ = conn.Close(stream.CloseProtocol, "hello request ID is required")
		return nil, HelloParams{}, errors.New("hello request ID is required")
	}
	var hello HelloParams
	if err := json.Unmarshal(frame.P, &hello); err != nil {
		h.rejectHello(conn, frame.ID, stream.CodeBadRequest, "invalid hello parameters", stream.ClosePolicy)
		return nil, HelloParams{}, fmt.Errorf("decode hello parameters: %w", err)
	}
	hello.AgentID = strings.TrimSpace(hello.AgentID)
	if hello.AgentID == "" {
		h.rejectHello(conn, frame.ID, stream.CodeBadRequest, "agentId is required", stream.ClosePolicy)
		return nil, HelloParams{}, errors.New("hello agentId is required")
	}
	return frame, hello, nil
}

func (h *AgentHub) rejectHello(conn stream.Conn, requestID, code, message string, closeCode stream.CloseCode) {
	frame := &stream.Frame{T: stream.TRes, ID: requestID, E: &stream.Error{Code: code, Message: message}}
	if encoded, err := stream.EncodeFrame(frame, h.maxFrame()); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), stream.DefaultWriteTimeout)
		_ = conn.Write(ctx, encoded)
		cancel()
	}
	_ = conn.Close(closeCode, message)
}

func (h *AgentHub) writeHelloResult(parent context.Context, conn stream.Conn, requestID string, welcome WelcomeResult) error {
	payload, err := json.Marshal(welcome)
	if err != nil {
		return err
	}
	encoded, err := stream.EncodeFrame(&stream.Frame{T: stream.TRes, ID: requestID, OK: true, P: payload}, h.maxFrame())
	if err != nil {
		return err
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, stream.DefaultWriteTimeout)
	defer cancel()
	return conn.Write(ctx, encoded)
}

func (h *AgentHub) heartbeatHandler(agentID string, session *stream.Session) stream.EventHandler {
	return func(ctx context.Context, method string, payload json.RawMessage) {
		if method != MethodHeartbeat || ctx.Err() != nil {
			return
		}
		var heartbeat HeartbeatParams
		if err := json.Unmarshal(payload, &heartbeat); err != nil {
			h.throttle.Log("agent-heartbeat-malformed", fmt.Sprintf("hub: ignored malformed heartbeat agent=%q: %v", agentID, err))
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if current, ok := h.registry.Session(agentID); !ok || current != session {
			return
		}
		h.registry.UpdateVersion(agentID, heartbeat.Version)
		if heartbeat.Drift != nil {
			h.registry.UpdateDrift(agentID, *heartbeat.Drift)
		}
		if heartbeat.Host != nil {
			h.registry.UpdateHost(agentID, *heartbeat.Host)
		}
		if heartbeat.Tools != nil {
			h.registry.UpdateTools(agentID, *heartbeat.Tools)
		}
	}
}

func reportedHelloTools(tools *[]toolctl.Status) []toolctl.Status {
	if tools == nil {
		return nil
	}
	if *tools == nil {
		return []toolctl.Status{}
	}
	return *tools
}

func supportsStreamSubprotocol(values []string) bool {
	for _, value := range values {
		for _, subprotocol := range strings.Split(value, ",") {
			if strings.TrimSpace(subprotocol) == stream.Subprotocol {
				return true
			}
		}
	}
	return false
}

func writeHubJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSON, err := json.Marshal(struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
	if err != nil {
		writeJSON = []byte(`{"error":{"code":"internal","message":"internal error"}}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(writeJSON)
}

func (h *AgentHub) beginRequest() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return false
	}
	h.wg.Add(1)
	return true
}

func (h *AgentHub) track(connection *agentConnection) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return false
	}
	h.active[connection] = struct{}{}
	return true
}

func (h *AgentHub) untrack(connection *agentConnection) {
	h.mu.Lock()
	delete(h.active, connection)
	h.mu.Unlock()
}

func (h *AgentHub) attachSession(connection *agentConnection, info AgentInfo, session *stream.Session, sessionConn *touchConn) (*stream.Session, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, false
	}
	connection.agentID = info.AgentID
	connection.session = session
	connection.sessionConn = sessionConn
	return h.registry.Attach(info, session), true
}

func (h *AgentHub) log(format string, args ...any) {
	if h != nil && h.opts.Logger != nil {
		h.opts.Logger.Printf(format, args...)
	}
}

func (h *AgentHub) maxFrame() int {
	if h.opts.Session.MaxFrame > 0 {
		return h.opts.Session.MaxFrame
	}
	return stream.DefaultMaxFrame
}

func (h *AgentHub) pingInterval() time.Duration {
	if h.opts.Session.PingInterval > 0 {
		return h.opts.Session.PingInterval
	}
	return stream.DefaultPingInterval
}

func (h *AgentHub) pingTimeout() time.Duration {
	if h.opts.Session.PingTimeout > 0 {
		return h.opts.Session.PingTimeout
	}
	return stream.DefaultPingTimeout
}

func (h *AgentHub) Kick(agentID string, code stream.CloseCode, reason string) bool {
	if h == nil || h.registry == nil {
		return false
	}
	session, ok := h.registry.Session(agentID)
	if !ok {
		return false
	}
	h.log("hub: kicking agent stream agent=%q code=%d reason=%q", agentID, code, reason)
	return h.closeSession(session, code, reason)
}

// closeSession begins the transport close handshake before canceling Session's
// read context. coder/websocket treats a canceled Read as an abort, so sending
// the close frame first is required to preserve custom close codes. wsconn
// bounds this wait to a short close-handshake grace period before pending RPCs
// are failed.
func (h *AgentHub) closeSession(session *stream.Session, code stream.CloseCode, reason string) bool {
	if session == nil {
		return false
	}
	h.mu.Lock()
	var conn stream.Conn
	var sessionConn *touchConn
	for connection := range h.active {
		if connection.session == session {
			conn = connection.conn
			sessionConn = connection.sessionConn
			break
		}
	}
	h.mu.Unlock()
	if sessionConn != nil {
		_ = sessionConn.Close(code, reason)
	} else if conn != nil {
		_ = conn.Close(code, reason)
	}
	session.Close(code, reason)
	return true
}

func (h *AgentHub) Shutdown(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.mu.Lock()
	h.closing = true
	type activeClose struct {
		conn        stream.Conn
		session     *stream.Session
		sessionConn *touchConn
	}
	connections := make([]activeClose, 0, len(h.active))
	for connection := range h.active {
		connections = append(connections, activeClose{conn: connection.conn, session: connection.session, sessionConn: connection.sessionConn})
	}
	h.mu.Unlock()
	for _, connection := range connections {
		connection := connection
		go func() {
			if connection.sessionConn != nil {
				_ = connection.sessionConn.Close(stream.CloseGoingAway, "hub restarting")
			} else if connection.conn != nil {
				_ = connection.conn.Close(stream.CloseGoingAway, "hub restarting")
			}
			if connection.session != nil {
				connection.session.Close(stream.CloseGoingAway, "hub restarting")
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type touchConn struct {
	stream.Conn
	hub       *AgentHub
	registry  *Registry
	agentID   string
	session   *stream.Session
	closeDone chan struct{}
	closeOnce sync.Once
	closeMu   sync.Mutex
	closed    bool
	readReady chan struct{}
	readyOnce sync.Once
}

func (c *touchConn) startRead() {
	c.readyOnce.Do(func() { close(c.readReady) })
}

func (h *AgentHub) touchSession(agentID string, session *stream.Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if current, ok := h.registry.Session(agentID); ok && current == session {
		h.registry.Touch(agentID)
	}
}

func (c *touchConn) Read(ctx context.Context) ([]byte, error) {
	if c.readReady != nil {
		select {
		case <-c.readReady:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	message, err := c.Conn.Read(ctx)
	if err == nil && c.hub != nil && c.session != nil {
		c.hub.touchSession(c.agentID, c.session)
	}
	return message, err
}

func (c *touchConn) Close(code stream.CloseCode, reason string) error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	err := c.Conn.Close(code, reason)
	c.closeOnce.Do(func() { close(c.closeDone) })
	return err
}

func (c *touchConn) CloseNow() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	err := c.Conn.CloseNow()
	c.closeOnce.Do(func() { close(c.closeDone) })
	return err
}

var _ http.Handler = (*AgentHub)(nil)
