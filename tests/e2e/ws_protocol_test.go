package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/wsconn"
)

const protocolTestToken = "ws-e2e-hub-token"

type e2eLogCapture struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *e2eLogCapture) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *e2eLogCapture) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type e2eChildProcess struct {
	cmd    *exec.Cmd
	output *e2eLogCapture
	done   chan struct{}
	err    error
	mu     sync.Mutex
}

func startE2EChild(t *testing.T, binary, fakeHome, homerHome, gitConfig string, args ...string) *e2eChildProcess {
	t.Helper()
	cmd := exec.Command(binary, args...)
	orphanGuard(cmd)
	cmd.Env = []string{
		"HOME=" + fakeHome,
		"HOMER_HOME=" + homerHome,
		"GIT_CONFIG_GLOBAL=" + gitConfig,
		"GIT_CONFIG_NOSYSTEM=1",
		"HOMER_E2E_LOG=1",
		"PATH=" + os.Getenv("PATH"),
	}
	capture := &e2eLogCapture{}
	cmd.Stdout, cmd.Stderr = capture, capture
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s %s: %v", binary, strings.Join(args, " "), err)
	}
	child := &e2eChildProcess{cmd: cmd, output: capture, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		child.mu.Lock()
		child.err = err
		child.mu.Unlock()
		close(child.done)
	}()
	t.Cleanup(func() { child.killAndWait() })
	return child
}

func (p *e2eChildProcess) waitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *e2eChildProcess) killAndWait() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(8 * time.Second):
		// Wait is already running in startE2EChild; this final bound ensures a
		// misbehaving child cannot keep the e2e process alive indefinitely.
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (p *e2eChildProcess) interruptAndWait(timeout time.Duration) {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGINT)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		p.killAndWait()
	}
}

func e2eIsRunning(p *e2eChildProcess) bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

type wsProcessWorld struct {
	root      string
	global    string
	fakeHome  string
	hubHome   string
	agentHome string
	binary    string
}

func newWSProcessWorld(t *testing.T) wsProcessWorld {
	t.Helper()
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer WS E2E\n\temail = ws-e2e@example.invalid\n")
	fakeHome := filepath.Join(root, "fake-home")
	hubHome := filepath.Join(root, "hub-home")
	agentHome := filepath.Join(root, "agent-home")
	for _, dir := range []string{fakeHome, hubHome, agentHome} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return wsProcessWorld{root: root, global: global, fakeHome: fakeHome, hubHome: hubHome, agentHome: agentHome, binary: binaryOf(t)}
}

var e2eServeURLPattern = regexp.MustCompile(`http://(127\.0\.0\.1:[0-9]+)`)

func startWSE2EHub(t *testing.T, world wsProcessWorld, address string) (*e2eChildProcess, string) {
	t.Helper()
	child := startE2EChild(t, world.binary, world.fakeHome, world.hubHome, world.global,
		"serve", "--addr", address, "--home", world.hubHome, "--token", protocolTestToken)
	deadline := time.Now().Add(15 * time.Second)
	var base string
	for time.Now().Before(deadline) {
		if match := e2eServeURLPattern.FindStringSubmatch(child.output.String()); len(match) == 2 {
			base = "http://" + match[1]
			break
		}
		if !e2eIsRunning(child) {
			t.Fatalf("hub exited before printing its bound address: %v\n%s", child.waitError(), child.output.String())
		}
		time.Sleep(40 * time.Millisecond)
	}
	if base == "" {
		t.Fatalf("hub did not print its bound address within 15s:\n%s", child.output.String())
	}
	waitE2EHTTP(t, http.MethodGet, base+"/api/health", nil, http.StatusOK, 10*time.Second)
	return child, base
}

func waitE2EHTTP(t *testing.T, method, target string, headers http.Header, want int, timeout time.Duration) []byte {
	t.Helper()
	client := &http.Client{Timeout: 4 * time.Second}
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		request, err := http.NewRequest(method, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		for name, values := range headers {
			for _, value := range values {
				request.Header.Add(name, value)
			}
		}
		response, err := client.Do(request)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<20))
			_ = response.Body.Close()
			if readErr != nil {
				last = readErr.Error()
			} else if response.StatusCode == want {
				return body
			} else {
				last = fmt.Sprintf("HTTP %d: %s", response.StatusCode, body)
			}
		} else {
			last = err.Error()
		}
		time.Sleep(60 * time.Millisecond)
	}
	t.Fatalf("%s %s did not reach HTTP %d within %s: %s", method, target, want, timeout, last)
	return nil
}

