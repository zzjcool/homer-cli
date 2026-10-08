package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/wsconn"
	"github.com/zzjcool/homer-cli/internal/toolctl"
	"github.com/zzjcool/homer-cli/internal/web"
)

const hubTestToken = "hub-test-token"

type synchronizedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *synchronizedLog) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(&l.b, format+"\n", args...)
}

func (l *synchronizedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newTestAgentHub(t *testing.T, registry *Registry, enrollment *EnrollmentManager, options HubOptions) (*AgentHub, *httptest.Server) {
	t.Helper()
	if options.Logger == nil {
		options.Logger = &synchronizedLog{}
	}
	if options.HelloTimeout == 0 {
		options.HelloTimeout = 300 * time.Millisecond
	}
	auth := &Authenticator{Token: hubTestToken, Enrollment: enrollment}
	hub := NewAgentHub(registry, auth, enrollment, options)
	server := httptest.NewServer(hub)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = hub.Shutdown(ctx)
		server.Close()
	})
	return hub, server
}

func dialTestAgent(t *testing.T, serverURL, bearer string) stream.Conn {
	t.Helper()
	header := make(http.Header)
	if bearer != "" {
		header.Set("Authorization", "Bearer "+bearer)
	}
	conn, _, err := wsconn.Dial(context.Background(), serverURL+agentStreamPath, wsconn.DialOptions{Header: header})
	if err != nil {
		t.Fatalf("wsconn.Dial(%q): %v", serverURL+agentStreamPath, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func sendTestFrame(t *testing.T, conn stream.Conn, frame *stream.Frame) {
	t.Helper()
	encoded, err := stream.EncodeFrame(frame, stream.DefaultMaxFrame)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, encoded); err != nil {
		t.Fatalf("write frame %q: %v", frame.T, err)
	}
}

func readTestFrame(t *testing.T, conn stream.Conn) *stream.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	message, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	frame, err := stream.DecodeFrame(message, stream.DefaultMaxFrame)
	if err != nil {
		t.Fatalf("decode frame %q: %v", message, err)
	}
	return frame
}

func sendTestHello(t *testing.T, conn stream.Conn, agentID string, proto int) WelcomeResult {
	t.Helper()
	if proto == 0 {
		proto = stream.ProtocolVersion
	}
	payload, err := json.Marshal(HelloParams{
		Proto: proto, AgentID: agentID, Hostname: "host-" + agentID, Version: "v-test",
		Caps:  []string{string(TaskKindStatus), string(TaskKindDiff), string(TaskKindPush), string(TaskKindPull), string(TaskKindSSHKey), string(TaskKindSecret), string(TaskKindUpgrade), string(TaskKindToolUpgrade), MethodInspect},
		Drift: &AgentDrift{Push: 1}, Host: &HostSnapshot{OS: "linux"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sendTestFrame(t, conn, &stream.Frame{T: stream.TReq, ID: "hello-1", M: MethodHello, P: payload})
	frame := readTestFrame(t, conn)
	if frame.T != stream.TRes || frame.ID != "hello-1" || !frame.OK {
		t.Fatalf("hello response = %+v", frame)
	}
	var welcome WelcomeResult
	if err := json.Unmarshal(frame.P, &welcome); err != nil {
		t.Fatalf("decode welcome: %v", err)
	}
	return welcome
}

func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}

func awaitRemoteClose(conn stream.Conn) <-chan error {
	result := make(chan error, 1)
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			message, err := conn.Read(ctx)
			cancel()
			if err != nil {
				result <- err
				return
			}
			frame, err := stream.DecodeFrame(message, stream.DefaultMaxFrame)
			if err != nil {
				result <- err
				return
			}
			if frame.T == stream.TPing {
				encoded, encodeErr := stream.EncodeFrame(&stream.Frame{T: stream.TPong, ID: frame.ID}, stream.DefaultMaxFrame)
				if encodeErr != nil {
					result <- encodeErr
					return
				}
				writeCtx, writeCancel := context.WithTimeout(context.Background(), time.Second)
				writeErr := conn.Write(writeCtx, encoded)
				writeCancel()
				if writeErr != nil {
					result <- writeErr
					return
				}
			}
		}
	}()
	return result
}

func expectRemoteClose(t *testing.T, result <-chan error, code stream.CloseCode) {
	t.Helper()
	select {
	case err := <-result:
		var closeErr *stream.CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != code || !closeErr.Remote {
			t.Fatalf("remote close = %T %v, want code %d", err, err, code)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("did not receive remote close code %d", code)
	}
}

func TestAgentHubHelloEnrollAndHeartbeat(t *testing.T) {
	registry := NewRegistry()
	enrollment := NewEnrollmentManager()
	code, err := enrollment.Mint(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	hub, server := newTestAgentHub(t, registry, enrollment, HubOptions{
		Version: "hub-test", Session: stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour},
	})
	conn := dialTestAgent(t, server.URL, code)
	welcome := sendTestHello(t, conn, "agent-a", stream.ProtocolVersion)
	if welcome.Proto != stream.ProtocolVersion || welcome.HubVersion != "hub-test" || welcome.InstanceID == "" || welcome.AgentSecret == "" || welcome.MaxFrame != stream.DefaultMaxFrame {
		t.Fatalf("welcome = %+v", welcome)
	}
	if welcome.PingIntervalMs != int(time.Hour.Milliseconds()) || welcome.PingTimeoutMs != int((2*time.Hour).Milliseconds()) {
		t.Fatalf("welcome heartbeat = %d/%d", welcome.PingIntervalMs, welcome.PingTimeoutMs)
	}
	if !enrollment.VerifySecret("agent-a", welcome.AgentSecret) {
		t.Fatal("welcome secret was not bound to hello agentId")
	}
	if enrollment.ValidCode(code) {
		t.Fatal("hello did not burn the enrollment code")
	}
	hb, err := json.Marshal(HeartbeatParams{
		Version: "v-next", Drift: &AgentDrift{Pull: 3}, Host: &HostSnapshot{OS: "linux", Arch: "amd64"},
		Tools: &[]toolctl.Status{{ID: "pi", Version: "1.2.3"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Send heartbeat immediately after welcome, before waiting for Registry
	// visibility. The hub must not lose this first event while publishing hello.
	sendTestFrame(t, conn, &stream.Frame{T: stream.TEvt, M: MethodHeartbeat, P: hb})
	await(t, func() bool {
		updated, ok := registry.Get("agent-a")
		return ok && updated.Version == "v-next" && updated.Drift != nil && updated.Drift.Pull == 3 && updated.Host != nil && updated.Host.Arch == "amd64" && len(updated.Tools) == 1 && updated.Tools[0].Version == "1.2.3"
	})
	info, ok := registry.Get("agent-a")
	if !ok || info.AgentID != "agent-a" || info.Hostname != "host-agent-a" || info.Stale {
		t.Fatalf("registered hello data after immediate heartbeat = %+v, found=%v", info, ok)
	}
	if !hub.Kick("agent-a", stream.CloseRemoved, "test remove") {
		t.Fatal("Kick() did not find the connected session")
	}
}

func TestAgentHubAuthenticatorAndProtocolGate(t *testing.T) {
	registry := NewRegistry()
	log := &synchronizedLog{}
	_, server := newTestAgentHub(t, registry, NewEnrollmentManager(), HubOptions{Logger: log})

	for _, test := range []struct {
		name       string
		bearer     string
		wantStatus int
		wantCode   string
	}{
		{name: "missing bearer", wantStatus: http.StatusUnauthorized, wantCode: "unauthorized"},
		{name: "wrong bearer", bearer: "wrong", wantStatus: http.StatusUnauthorized, wantCode: "unauthorized"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, server.URL+agentStreamPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.bearer != "" {
				request.Header.Set("Authorization", "Bearer "+test.bearer)
			}
			request.Header.Set("Sec-WebSocket-Protocol", stream.Subprotocol)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != test.wantStatus || hubErrorCode(body) != test.wantCode {
				t.Fatalf("response = %d %s", response.StatusCode, body)
			}
		})
	}

	request, err := http.NewRequest(http.MethodGet, server.URL+agentStreamPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+hubTestToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadRequest || hubErrorCode(body) != stream.CodeUnsupported {
		t.Fatalf("missing subprotocol response = %d %s", response.StatusCode, body)
	}
	if !strings.Contains(log.String(), "authentication failed") || !strings.Contains(log.String(), "unsupported WebSocket subprotocol") {
		t.Fatalf("auth/protocol refusals were not logged: %s", log.String())
	}
}

func TestAgentHubLegacyTombstonesAndUnknownPaths(t *testing.T) {
	hub := NewAgentHub(NewRegistry(), &Authenticator{Token: hubTestToken}, nil, HubOptions{})
	for _, path := range []string{"/agent/v1/enroll", "/agent/v1/register", "/agent/v1/poll", "/agent/v1/report"} {
		response := httptest.NewRecorder()
		hub.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"agentId":"ignored"}`)))
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode tombstone %s: %v", path, err)
		}
		if response.Code != http.StatusGone || envelope.Error.Code != "protocol-removed" || !strings.Contains(envelope.Error.Message, "升级 homer") {
			t.Errorf("tombstone %s = %d %+v", path, response.Code, envelope.Error)
		}
	}
	for _, path := range []string{"/agent/v1/other", "/agent/other"} {
		response := httptest.NewRecorder()
		hub.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("unknown endpoint %s status = %d", path, response.Code)
		}
	}
}

func TestAgentHubResponseWriterMustHijack(t *testing.T) {
	log := &synchronizedLog{}
	hub := NewAgentHub(NewRegistry(), &Authenticator{Token: hubTestToken}, nil, HubOptions{Logger: log})
	request := httptest.NewRequest(http.MethodGet, agentStreamPath, nil)
	request.Header.Set("Authorization", "Bearer "+hubTestToken)
	request.Header.Set("Sec-WebSocket-Protocol", stream.Subprotocol)
	writer := &nonHijackerRecorder{header: make(http.Header)}
	hub.ServeHTTP(writer, request)
	if writer.status != http.StatusInternalServerError || !strings.Contains(writer.body.String(), "http.Hijacker") {
		t.Fatalf("non-Hijacker response = %d %s", writer.status, writer.body.String())
	}
	if !strings.Contains(log.String(), "does not implement http.Hijacker") {
		t.Fatalf("non-Hijacker failure was not logged: %s", log.String())
	}
}

type nonHijackerRecorder struct {
	header http.Header
	status int
	body   strings.Builder
}

func (w *nonHijackerRecorder) Header() http.Header    { return w.header }
func (w *nonHijackerRecorder) WriteHeader(status int) { w.status = status }
func (w *nonHijackerRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(body)
}

func TestHelloTimeout(t *testing.T) {
	_, server := newTestAgentHub(t, NewRegistry(), NewEnrollmentManager(), HubOptions{HelloTimeout: 40 * time.Millisecond})
	conn := dialTestAgent(t, server.URL, hubTestToken)
	expectRemoteClose(t, awaitRemoteClose(conn), stream.ClosePolicy)
}

func TestFrameBeforeHello(t *testing.T) {
	_, server := newTestAgentHub(t, NewRegistry(), NewEnrollmentManager(), HubOptions{})
	conn := dialTestAgent(t, server.URL, hubTestToken)
	sendTestFrame(t, conn, &stream.Frame{T: stream.TPing, ID: "before-hello"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := conn.Read(ctx)
	var closeErr *stream.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != stream.ClosePolicy {
		t.Fatalf("frame-before-hello error = %T %v, want close 1008", err, err)
	}
}

func TestHelloIDMismatch(t *testing.T) {
	registry := NewRegistry()
	enrollment := NewEnrollmentManager()
	enrollment.BindAgent("bound-agent", "bound-secret")
	_, server := newTestAgentHub(t, registry, enrollment, HubOptions{})
	conn := dialTestAgent(t, server.URL, "bound-secret")
	payload, err := json.Marshal(HelloParams{Proto: stream.ProtocolVersion, AgentID: "other-agent"})
	if err != nil {
		t.Fatal(err)
	}
	sendTestFrame(t, conn, &stream.Frame{T: stream.TReq, ID: "hello-mismatch", M: MethodHello, P: payload})
	response := readTestFrame(t, conn)
	if response.T != stream.TRes || response.E == nil || response.E.Code != "agent-id-mismatch" {
		t.Fatalf("agent ID mismatch response = %+v", response)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = conn.Read(ctx)
	var closeErr *stream.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != stream.CloseRevoked {
		t.Fatalf("agent ID mismatch close = %T %v, want 4401", err, err)
	}
}

func TestAgentHubUnsupportedHelloProtocol(t *testing.T) {
	_, server := newTestAgentHub(t, NewRegistry(), NewEnrollmentManager(), HubOptions{})
	conn := dialTestAgent(t, server.URL, hubTestToken)
	payload, err := json.Marshal(HelloParams{Proto: stream.ProtocolVersion + 1, AgentID: "future-agent"})
	if err != nil {
		t.Fatal(err)
	}
	sendTestFrame(t, conn, &stream.Frame{T: stream.TReq, ID: "hello-future", M: MethodHello, P: payload})
	response := readTestFrame(t, conn)
	if response.E == nil || response.E.Code != stream.CodeUnsupported || !strings.Contains(response.E.Message, "重新安装") {
		t.Fatalf("unsupported protocol response = %+v", response)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = conn.Read(ctx)
	var closeErr *stream.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != stream.CloseProtocol {
		t.Fatalf("unsupported protocol close = %T %v, want 1002", err, err)
	}
}

func TestMalformedJSONFromAgent(t *testing.T) {
	_, server := newTestAgentHub(t, NewRegistry(), NewEnrollmentManager(), HubOptions{Session: stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour}})
	conn := dialTestAgent(t, server.URL, hubTestToken)
	sendTestHello(t, conn, "malformed-agent", stream.ProtocolVersion)
	closeResult := awaitRemoteClose(conn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, []byte("{")); err != nil {
		t.Fatal(err)
	}
	expectRemoteClose(t, closeResult, stream.CloseProtocol)
}

func TestOversizeFromAgent(t *testing.T) {
	_, server := newTestAgentHub(t, NewRegistry(), NewEnrollmentManager(), HubOptions{
		Session: stream.Options{MaxFrame: 1024, PingInterval: time.Hour, PingTimeout: 2 * time.Hour},
	})
	conn := dialTestAgent(t, server.URL, hubTestToken)
	sendTestHello(t, conn, "oversize-agent", stream.ProtocolVersion)
	closeResult := awaitRemoteClose(conn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, []byte(strings.Repeat("x", 1025))); err != nil {
		t.Fatal(err)
	}
	expectRemoteClose(t, closeResult, stream.CloseTooBig)
}

func TestAgentHubWelcomeSecretIsOneTime(t *testing.T) {
	registry := NewRegistry()
	enrollment := NewEnrollmentManager()
	code, err := enrollment.Mint(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, server := newTestAgentHub(t, registry, enrollment, HubOptions{})

	enrollingConn := dialTestAgent(t, server.URL, code)
	welcome := sendTestHello(t, enrollingConn, "enrolled-agent", stream.ProtocolVersion)
	if welcome.AgentSecret == "" {
		t.Fatal("enrollment hello did not return the new per-agent secret")
	}

	secretConn := dialTestAgent(t, server.URL, welcome.AgentSecret)
	secretWelcome := sendTestHello(t, secretConn, "enrolled-agent", stream.ProtocolVersion)
	if secretWelcome.AgentSecret != "" {
		t.Fatalf("reconnecting with per-agent secret received secret again: %q", secretWelcome.AgentSecret)
	}

	tokenConn := dialTestAgent(t, server.URL, hubTestToken)
	tokenWelcome := sendTestHello(t, tokenConn, "token-agent", stream.ProtocolVersion)
	if tokenWelcome.AgentSecret != "" {
		t.Fatalf("hub-token hello received a per-agent secret: %q", tokenWelcome.AgentSecret)
	}
}

func TestAgentHubSecretHelloAgentIDMustMatch(t *testing.T) {
	registry := NewRegistry()
	enrollment := NewEnrollmentManager()
	enrollment.BindAgent("bound-agent", "bound-secret")
	_, server := newTestAgentHub(t, registry, enrollment, HubOptions{})

	conn := dialTestAgent(t, server.URL, "bound-secret")
	payload, err := json.Marshal(HelloParams{Proto: stream.ProtocolVersion, AgentID: "forged-agent"})
	if err != nil {
		t.Fatal(err)
	}
	sendTestFrame(t, conn, &stream.Frame{T: stream.TReq, ID: "hello-forged", M: MethodHello, P: payload})
	response := readTestFrame(t, conn)
	if response.E == nil || response.E.Code != "agent-id-mismatch" {
		t.Fatalf("mismatched hello response = %+v", response)
	}
	if _, ok := registry.Get("forged-agent"); ok {
		t.Fatal("secret for another identity registered forged agent")
	}
	expectRemoteClose(t, awaitRemoteClose(conn), stream.CloseRevoked)
}

func TestAgentHubRevocationKicksConnectedAgent(t *testing.T) {
	registry := NewRegistry()
	enrollment := NewEnrollmentManager()
	enrollment.BindAgent("agent-a", "agent-secret")
	_, server := newTestAgentHub(t, registry, enrollment, HubOptions{})
	conn := dialTestAgent(t, server.URL, "agent-secret")
	sendTestHello(t, conn, "agent-a", stream.ProtocolVersion)
	await(t, func() bool {
		_, ok := registry.Session("agent-a")
		return ok
	})
	closeResult := awaitRemoteClose(conn)
	if !enrollment.Revoke("agent-a") {
		t.Fatal("Revoke(agent-a) = false")
	}
	expectRemoteClose(t, closeResult, stream.CloseRevoked)
	await(t, func() bool {
		info, ok := registry.Get("agent-a")
		return ok && info.Stale
	})
}

func TestAgentHubSupersedesPreviousSession(t *testing.T) {
	registry := NewRegistry()
	_, server := newTestAgentHub(t, registry, NewEnrollmentManager(), HubOptions{Session: stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour}})
	first := dialTestAgent(t, server.URL, hubTestToken)
	sendTestHello(t, first, "same-agent", stream.ProtocolVersion)
	firstSession := waitRegistrySession(t, registry, "same-agent")
	firstAgentSession := stream.NewSession(first, stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour})
	firstAgentSession.Handle(string(TaskKindStatus), func(ctx context.Context, _ *stream.Request) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	firstAgentRun := make(chan error, 1)
	go func() { firstAgentRun <- firstAgentSession.Run(context.Background()) }()

	callDone := make(chan error, 1)
	go func() {
		_, err := NewDispatcher(registry, "").AgentStatus(context.Background(), "same-agent")
		callDone <- err
	}()
	await(t, func() bool { return firstSession.Stats().Pending == 1 })

	second := dialTestAgent(t, server.URL, hubTestToken)
	sendTestHello(t, second, "same-agent", stream.ProtocolVersion)
	// The second hello is processed asynchronously: until the hub attaches
	// it, the registry still returns the first session. Wait for the swap
	// itself instead of for "any session", which races with the attach.
	var secondSession *stream.Session
	await(t, func() bool {
		current, ok := registry.Session("same-agent")
		if !ok || current == nil || current == firstSession {
			return false
		}
		secondSession = current
		return true
	})
	select {
	case remoteErr := <-firstAgentRun:
		var closeErr *stream.CloseError
		if !errors.As(remoteErr, &closeErr) || closeErr.Code != stream.CloseSuperseded {
			t.Fatalf("superseded peer session ended with %T %v", remoteErr, remoteErr)
		}
	case <-time.After(time.Second):
		t.Fatal("superseded peer session did not receive close 4001")
	}
	select {
	case err := <-callDone:
		if !hasAgentCode(err, "agent-offline", http.StatusServiceUnavailable) {
			t.Fatalf("in-flight call after supersede = %v, want agent-offline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight call did not fail promptly after supersede")
	}
	if current, ok := registry.Session("same-agent"); !ok || current != secondSession {
		t.Fatalf("late first-connection detach cleared replacement: (%p, %v), want %p", current, ok, secondSession)
	}
}

func TestAgentHubKickAndShutdownCloseCodes(t *testing.T) {
	t.Run("remove", func(t *testing.T) {
		registry := NewRegistry()
		hub, server := newTestAgentHub(t, registry, NewEnrollmentManager(), HubOptions{})
		conn := dialTestAgent(t, server.URL, hubTestToken)
		sendTestHello(t, conn, "removed-agent", stream.ProtocolVersion)
		await(t, func() bool {
			_, ok := registry.Session("removed-agent")
			return ok
		})
		closeResult := awaitRemoteClose(conn)
		dispatcher := NewDispatcher(registry, "")
		dispatcher.Hub = hub
		if !dispatcher.RemoveAgent("removed-agent") {
			t.Fatal("RemoveAgent() returned false")
		}
		expectRemoteClose(t, closeResult, stream.CloseRemoved)
		if _, ok := registry.Get("removed-agent"); ok {
			t.Fatal("RemoveAgent left the agent in the registry")
		}
	})

	t.Run("hub restart", func(t *testing.T) {
		registry := NewRegistry()
		hub, server := newTestAgentHub(t, registry, NewEnrollmentManager(), HubOptions{})
		conn := dialTestAgent(t, server.URL, hubTestToken)
		sendTestHello(t, conn, "restarting-agent", stream.ProtocolVersion)
		await(t, func() bool {
			_, ok := registry.Session("restarting-agent")
			return ok
		})
		closeResult := awaitRemoteClose(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := hub.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown(): %v", err)
		}
		expectRemoteClose(t, closeResult, stream.CloseGoingAway)
	})
}

func waitRegistrySession(t *testing.T, registry *Registry, agentID string) *stream.Session {
	t.Helper()
	var result *stream.Session
	await(t, func() bool {
		result, _ = registry.Session(agentID)
		return result != nil
	})
	return result
}

func TestStreamThroughWebServer(t *testing.T) {
	registry := NewRegistry()
	enrollment := NewEnrollmentManager()
	auth := &Authenticator{Token: hubTestToken, Enrollment: enrollment}
	hub := NewAgentHub(registry, auth, enrollment, HubOptions{Session: stream.Options{PingInterval: 30 * time.Millisecond, PingTimeout: time.Second}})
	server, err := web.NewServer(web.ServeOptions{
		Addr: "127.0.0.1:0", HomerHome: t.TempDir(), Token: hubTestToken,
		AgentEndpoint: hub, AgentEndpointAuthorized: auth.Authorized,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewUnstartedServer(server.Handler())
	httpServer.Config.WriteTimeout = 50 * time.Millisecond
	httpServer.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = hub.Shutdown(ctx)
		httpServer.Close()
	}()
	conn := dialTestAgent(t, httpServer.URL, hubTestToken)
	sendTestHello(t, conn, "web-server-agent", stream.ProtocolVersion)
	time.Sleep(150 * time.Millisecond) // more than the server WriteTimeout
	pong := make(chan *stream.Frame, 1)
	readDone := make(chan error, 1)
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			message, err := conn.Read(ctx)
			cancel()
			if err != nil {
				readDone <- err
				return
			}
			frame, err := stream.DecodeFrame(message, stream.DefaultMaxFrame)
			if err != nil {
				readDone <- err
				return
			}
			if frame.T == stream.TPing {
				encoded, _ := stream.EncodeFrame(&stream.Frame{T: stream.TPong, ID: frame.ID}, stream.DefaultMaxFrame)
				writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				writeErr := conn.Write(writeCtx, encoded)
				cancel()
				if writeErr != nil {
					readDone <- writeErr
					return
				}
			}
			if frame.T == stream.TPong && frame.ID == "after-write-timeout" {
				pong <- frame
				return
			}
		}
	}()
	sendTestFrame(t, conn, &stream.Frame{T: stream.TPing, ID: "after-write-timeout"})
	select {
	case frame := <-pong:
		if frame.T != stream.TPong || frame.ID != "after-write-timeout" {
			t.Fatalf("stream through web.Server after WriteTimeout = %+v", frame)
		}
	case err := <-readDone:
		t.Fatalf("stream through web.Server failed after WriteTimeout: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pong through web.Server")
	}
}

type errorCodeEnvelope struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

func hubErrorCode(body []byte) string {
	var envelope errorCodeEnvelope
	if json.Unmarshal(body, &envelope) != nil {
		return ""
	}
	return envelope.Error.Code
}