func wsE2ERequest(t *testing.T, method, target string, bearer, cookie string, body []byte) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	if strings.Contains(target, "/dl/") {
		request.Header.Set("Range", "bytes=0-0")
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 8 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		t.Fatalf("read %s %s: %v", method, target, err)
	}
	return response.StatusCode, response.Header.Clone(), result
}

func wsE2EPostJSON(t *testing.T, base, path, bearer string, value any) (int, http.Header, []byte) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return wsE2ERequest(t, http.MethodPost, base+path, bearer, "", data)
}

func wsE2EJoinCode(t *testing.T, base string) string {
	t.Helper()
	status, _, body := wsE2ERequest(t, http.MethodGet, base+"/api/auth/join", protocolTestToken, "", nil)
	if status != http.StatusOK {
		t.Fatalf("mint join code = %d: %s", status, body)
	}
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &response); err != nil || !strings.HasPrefix(response.Code, "hr_") {
		t.Fatalf("join response has invalid enrollment code: %s (err=%v)", body, err)
	}
	return response.Code
}

func wsE2ERegistered(t *testing.T, base, agentID string) bool {
	t.Helper()
	status, _, body := wsE2ERequest(t, http.MethodGet, base+"/api/agents", protocolTestToken, "", nil)
	if status != http.StatusOK {
		return false
	}
	var response struct {
		Agents []struct {
			AgentID string `json:"agentId"`
		} `json:"agents"`
	}
	if json.Unmarshal(body, &response) != nil {
		return false
	}
	for _, agent := range response.Agents {
		if agent.AgentID == agentID {
			return true
		}
	}
	return false
}

func waitWSE2E(t *testing.T, description string, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(70 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, description)
}

func e2eHello(t *testing.T, base, agentID, bearer string, keep bool) (stream.Conn, hub.WelcomeResult, int, error) {
	return e2eHelloWithCookie(t, base, agentID, bearer, "", keep)
}

func e2eHelloWithCookie(t *testing.T, base, agentID, bearer, cookie string, keep bool) (stream.Conn, hub.WelcomeResult, int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := strings.TrimRight(base, "/") + "/agent/v1/stream"
	header := make(http.Header)
	if bearer != "" {
		header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		header.Set("Cookie", cookie)
	}
	conn, response, err := wsconn.Dial(ctx, endpoint, wsconn.DialOptions{Header: header})
	if err != nil {
		status := 0
		var dialErr *wsconn.DialError
		if errors.As(err, &dialErr) {
			status = dialErr.Status
		} else if response != nil {
			status = response.StatusCode
		}
		return nil, hub.WelcomeResult{}, status, err
	}
	params, err := json.Marshal(hub.HelloParams{
		Proto: stream.ProtocolVersion, AgentID: agentID, Hostname: "gate-e2e", Version: "e2e",
		Caps: []string{"status", "diff", "push", "pull", "collect.inspect"},
		// The platform-less /dl/homer router reads host.os/arch from the
		// registry; the gate matrix's per-agent-secret download expects a
		// same-platform 206, so the hello must report a platform.
		Host: &hub.HostSnapshot{OS: runtime.GOOS, Arch: runtime.GOARCH},
	})
	if err != nil {
		_ = conn.CloseNow()
		return nil, hub.WelcomeResult{}, http.StatusSwitchingProtocols, err
	}
	frame, err := stream.EncodeFrame(&stream.Frame{T: stream.TReq, ID: "hello-e2e", M: hub.MethodHello, P: params}, stream.DefaultMaxFrame)
	if err != nil {
		_ = conn.CloseNow()
		return nil, hub.WelcomeResult{}, http.StatusSwitchingProtocols, err
	}
	if err := conn.Write(ctx, frame); err != nil {
		_ = conn.CloseNow()
		return nil, hub.WelcomeResult{}, http.StatusSwitchingProtocols, err
	}
	message, err := conn.Read(ctx)
	if err != nil {
		_ = conn.CloseNow()
		return nil, hub.WelcomeResult{}, http.StatusSwitchingProtocols, err
	}
	result, err := stream.DecodeFrame(message, stream.DefaultMaxFrame)
	if err != nil {
		_ = conn.CloseNow()
		return nil, hub.WelcomeResult{}, http.StatusSwitchingProtocols, err
	}
	if result.T != stream.TRes || result.ID != "hello-e2e" || !result.OK {
		_ = conn.CloseNow()
		if result.E != nil {
			return nil, hub.WelcomeResult{}, http.StatusSwitchingProtocols, result.E
		}
		return nil, hub.WelcomeResult{}, http.StatusSwitchingProtocols, fmt.Errorf("unexpected hello response: %+v", result)
	}
	var welcome hub.WelcomeResult
	if err := json.Unmarshal(result.P, &welcome); err != nil {
		_ = conn.CloseNow()
		return nil, hub.WelcomeResult{}, http.StatusSwitchingProtocols, err
	}
	if !keep {
		_ = conn.Close(stream.CloseNormal, "gate matrix check")
		return nil, welcome, http.StatusSwitchingProtocols, nil
	}
	return conn, welcome, http.StatusSwitchingProtocols, nil
}

// TestWSReconnectAfterHubRestart proves that a real agent process reconnects
// after a real hub process is restarted. The hub's persisted enrollment secret
// and the agent's agent.json are both reused; a post-reconnect task proves that
// the replacement Registry has a working Stream session.
func TestWSReconnectAfterHubRestart(t *testing.T) {
	world := newWSProcessWorld(t)
	hubProc, base := startWSE2EHub(t, world, "127.0.0.1:0")
	port, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	address := port.Host
	enrollCode := wsE2EJoinCode(t, base)
	agentProc := startE2EChild(t, world.binary, world.fakeHome, world.agentHome, world.global,
		"agent", "--hub", base, "--token", enrollCode, "--id", "reconnect-agent", "--home", world.agentHome)
	waitWSE2E(t, "agent enrollment and initial Stream registration", 35*time.Second, func() bool {
		return wsE2ERegistered(t, base, "reconnect-agent")
	})
	configPath := filepath.Join(world.agentHome, "agent.json")
	firstConfig := mustReadFile(t, configPath)
	var before struct {
		AgentSecret string `json:"agentSecret"`
	}
	if err := json.Unmarshal(firstConfig, &before); err != nil || before.AgentSecret == "" {
		t.Fatalf("agent did not persist the WS hello secret: %s (err=%v)", firstConfig, err)
	}

	// Graceful shutdown sends 1001; the agent logs the disconnect and retries.
	hubProc.interruptAndWait(8 * time.Second)
	if e2eIsRunning(hubProc) {
		t.Fatal("old hub process is still alive after shutdown")
	}
	hubProc, base = startWSE2EHub(t, world, address)
	waitWSE2E(t, "agent reconnect to the restarted hub", 45*time.Second, func() bool {
		return wsE2ERegistered(t, base, "reconnect-agent")
	})
	var after struct {
		AgentSecret string `json:"agentSecret"`
	}
	if err := json.Unmarshal(mustReadFile(t, configPath), &after); err != nil || after.AgentSecret != before.AgentSecret {
		t.Fatalf("restart changed persisted agent secret: before=%q after=%q err=%v", before.AgentSecret, after.AgentSecret, err)
	}
	status, _, body := wsE2ERequest(t, http.MethodPost, base+"/api/agents/reconnect-agent/status", protocolTestToken, "", nil)
	if status != http.StatusOK {
		t.Fatalf("status task after reconnect = %d: %s\nhub:\n%s\nagent:\n%s", status, body, hubProc.output.String(), agentProc.output.String())
	}
	var report map[string]any
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("status task response is not JSON: %s (%v)", body, err)
	}
	if _, ok := report["report"]; !ok {
		t.Fatalf("status task response lacks report after reconnect: %s", body)
	}
}

// TestRevokeKicksLiveAgent covers both halves of revocation: a connected agent
// is kicked immediately, and a subsequent WebSocket dial with its persisted
// secret receives a visible HTTP 401 retry log rather than silently retrying.
func TestRevokeKicksLiveAgent(t *testing.T) {
	world := newWSProcessWorld(t)
	hubProc, base := startWSE2EHub(t, world, "127.0.0.1:0")
	code := wsE2EJoinCode(t, base)
	agentProc := startE2EChild(t, world.binary, world.fakeHome, world.agentHome, world.global,
		"agent", "--hub", base, "--token", code, "--id", "revoked-agent", "--home", world.agentHome)
	waitWSE2E(t, "live enrolled Stream session", 35*time.Second, func() bool {
		return wsE2ERegistered(t, base, "revoked-agent")
	})
	if _, err := os.Stat(filepath.Join(world.agentHome, "agent.json")); err != nil {
		t.Fatalf("enrollment secret not persisted before revocation: %v", err)
	}

	status, _, body := wsE2EPostJSON(t, base, "/api/agents/revoke", protocolTestToken, map[string]string{"agentId": "revoked-agent"})
	if status != http.StatusOK {
		t.Fatalf("console revoke = %d: %s", status, body)
	}
	waitWSE2E(t, "agent log confirming its revoked live session", 15*time.Second, func() bool {
		output := agentProc.output.String()
		return strings.Contains(output, "吊销") || strings.Contains(output, "revoked")
	})
	hubLog := hubProc.output.String()
	if !strings.Contains(hubLog, "kicking agent stream") && !strings.Contains(hubLog, "secret revoked") {
		t.Fatalf("hub did not log kicking the live stream after revoke:\n%s", hubLog)
	}

	// Stop the kicked process before starting a second copy with the exact
	// persisted (now revoked) secret. Its first retry gets HTTP 401, which is
	// logged with the prescribed actionable re-enrollment hint.
	agentProc.killAndWait()
	staleProc := startE2EChild(t, world.binary, world.fakeHome, world.agentHome, world.global,
		"agent", "--hub", base, "--home", world.agentHome)
	waitWSE2E(t, "HTTP 401 / revoked-secret log for the stale agent credential", 15*time.Second, func() bool {
		output := staleProc.output.String()
		return strings.Contains(output, "401") && (strings.Contains(output, "接入码已用") || strings.Contains(output, "重新生成接入码"))
	})
	waitWSE2E(t, "revoked agent status task returning agent-offline", 8*time.Second, func() bool {
		status, _, _ := wsE2ERequest(t, http.MethodPost, base+"/api/agents/revoked-agent/status", protocolTestToken, "", nil)
		return status == http.StatusServiceUnavailable
	})
}

type gateCredential struct {
	name       string
	bearer     string
	cookie     string
	admitted   bool
	knownBroad bool
}

type gatePath struct {
	name string
	path string
	kind string
}

func expectedGateStatus(path gatePath, credential gateCredential) int {
	if path.kind == "stream" {
		if credential.admitted || credential.bearer == protocolTestToken {
			return http.StatusSwitchingProtocols
		}
		return http.StatusUnauthorized
	}
	switch path.kind {
	case "tombstone":
		return http.StatusGone
	case "unknown-agent":
		return http.StatusNotFound
	case "public":
		return http.StatusOK
	case "api", "snapshot":
		if credential.bearer == protocolTestToken || credential.cookie != "" || credential.admitted {
			return http.StatusOK
		}
		return http.StatusUnauthorized
	case "download":
		if credential.bearer == protocolTestToken || credential.cookie != "" || credential.admitted {
			return http.StatusPartialContent
		}
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

func TestGateMatrixE2E(t *testing.T) {
	world := newWSProcessWorld(t)
	_, base := startWSE2EHub(t, world, "127.0.0.1:0")

	// Create a real browser session, and publish a tiny snapshot so the data
	// endpoint itself returns 200 after passing its gate.
	setupStatus, setupHeaders, setupBody := wsE2EPostJSON(t, base, "/api/auth/setup", "", map[string]string{"password": "gate-matrix-admin-password"})
	if setupStatus != http.StatusOK {
		t.Fatalf("setup admin cookie = %d: %s", setupStatus, setupBody)
	}
	cookies := setupHeaders.Values("Set-Cookie")
	if len(cookies) == 0 {
		t.Fatalf("setup did not issue a browser cookie: %v", setupHeaders)
	}
	parsedCookie, err := http.ParseSetCookie(cookies[0])
	if err != nil {
		t.Fatalf("parse session cookie %q: %v", cookies[0], err)
	}
	cookie := parsedCookie.Name + "=" + parsedCookie.Value
	snapshotStatus, _, snapshotBody := wsE2EPostJSON(t, base, "/api/snapshot", protocolTestToken, map[string]any{
		"homerJson": "{}", "store": map[string]map[string]string{"pi": {"settings/settings.json": "gate matrix fixture\n"}},
	})
	if snapshotStatus != http.StatusOK {
		t.Fatalf("seed snapshot = %d: %s", snapshotStatus, snapshotBody)
	}

	// Enroll one real agent credential for the per-agent stream and data-plane
	// gates. The dedicated revocation test separately keeps a real process live
	// while asserting the 4401 kick and its retry log.
	secretCode := wsE2EJoinCode(t, base)
	_, secretWelcome, status, err := e2eHello(t, base, "gate-secret-agent", secretCode, false)
	if err != nil || status != http.StatusSwitchingProtocols || secretWelcome.AgentSecret == "" {
		t.Fatalf("enroll secret used for matrix setup: status=%d welcome=%+v err=%v", status, secretWelcome, err)
	}
	unburnedCode := wsE2EJoinCode(t, base)
	streamCode := wsE2EJoinCode(t, base)
	if _, _, status, err := e2eHello(t, base, "gate-stream-enroll-agent", streamCode, false); err != nil || status != http.StatusSwitchingProtocols {
		t.Fatalf("unburned enrollment code did not complete Stream hello: status=%d err=%v", status, err)
	}
	burnedCode := streamCode

	credentials := []gateCredential{
		{name: "无"},
		{name: "cookie", cookie: cookie},
		{name: "hub-token", bearer: protocolTestToken},
		{name: "enroll-code-unburned", bearer: unburnedCode, admitted: true, knownBroad: true},
		{name: "enroll-code-burned", bearer: burnedCode},
		{name: "per-agent-secret", bearer: secretWelcome.AgentSecret, admitted: true, knownBroad: true},
	}
	paths := []gatePath{
		{name: "stream", path: "/agent/v1/stream", kind: "stream"},
		{name: "legacy-enroll", path: "/agent/v1/enroll", kind: "tombstone"},
		{name: "legacy-register", path: "/agent/v1/register", kind: "tombstone"},
		{name: "legacy-poll", path: "/agent/v1/poll", kind: "tombstone"},
		{name: "legacy-report", path: "/agent/v1/report", kind: "tombstone"},
		{name: "other-agent", path: "/agent/v1/not-a-route", kind: "unknown-agent"},
		{name: "api-console", path: "/api/console", kind: "api"},
		{name: "api-snapshot", path: "/api/snapshot", kind: "snapshot"},
		{name: "download", path: "/dl/homer", kind: "download"},
		{name: "install-script", path: "/install.sh", kind: "public"},
	}

	for _, path := range paths {
		if path.kind == "stream" {
			for _, credential := range credentials {
				label := path.name + "/" + credential.name
				if credential.knownBroad {
					t.Logf("KNOWN-BROAD credential: %s is expected to pass the /api gate", credential.name)
				}
				switch credential.name {
				case "per-agent-secret":
					if _, _, status, err := e2eHello(t, base, "gate-secret-agent", credential.bearer, false); err != nil || status != http.StatusSwitchingProtocols {
						t.Errorf("%s: matching per-agent secret hello status=%d err=%v", label, status, err)
					}
				case "enroll-code-burned":
					_, _, status, err := e2eHello(t, base, "gate-burned-agent", credential.bearer, false)
					if status != http.StatusUnauthorized || err == nil {
						t.Errorf("%s: burned enrollment Stream status=%d err=%v, want HTTP 401", label, status, err)
					}
				case "无":
					_, _, status, err := e2eHello(t, base, "gate-no-credential", "", false)
					if status != http.StatusUnauthorized || err == nil {
						t.Errorf("%s: Stream status=%d err=%v, want HTTP 401", label, status, err)
					}
				case "cookie":
					_, _, status, err := e2eHelloWithCookie(t, base, "gate-cookie-agent", "", credential.cookie, false)
					if status != http.StatusUnauthorized || err == nil {
						t.Errorf("%s: Stream status=%d err=%v, want HTTP 401", label, status, err)
					}
				default:
					agentID := "gate-token-agent"
					bearer := credential.bearer
					if credential.name == "enroll-code-unburned" {
						// Preserve the matrix credential for the KNOWN-BROAD HTTP gate
						// below; enroll on a separate one-shot code here.
						agentID = "gate-stream-code-agent"
						bearer = wsE2EJoinCode(t, base)
					}
					if _, _, status, err := e2eHello(t, base, agentID, bearer, false); err != nil || status != http.StatusSwitchingProtocols {
						t.Errorf("%s: Stream hello status=%d err=%v, want a successful WebSocket hello", label, status, err)
					}
				}
				continue
			}
			continue
		}
		for _, credential := range credentials {
			expected := expectedGateStatus(path, credential)
			status, _, body := wsE2ERequest(t, http.MethodGet, base+path.path, credential.bearer, credential.cookie, nil)
			if status != expected {
				t.Errorf("%s/%s = HTTP %d, want %d (body=%s)", path.name, credential.name, status, expected, body)
			}
			if credential.knownBroad && (path.kind == "api" || path.kind == "snapshot") {
				t.Logf("KNOWN-BROAD: %s currently passes %s with HTTP %d", credential.name, path.path, status)
			}
		}
	}

	if status, _, body := wsE2EPostJSON(t, base, "/api/agents/revoke", protocolTestToken, map[string]string{"agentId": "gate-secret-agent"}); status != http.StatusOK {
		t.Fatalf("revoke matrix secret = %d: %s", status, body)
	}
	revoked := gateCredential{name: "吊销的-secret", bearer: secretWelcome.AgentSecret}
	for _, path := range paths {
		if path.kind == "stream" {
			_, _, status, err := e2eHello(t, base, "gate-secret-agent", revoked.bearer, false)
			if status != http.StatusUnauthorized || err == nil {
				t.Errorf("stream/revoked-secret = HTTP %d err=%v, want 401", status, err)
			}
			continue
		}
		expected := expectedGateStatus(path, revoked)
		status, _, body := wsE2ERequest(t, http.MethodGet, base+path.path, revoked.bearer, "", nil)
		if status != expected {
			t.Errorf("%s/revoked-secret = HTTP %d, want %d (body=%s)", path.name, status, expected, body)
		}
	}
}

func TestOldProtocolTombstone(t *testing.T) {
	world := newWSProcessWorld(t)
	_, base := startWSE2EHub(t, world, "127.0.0.1:0")
	status, headers, body := wsE2ERequest(t, http.MethodGet, base+"/agent/v1/poll", "", "", nil)
	if status != http.StatusGone {
		t.Fatalf("old poll endpoint = HTTP %d, want 410: %s", status, body)
	}
	if got := headers.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("tombstone Content-Type = %q, want JSON", got)
	}
	var response struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("old poll tombstone is not JSON: %s (%v)", body, err)
	}
	if response.Error.Code != "protocol-removed" || !strings.Contains(response.Error.Message, "升级") || !strings.Contains(response.Error.Message, "重新安装") {
		t.Fatalf("old protocol tombstone is not actionable: %+v", response.Error)
	}
}

func TestOldAgentFlagsRejected(t *testing.T) {
	world := newWSProcessWorld(t)
	for _, oldFlag := range []string{"--listen", "--connect"} {
		t.Run(oldFlag, func(t *testing.T) {
			isolatedHome := t.TempDir()
			fakeHome := filepath.Join(isolatedHome, "fake-home")
			homerHome := filepath.Join(isolatedHome, "homer-home")
			if err := os.MkdirAll(fakeHome, 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, world.binary, "agent", oldFlag, "127.0.0.1:7760", "--home", homerHome)
			command.Env = []string{
				"HOME=" + fakeHome,
				"HOMER_HOME=" + homerHome,
				"GIT_CONFIG_GLOBAL=" + world.global,
				"GIT_CONFIG_NOSYSTEM=1",
				"PATH=" + os.Getenv("PATH"),
			}
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("homer agent %s did not exit within 10s: %s", oldFlag, output)
			}
			if err == nil {
				t.Fatalf("homer agent %s exited successfully: %s", oldFlag, output)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
				t.Fatalf("homer agent %s error = %v; output=%s", oldFlag, err, output)
			}
			// The CLI answers a removed flag with a migration hint rather than a
			// bare "unknown option": it names the flag, says it was removed, and
			// points at --hub so a systemd restart loop is self-explanatory.
			text := string(output)
			if !strings.Contains(text, oldFlag) || !strings.Contains(text, "已移除") || !strings.Contains(text, "--hub") {
				t.Fatalf("homer agent %s did not clearly reject the removed flag with a --hub migration hint: %s", oldFlag, output)
			}
		})
	}
}
