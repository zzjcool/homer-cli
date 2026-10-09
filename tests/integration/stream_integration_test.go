package integration

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
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/zzjcool/homer-cli/internal/agentd"
	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
	"github.com/zzjcool/homer-cli/internal/stream/wsconn"
	"github.com/zzjcool/homer-cli/internal/web"
)

func newHelperWorld(t *testing.T, pingInterval, pingTimeout, writeTimeout time.Duration) *hubWorld {
	t.Helper()
	root, global := testRoot(t)
	home := filepath.Join(root, "hub-home")
	addr := reserveAddr(t)
	token := "integration-" + sanitizeName(t.Name())
	process := startHelperHub(t, root, global, home, addr, token, pingInterval, pingTimeout, writeTimeout)
	world := &hubWorld{root: root, global: global, home: home, addr: addr, url: loopbackURL(addr), token: token, process: process}
	waitFor(t, 10*time.Second, "test web server ready", func() bool {
		status, _, err := httpRequest(world.url, token, http.MethodGet, "/api/health", nil, 500*time.Millisecond)
		return err == nil && status == http.StatusOK
	})
	return world
}

func initAgentWorkspace(t *testing.T, global, home string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create agent workspace: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, buildHomer(t), "init", "--json", "--home", home)
	command.Dir = repositoryRoot()
	command.Env = processEnv(map[string]string{
		"HOME": home, "HOMER_HOME": home,
		"GIT_CONFIG_GLOBAL": global, "GIT_CONFIG_NOSYSTEM": "1",
	})
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("homer init --json --home %s: %v\n%s", home, err, output)
	}
}

func waitForLog(t *testing.T, process *childProcess, text string, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, fmt.Sprintf("log line %q from pid %d", text, process.pid()), func() bool {
		return strings.Contains(process.output(), text)
	})
}

func startFaultRoutedAgent(t *testing.T, world *hubWorld, agentID, token string, executor fakeExecutorConfig, taskTimeout, pingInterval, pingTimeout time.Duration) (*childProcess, *routeProxy, *streamtest.TCPProxy) {
	t.Helper()
	fault, err := streamtest.NewTCPProxy(world.addr)
	if err != nil {
		t.Fatalf("start TCP fault proxy: %v", err)
	}
	t.Cleanup(func() { closeTCPProxy(t, fault) })
	route, err := newRouteProxy(fault.Addr())
	if err != nil {
		t.Fatalf("start reconnection route proxy: %v", err)
	}
	t.Cleanup(func() {
		if err := route.close(); err != nil {
			t.Errorf("close reconnection route proxy: %v", err)
		}
	})
	home := filepath.Join(world.root, "agent-"+sanitizeName(agentID))
	process := startHelperAgent(t, world.root, world.global, home, loopbackURL(route.Addr()), agentID, token, executor, taskTimeout, pingInterval, pingTimeout, 0)
	return process, route, fault
}

func apiBody(t *testing.T, response httpResult, wantStatus int) []byte {
	t.Helper()
	if response.err != nil {
		t.Fatalf("HTTP request failed: %v", response.err)
	}
	if response.status != wantStatus {
		t.Fatalf("HTTP status = %d, want %d: %s", response.status, wantStatus, truncate(string(response.body), 1024))
	}
	return response.body
}

func decodeTaskReport(t *testing.T, body []byte) commands.StatusReport {
	t.Helper()
	var response struct {
		OK     bool                  `json:"ok"`
		Report commands.StatusReport `json:"report"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode task response: %v (%s)", err, truncate(string(body), 1024))
	}
	if !response.OK {
		t.Fatalf("status response is not ok: %s", truncate(string(body), 1024))
	}
	return response.Report
}

func requestRaw(world *hubWorld, method, path string, body []byte, timeout time.Duration) httpResult {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	status, response, err := httpRequest(world.url, world.token, method, path, reader, timeout)
	return httpResult{status: status, body: response, err: err}
}

type asyncRequest struct {
	cancel context.CancelFunc
	done   <-chan httpResult
}

func startAsyncRequest(world *hubWorld, method, path string, body []byte, timeout time.Duration) (*asyncRequest, error) {
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(world.url, "/")+path, reader)
	if err != nil {
		cancel()
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+world.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	done := make(chan httpResult, 1)
	go func() {
		response, requestErr := (&http.Client{}).Do(request)
		if requestErr != nil {
			done <- httpResult{err: requestErr}
			return
		}
		defer response.Body.Close()
		data, readErr := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
		if readErr == nil && len(data) > 16<<20 {
			readErr = errors.New("HTTP response exceeded integration test read limit")
		}
		done <- httpResult{status: response.StatusCode, body: data, err: readErr}
	}()
	return &asyncRequest{cancel: cancel, done: done}, nil
}

func waitAsyncRequest(t *testing.T, request *asyncRequest, timeout time.Duration) httpResult {
	t.Helper()
	select {
	case response := <-request.done:
		return response
	case <-time.After(timeout):
		t.Fatalf("HTTP request did not finish in %s", timeout)
		return httpResult{}
	}
}

func agentSecret(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(home, "agent.json")
	var config agentd.AgentConfig
	waitFor(t, 5*time.Second, "persisted agent secret", func() bool {
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &config) != nil {
			return false
		}
		return config.AgentSecret != ""
	})
	return config.AgentSecret
}

func waitForCodeBurned(t *testing.T, world *hubWorld, code string) {
	t.Helper()
	waitFor(t, 10*time.Second, "enrollment code burn", func() bool {
		status, body, err := httpRequest(world.url, world.token, http.MethodGet,
			"/api/auth/join/status?code="+url.QueryEscape(code), nil, time.Second)
		if err != nil || status != http.StatusOK {
			return false
		}
		var response struct {
			Active bool `json:"active"`
		}
		return json.Unmarshal(body, &response) == nil && !response.Active
	})
}

func resetEventLog(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("reset event log: %v", err)
	}
}

type executorActivity struct {
	Method      string `json:"method"`
	Event       string `json:"event"`
	ReadActive  int    `json:"readActive"`
	WriteActive int    `json:"writeActive"`
	TotalActive int    `json:"totalActive"`
}

func activitiesFromLog(t *testing.T, path string) []executorActivity {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read executor event log: %v", err)
	}
	var events []executorActivity
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event executorActivity
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("decode executor event %q: %v", truncate(string(line), 512), err)
		}
		events = append(events, event)
	}
	return events
}

func allContain(values []string, substring string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if !strings.Contains(value, substring) {
			return false
		}
	}
	return true
}

func maximumActivity(events []executorActivity, read bool) int {
	maximum := 0
	for _, event := range events {
		active := event.WriteActive
		if read {
			active = event.ReadActive
		}
		if active > maximum {
			maximum = active
		}
	}
	return maximum
}

func latestTotalActivity(events []executorActivity) int {
	if len(events) == 0 {
		return 0
	}
	return events[len(events)-1].TotalActive
}

func TestMultiAgentOnline(t *testing.T) {
	world := newHomerWorld(t)
	ids := []string{"integration-a", "integration-b", "integration-c"}
	processes := make([]*childProcess, 0, len(ids))
	for _, id := range ids {
		home := filepath.Join(world.root, id)
		initAgentWorkspace(t, world.global, home)
		processes = append(processes, startHomerAgent(t, world.root, world.global, buildHomer(t), home, world.url, id, world.token))
	}
	waitAgents(t, world, ids, true, 20*time.Second)
	for _, id := range ids {
		response := apiJSON(t, world, http.MethodPost, "/api/agents/"+id+"/status", nil, 20*time.Second)
		report := decodeTaskReport(t, apiBody(t, response, http.StatusOK))
		if len(report.Adapters) == 0 {
			t.Errorf("agent %s status contains no adapters: %+v", id, report)
		}
	}
	if len(processes) != 3 {
		t.Fatalf("started %d agent processes, want 3", len(processes))
	}
}

func TestReconnectAfterCut(t *testing.T) {
	world := newHomerWorld(t)
	process, route, fault := startFaultRoutedAgent(t, world, "cut-agent", world.token, fakeExecutorConfig{}, 0, 0, 0)
	waitAgent(t, world, "cut-agent", true, 10*time.Second)
	reconnectStarted := time.Now()
	fault.Cut()
	route.setTarget(world.addr)
	waitFor(t, 5*time.Second, "logged reconnect attempt", func() bool {
		text := process.output()
		return strings.Contains(text, "退避") || strings.Contains(text, "重连") || strings.Contains(text, "网络失败")
	})
	waitAgent(t, world, "cut-agent", true, 8*time.Second)
	if elapsed := time.Since(reconnectStarted); elapsed > 1500*time.Millisecond {
		t.Fatalf("agent reconnect took %s; scaled backoff should be ≤1.5s", elapsed)
	}
	logText := process.output()
	if !strings.Contains(logText, "退避") && !strings.Contains(logText, "重连") && !strings.Contains(logText, "网络失败") {
		t.Fatalf("agent did not log the reconnect attempt:\n%s", logText)
	}
}

func TestHubRestartRecovery(t *testing.T) {
	world := newHomerWorld(t)
	home := filepath.Join(world.root, "restart-agent")
	initAgentWorkspace(t, world.global, home)
	agent := startHomerAgent(t, world.root, world.global, buildHomer(t), home, world.url, "restart-agent", world.token)
	waitAgent(t, world, "restart-agent", true, 15*time.Second)
	firstInstanceLog := world.process.output()
	stopGracefully(t, world.process, 8*time.Second)
	waitForLog(t, agent, "1001", 5*time.Second)
	world.process = startHomerHub(t, world.root, world.global, world.home, world.addr, world.token)
	waitFor(t, 10*time.Second, "restarted homer hub ready", func() bool {
		status, _, err := httpRequest(world.url, world.token, http.MethodGet, "/api/health", nil, 500*time.Millisecond)
		return err == nil && status == http.StatusOK
	})
	waitAgent(t, world, "restart-agent", true, 10*time.Second)
	response := apiJSON(t, world, http.MethodPost, "/api/agents/restart-agent/status", nil, 15*time.Second)
	_ = decodeTaskReport(t, apiBody(t, response, http.StatusOK))
	if strings.TrimSpace(firstInstanceLog) == "" || !strings.Contains(world.process.output(), "agent stream connected") {
		t.Fatal("hub restart did not rebuild a registry entry for the reconnecting agent")
	}
}

func TestAgentRestart(t *testing.T) {
	world := newHomerWorld(t)
	home := filepath.Join(world.root, "agent-home")
	initAgentWorkspace(t, world.global, home)
	binary := buildHomer(t)
	first := startHomerAgent(t, world.root, world.global, binary, home, world.url, "agent-restart", world.token)
	waitAgent(t, world, "agent-restart", true, 15*time.Second)
	stopGracefully(t, first, 5*time.Second)
	waitAgent(t, world, "agent-restart", false, 5*time.Second)
	second := startHomerAgent(t, world.root, world.global, binary, home, world.url, "agent-restart", world.token)
	waitAgent(t, world, "agent-restart", true, 10*time.Second)
	response := apiJSON(t, world, http.MethodPost, "/api/agents/agent-restart/status", nil, 15*time.Second)
	_ = decodeTaskReport(t, apiBody(t, response, http.StatusOK))
	if first.pid() == second.pid() {
		t.Fatal("agent restart reused the stopped process")
	}
}

func TestInflightDisconnectFailsFast(t *testing.T) {
	world := newHomerWorld(t)
	eventLog := filepath.Join(world.root, "inflight-events.jsonl")
	agent, route, fault := startFaultRoutedAgent(t, world, "inflight-agent", world.token,
		fakeExecutorConfig{BlockDiff: true, EventLog: eventLog}, 0, 0, 0)
	waitAgent(t, world, "inflight-agent", true, 10*time.Second)
	request, err := startAsyncRequest(world, http.MethodPost, "/api/agents/inflight-agent/diff?adapter=pi", []byte("{}"), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitForFileEvent(t, eventLog, "diff", "start", 5*time.Second)
	started := time.Now()
	fault.Cut()
	route.setTarget(world.addr)
	response := waitAsyncRequest(t, request, 3*time.Second)
	if response.err != nil {
		t.Fatalf("in-flight call did not return an agent error: %v", response.err)
	}
	if response.status != http.StatusServiceUnavailable || errorCode(t, response.body) != "agent-offline" {
		t.Fatalf("in-flight call after disconnect = %d %s, want immediate agent-offline", response.status, truncate(string(response.body), 512))
	}
	if elapsed := time.Since(started); elapsed >= 3*time.Second || elapsed >= time.Minute {
		t.Fatalf("in-flight disconnect took %s; it must not wait for the 60s call budget", elapsed)
	}
	waitForFileEvent(t, eventLog, "diff", "canceled", 3*time.Second)
	_ = agent
}

func TestSameAgentIDSupersede(t *testing.T) {
	world := newInProcessWorld(t)
	eventLog := filepath.Join(world.world.root, "superseded-events.jsonl")
	first := startHelperAgent(t, world.world.root, world.world.global, filepath.Join(world.world.root, "first-agent-home"),
		world.world.url, "duplicate-id", world.world.token, fakeExecutorConfig{BlockDiff: true, EventLog: eventLog}, 0, 0, 0, 0)
	waitAgent(t, world.world, "duplicate-id", true, 10*time.Second)

	callDone := make(chan error, 1)
	go func() {
		_, err := world.dispatcher.AgentDiff(context.Background(), "duplicate-id", web.DiffParams{Adapter: "pi"})
		callDone <- err
	}()
	waitForFileEvent(t, eventLog, "diff", "start", 5*time.Second)

	oldConn := openRawAgentStream(t, world.world.url, world.world.token)
	helloRawAgent(t, oldConn, "duplicate-id")
	waitFor(t, 3*time.Second, "hub to attach the raw duplicate agent", func() bool {
		return strings.Count(world.agentHubLog(), "agent stream connected agent=\"duplicate-id\"") >= 2
	})
	select {
	case err := <-callDone:
		var agentErr *web.AgentError
		if !errors.As(err, &agentErr) || agentErr.Code != "agent-offline" {
			t.Fatalf("old in-flight call error = %v, want immediate agent-offline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("same-ID replacement did not fail the old in-flight call immediately")
	}
	waitForFileEvent(t, eventLog, "diff", "canceled", 3*time.Second)
	waitForLog(t, first, "agent 被顶替", 3*time.Second)

	closeResult := make(chan error, 1)
	go func() { closeResult <- readCloseCode(oldConn, stream.CloseSuperseded, 5*time.Second) }()
	second := startHelperAgent(t, world.world.root, world.world.global, filepath.Join(world.world.root, "replacement-home"),
		world.world.url, "duplicate-id", world.world.token, fakeExecutorConfig{}, 0, 0, 0, 0)
	waitFor(t, 5*time.Second, "hub to log the replacement connection", func() bool {
		return strings.Contains(world.agentHubLog(), "superseding prior connection agent=\"duplicate-id\"")
	})
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("old raw agent connection did not receive close 4001: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("old raw agent connection did not receive close 4001")
	}
	_ = second
}

func TestRevokedSecretKicked(t *testing.T) {
	world := newHomerWorld(t)
	home := filepath.Join(world.root, "revoked-agent")
	agentID := "revoked-agent"
	code := getJoinCode(t, world)
	agent := startHomerAgent(t, world.root, world.global, buildHomer(t), home, world.url, agentID, code)
	waitAgent(t, world, agentID, true, 15*time.Second)
	secret := agentSecret(t, home)
	body, _ := json.Marshal(map[string]string{"agentId": agentID})
	revoked := requestRaw(world, http.MethodPost, "/api/agents/revoke", body, 5*time.Second)
	apiBody(t, revoked, http.StatusOK)
	waitForLog(t, world.process, "code=4401", 5*time.Second)
	waitForLog(t, agent, "agentSecret 已吊销", 5*time.Second)
	waitAgent(t, world, agentID, false, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+secret)
	_, _, err := wsconn.Dial(ctx, agentStreamURL(world.url), wsconn.DialOptions{Header: header})
	var dialErr *wsconn.DialError
	if !errors.As(err, &dialErr) || dialErr.Status != http.StatusUnauthorized {
		t.Fatalf("reconnect with revoked secret error = %v, want HTTP 401", err)
	}
}

func TestBurnedEnrollCodeReconnect(t *testing.T) {
	world := newHomerWorld(t)
	agentID := "burned-agent"
	home := filepath.Join(world.root, agentID)
	code := getJoinCode(t, world)
	fault, err := streamtest.NewTCPProxy(world.addr)
	if err != nil {
		t.Fatal(err)
	}
	fault.Delay(1500 * time.Millisecond)
	t.Cleanup(func() { closeTCPProxy(t, fault) })
	route, err := newRouteProxy(fault.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := route.close(); err != nil {
			t.Errorf("close enrollment route proxy: %v", err)
		}
	})
	agent := startHomerAgent(t, world.root, world.global, buildHomer(t), home,
		loopbackURL(route.Addr()), agentID, code)
	// The hub burns the code as it processes hello; cut immediately after that
	// observable transition so the delayed welcome cannot reach the agent.
	waitForCodeBurned(t, world, code)
	fault.Cut()
	route.setTarget(world.addr)
	waitForLog(t, agent, "WebSocket 401", 8*time.Second)
	if !strings.Contains(agent.output(), "接入码已用") {
		t.Fatalf("agent did not clearly log that its one-time code was burned:\n%s", agent.output())
	}
	if config, ok := agentd.LoadAgentConfig(home); ok && config.AgentSecret != "" {
		t.Fatal("agent persisted an enrollment secret even though the welcome response was lost")
	}
	stopGracefully(t, agent, 5*time.Second)

	freshCode := getJoinCode(t, world)
	second := startHomerAgent(t, world.root, world.global, buildHomer(t), home, world.url, agentID, freshCode)
	waitAgent(t, world, agentID, true, 15*time.Second)
	secret := agentSecret(t, home)
	if secret == "" {
		t.Fatal("successful enrollment did not persist its per-agent secret")
	}
	stopGracefully(t, second, 5*time.Second)
	restarted := startHomerAgent(t, world.root, world.global, buildHomer(t), home, world.url, agentID, "")
	waitAgent(t, world, agentID, true, 10*time.Second)
	response := apiJSON(t, world, http.MethodPost, "/api/agents/"+agentID+"/status", nil, 10*time.Second)
	_ = decodeTaskReport(t, apiBody(t, response, http.StatusOK))
	_ = restarted
}

// TestEnrollBurnedAfterHelloLost records R8: Redeem burns a one-time code
// before welcome is written, so a dropped welcome requires a newly minted code.
func TestEnrollBurnedAfterHelloLost(t *testing.T) {
	world := newHomerWorld(t)
	code := getJoinCode(t, world)
	proxy, err := streamtest.NewTCPProxy(world.addr)
	if err != nil {
		t.Fatal(err)
	}
	proxy.Delay(time.Second)
	t.Cleanup(func() { closeTCPProxy(t, proxy) })
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+code)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	conn, _, err := wsconn.Dial(ctx, agentStreamURL(loopbackURL(proxy.Addr())), wsconn.DialOptions{Header: header})
	cancel()
	if err != nil {
		t.Fatalf("dial with enrollment code through delayed proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	payload, err := json.Marshal(hub.HelloParams{
		Proto: stream.ProtocolVersion, AgentID: "lost-hello-agent", Hostname: "lost-hello-agent",
		Version: "integration", Caps: []string{hub.MethodStatus},
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := stream.EncodeFrame(&stream.Frame{T: stream.TReq, ID: "hello-lost-1", M: hub.MethodHello, P: payload}, stream.DefaultMaxFrame)
	if err != nil {
		t.Fatal(err)
	}
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 4*time.Second)
	err = conn.Write(writeCtx, frame)
	writeCancel()
	if err != nil {
		t.Fatalf("write enrollment hello: %v", err)
	}
	waitForCodeBurned(t, world, code)
	proxy.Cut()
	if codeStillActive(t, world, code) {
		t.Fatal("enrollment code remained active after the hub accepted hello; R8 burn-on-hello behavior changed")
	}

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dialCancel()
	_, _, retryErr := wsconn.Dial(dialCtx, agentStreamURL(world.url), wsconn.DialOptions{Header: header})
	var dialErr *wsconn.DialError
	if !errors.As(retryErr, &dialErr) || dialErr.Status != http.StatusUnauthorized {
		t.Fatalf("reconnect after losing the hello welcome error = %v, want HTTP 401", retryErr)
	}
	if strings.Contains(world.process.output(), "agent secret was revoked") {
		t.Fatal("test unexpectedly reached a revoked-secret path rather than consumed-code path")
	}
}

func TestSlowConsumerIsolation(t *testing.T) {
	world := newInProcessWorld(t, stream.Options{WriteTimeout: 250 * time.Millisecond, SendQueue: 1})
	healthy := startHelperAgent(t, world.world.root, world.world.global, filepath.Join(world.world.root, "healthy-agent"),
		world.world.url, "healthy-agent", world.world.token, fakeExecutorConfig{}, 0, 0, 0, 0)
	waitAgent(t, world.world, "healthy-agent", true, 10*time.Second)

	// This real WebSocket peer completes hello and then deliberately never
	// reads. Multi-MiB outbound RPCs fill its TCP window; the bounded writer
	// must shed just this session while the separate healthy agent still works.
	silent := openRawAgentStream(t, world.world.url, world.world.token)
	helloRawAgent(t, silent, "silent-agent")
	const callers = 4
	pathPayload := strings.Repeat("x", 7<<20)
	results := make(chan error, callers)
	for index := 0; index < callers; index++ {
		go func() {
			_, err := world.dispatcher.AgentDiff(context.Background(), "silent-agent", web.DiffParams{Path: pathPayload})
			results <- err
		}()
	}
	waitFor(t, 5*time.Second, "slow agent outbound tasks pending", func() bool {
		session, ok := world.dispatcher.Registry.Session("silent-agent")
		return ok && session.Stats().Pending > 0
	})

	started := time.Now()
	status := apiJSON(t, world.world, http.MethodPost, "/api/agents/healthy-agent/status", nil, 3*time.Second)
	_ = decodeTaskReport(t, apiBody(t, status, http.StatusOK))
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("healthy agent was delayed by the non-reading peer: %s", elapsed)
	}

	failed := 0
	var failureTexts []string
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for completed := 0; completed < callers; completed++ {
		select {
		case err := <-results:
			if err != nil {
				failed++
				failureTexts = append(failureTexts, err.Error())
			}
		case <-deadline.C:
			t.Fatalf("only %d/%d slow-agent calls completed", completed, callers)
		}
	}
	if failed == 0 {
		t.Fatal("all slow-agent RPCs succeeded although the peer never read its stream")
	}
	isolationDeadline := time.Now().Add(3 * time.Second)
	isolated := false
	for time.Now().Before(isolationDeadline) {
		_, online := world.dispatcher.Registry.Session("silent-agent")
		if !online && failed == callers && allContain(failureTexts, "slow-consumer") {
			isolated = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !isolated {
		session, online := world.dispatcher.Registry.Session("silent-agent")
		t.Fatalf("slow-consumer session remains online=%t session=%+v failed=%d/%d errors=%q", online, session, failed, callers, failureTexts)
	}
	waitAgent(t, world.world, "healthy-agent", true, 2*time.Second)
	_ = healthy
}

func TestLargePayload(t *testing.T) {
	world := newHomerWorld(t)
	for _, size := range []int{2 << 20, 7 << 20} {
		id := fmt.Sprintf("payload-%d", size)
		home := filepath.Join(world.root, id)
		agent := startHelperAgent(t, world.root, world.global, home, world.url, id, world.token,
			fakeExecutorConfig{PayloadBytes: size}, 0, 0, 0, 0)
		waitAgent(t, world, id, true, 10*time.Second)
		started := time.Now()
		response := apiJSON(t, world, http.MethodPost, "/api/agents/"+id+"/status", nil, 90*time.Second)
		body := apiBody(t, response, http.StatusOK)
		if size == 7<<20 {
			t.Logf("7 MiB status round trip: %s", time.Since(started))
		}
		var payload struct {
			Report struct {
				Errors []string `json:"errors"`
			} `json:"report"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode %d-byte status response: %v", size, err)
		}
		if len(payload.Report.Errors) != 1 || len(payload.Report.Errors[0]) != size {
			t.Fatalf("payload size %d round-tripped as %d bytes", size, len(payload.Report.Errors[0]))
		}
		_ = agent
	}

	tooLargeID := "payload-over-limit"
	tooLarge := startHelperAgent(t, world.root, world.global, filepath.Join(world.root, tooLargeID), world.url,
		tooLargeID, world.token, fakeExecutorConfig{PayloadBytes: (8 << 20) + 1024}, 0, 0, 0, 0)
	waitAgent(t, world, tooLargeID, true, 10*time.Second)
	response := apiJSON(t, world, http.MethodPost, "/api/agents/"+tooLargeID+"/status", nil, 20*time.Second)
	if response.err != nil {
		t.Fatal(response.err)
	}
	if response.status != http.StatusBadGateway || errorCode(t, response.body) != "agent-unreachable" {
		t.Fatalf("payload above 8 MiB response = %d %s, want a clear frame-size error", response.status, truncate(string(response.body), 512))
	}
	if !strings.Contains(strings.ToLower(string(response.body)), "max frame") {
		t.Fatalf("oversize payload error is not explicit: %s", truncate(string(response.body), 512))
	}
	_ = tooLarge
}

func TestFiftyConcurrentTasks(t *testing.T) {
	world := newHomerWorld(t)
	const agents = 50
	const delay = 200 * time.Millisecond
	prefix := "concurrent"
	executor := fakeExecutorConfig{StatusDelayMS: int(delay / time.Millisecond)}
	agentsStarted := make([]*childProcess, 0, agents)
	ids := make([]string, 0, agents)
	for index := 0; index < agents; index++ {
		id := fmt.Sprintf("%s-%02d", prefix, index)
		ids = append(ids, id)
		agent := startHelperAgent(t, world.root, world.global, filepath.Join(world.root, "agents-home", id),
			world.url, id, world.token, executor, 5*time.Second, 0, 0, 0)
		agentsStarted = append(agentsStarted, agent)
	}
	waitAgents(t, world, ids, true, 30*time.Second)
	// Drain any initial status scans kicked off by hello before the serial
	// baseline. Otherwise this measurement would include startup traffic.
	for _, id := range ids {
		response := requestRaw(world, http.MethodPost, "/api/agents/"+id+"/status", nil, 10*time.Second)
		_ = apiBody(t, response, http.StatusOK)
	}

	var slowestSingle time.Duration
	for _, id := range ids {
		started := time.Now()
		response := requestRaw(world, http.MethodPost, "/api/agents/"+id+"/status", nil, 10*time.Second)
		_ = apiBody(t, response, http.StatusOK)
		if elapsed := time.Since(started); elapsed > slowestSingle {
			slowestSingle = elapsed
		}
	}

	started := time.Now()
	results := make(chan httpResult, agents)
	var workers sync.WaitGroup
	workers.Add(agents)
	for _, id := range ids {
		id := id
		go func() {
			defer workers.Done()
			results <- requestRaw(world, http.MethodPost, "/api/agents/"+id+"/status", nil, 10*time.Second)
		}()
	}
	workers.Wait()
	close(results)
	for response := range results {
		if response.err != nil || response.status != http.StatusOK {
			t.Errorf("concurrent task failed: status=%d err=%v body=%s", response.status, response.err, truncate(string(response.body), 512))
		}
	}
	elapsed := time.Since(started)
	if elapsed > slowestSingle*13/10 {
		t.Fatalf("50 concurrent tasks took %s; slowest single task took %s (limit %s)", elapsed, slowestSingle, slowestSingle*13/10)
	}
	t.Logf("50 concurrent status tasks: batch=%s slowest-single=%s", elapsed, slowestSingle)
	if len(agentsStarted) != agents {
		t.Fatalf("started %d real agent processes, want %d", len(agentsStarted), agents)
	}
}

func TestCancelTask(t *testing.T) {
	inProcess := newInProcessWorld(t)
	world := inProcess.world
	eventLog := filepath.Join(world.root, "cancel-events.jsonl")
	agent := startHelperAgent(t, world.root, world.global, filepath.Join(world.root, "cancel-agent"), world.url,
		"cancel-agent", world.token, fakeExecutorConfig{BlockDiff: true, EventLog: eventLog}, 0, 0, 0, 0)
	waitAgent(t, world, "cancel-agent", true, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := inProcess.dispatcher.AgentDiff(ctx, "cancel-agent", web.DiffParams{Adapter: "pi"})
		result <- err
	}()
	waitForFileEvent(t, eventLog, "diff", "start", 5*time.Second)
	cancelAt := time.Now()
	cancel()
	select {
	case err := <-result:
		var agentErr *web.AgentError
		if !errors.As(err, &agentErr) || agentErr.Code != "agent-timeout" {
			t.Fatalf("canceled dispatcher call error = %v, want agent-timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hub dispatcher did not return after the caller canceled the task")
	}
	if time.Since(cancelAt) > time.Second {
		t.Fatalf("caller cancellation took %s to return", time.Since(cancelAt))
	}
	deadline := time.Now().Add(2 * time.Second)
	for !hasFileEvent(eventLog, "diff", "canceled") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !hasFileEvent(eventLog, "diff", "canceled") {
		body, _ := os.ReadFile(eventLog)
		t.Fatalf("agent task context was not canceled after caller cancellation: events=%s\nagent log:\n%s", body, agent.output())
	}
	if strings.Contains(agent.output(), "panic:") {
		t.Fatalf("agent panicked while canceling a task:\n%s", agent.output())
	}
}

// TestCancelTaskHTTPContext checks the caller-facing cancellation path end to
// end: a console request that disconnects must cancel the task on the agent.
// Dispatcher-level cancellation is covered by TestCancelTask.
func TestCancelTaskHTTPContext(t *testing.T) {
	world := newHomerWorld(t)
	eventLog := filepath.Join(world.root, "cancel-http-events.jsonl")
	agent := startHelperAgent(t, world.root, world.global, filepath.Join(world.root, "cancel-http-agent"), world.url,
		"cancel-http-agent", world.token, fakeExecutorConfig{BlockDiff: true, EventLog: eventLog}, 0, 0, 0, 0)
	waitAgent(t, world, "cancel-http-agent", true, 10*time.Second)
	// The console sends this POST with no body. A request that carries a body
	// the handler never reads does not get its context canceled when the
	// client disconnects (net/http server behavior, verified separately), so
	// the probe must use the same bodiless shape as the real caller.
	request, err := startAsyncRequest(world, http.MethodPost, "/api/agents/cancel-http-agent/diff?adapter=pi", nil, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitForFileEvent(t, eventLog, "diff", "start", 5*time.Second)
	request.cancel()
	select {
	case response := <-request.done:
		if response.err == nil {
			t.Fatalf("canceled HTTP request unexpectedly returned status=%d body=%s", response.status, response.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP client cancellation did not return to the caller promptly")
	}
	deadline := time.Now().Add(time.Second)
	for !hasFileEvent(eventLog, "diff", "canceled") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !hasFileEvent(eventLog, "diff", "canceled") {
		events, _ := os.ReadFile(eventLog)
		t.Fatalf("HTTP caller returned context-canceled, but the agent executor did not receive ctx cancellation within 1s; events=%s", events)
	}
	_ = agent
}

func TestTaskTimeout(t *testing.T) {
	world := newHomerWorld(t)
	agent := startHelperAgent(t, world.root, world.global, filepath.Join(world.root, "timeout-agent"), world.url,
		"timeout-agent", world.token, fakeExecutorConfig{DiffDelayMS: 1200}, 250*time.Millisecond, 0, 0, 0)
	waitAgent(t, world, "timeout-agent", true, 10*time.Second)
	started := time.Now()
	response := apiJSON(t, world, http.MethodPost, "/api/agents/timeout-agent/diff?adapter=pi", map[string]any{}, 5*time.Second)
	if response.err != nil {
		t.Fatal(response.err)
	}
	if response.status != http.StatusGatewayTimeout || errorCode(t, response.body) != "agent-timeout" {
		t.Fatalf("task timeout response = %d %s, want agent-timeout", response.status, truncate(string(response.body), 512))
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("configured task timeout took %s", time.Since(started))
	}
	_ = agent
}

func TestUpgradeReexecReconnect(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reexecAgent uses syscall.Exec only on Linux")
	}
	world := newHomerWorld(t)
	home := filepath.Join(world.root, "upgrade-agent")
	initAgentWorkspace(t, world.global, home)
	agent := startHomerAgent(t, world.root, world.global, buildHomer(t), home, world.url, "upgrade-agent", world.token)
	waitAgent(t, world, "upgrade-agent", true, 15*time.Second)
	before := strings.Count(agent.output(), "homer agent: 已启动")
	upgrade := apiJSON(t, world, http.MethodPost, "/api/agents/upgrade-agent/upgrade", map[string]any{}, 4*time.Minute)
	if upgrade.err != nil || upgrade.status != http.StatusOK {
		t.Fatalf("agent upgrade response = %d err=%v body=%s", upgrade.status, upgrade.err, truncate(string(upgrade.body), 1024))
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(upgrade.body, &result); err != nil || !result.OK {
		t.Fatalf("agent upgrade result = %s err=%v", truncate(string(upgrade.body), 1024), err)
	}
	waitFor(t, 10*time.Second, "agent reexec and reconnect", func() bool {
		return strings.Count(agent.output(), "homer agent: 已启动") >= before+1
	})
	waitAgent(t, world, "upgrade-agent", true, 10*time.Second)
	status := apiJSON(t, world, http.MethodPost, "/api/agents/upgrade-agent/status", nil, 15*time.Second)
	_ = decodeTaskReport(t, apiBody(t, status, http.StatusOK))
}

func TestToolUpgradeLongTask(t *testing.T) {
	world := newHelperWorld(t, 20*time.Millisecond, 180*time.Millisecond, 0)
	agent := startHelperAgent(t, world.root, world.global, filepath.Join(world.root, "tool-agent"), world.url,
		"tool-agent", world.token, fakeExecutorConfig{}, 0, 20*time.Millisecond, 180*time.Millisecond, 3*time.Second)
	waitAgent(t, world, "tool-agent", true, 10*time.Second)
	started := time.Now()
	response := apiJSON(t, world, http.MethodPost, "/api/agents/tool-agent/tool-upgrade", map[string]string{"tool": "pi"}, 15*time.Second)
	if response.err != nil || response.status != http.StatusOK {
		t.Fatalf("3s tool upgrade response = %d err=%v body=%s", response.status, response.err, truncate(string(response.body), 1024))
	}
	if elapsed := time.Since(started); elapsed < 3*time.Second || elapsed > 10*time.Second {
		t.Fatalf("tool upgrade duration = %s, want about 3s without heartbeat timeout", elapsed)
	}
	if !strings.Contains(string(response.body), `"after":"2.0.0"`) {
		t.Fatalf("tool upgrade response missing the measured result: %s", truncate(string(response.body), 512))
	}
	if strings.Contains(agent.output(), "与 hub ") && strings.Contains(agent.output(), "的连接已断开") {
		t.Fatalf("long tool upgrade disconnected the agent:\n%s", agent.output())
	}
	if strings.Contains(world.process.output(), "ping timeout") {
		t.Fatalf("long tool upgrade triggered a hub ping timeout:\n%s", world.process.output())
	}
	_ = agent
}

func TestPullLongBudget(t *testing.T) {
	if got := hub.CallBudget(hub.TaskKindPull); got != 12*time.Minute {
		t.Fatalf("hub pull budget = %s, want 12m", got)
	}
	if hub.PullWait != 12*time.Minute {
		t.Fatalf("hub PullWait = %s, want 12m", hub.PullWait)
	}
	world := newHomerWorld(t)
	agent := startHelperAgent(t, world.root, world.global, filepath.Join(world.root, "pull-agent"), world.url,
		"pull-agent", world.token, fakeExecutorConfig{PullDelayMS: 700}, 100*time.Millisecond, 0, 0, 0)
	waitAgent(t, world, "pull-agent", true, 10*time.Second)
	started := time.Now()
	response := apiJSON(t, world, http.MethodPost, "/api/agents/pull-agent/pull?confirm=true", map[string]any{}, 5*time.Second)
	if response.err != nil || response.status != http.StatusOK {
		t.Fatalf("scaled long-budget pull response = %d err=%v body=%s", response.status, response.err, truncate(string(response.body), 1024))
	}
	if time.Since(started) < 700*time.Millisecond {
		t.Fatalf("scaled pull returned before the delayed executor completed: %s", time.Since(started))
	}
	_ = agent
}

func TestFourTasksParallel(t *testing.T) {
	world := newHomerWorld(t)
	const delay = 300 * time.Millisecond
	ids := []string{"parallel-00", "parallel-01", "parallel-02", "parallel-03"}
	agents := make([]*childProcess, 0, len(ids))
	for _, id := range ids {
		agents = append(agents, startHelperAgent(t, world.root, world.global, filepath.Join(world.root, id), world.url, id, world.token,
			fakeExecutorConfig{StatusDelayMS: int(delay / time.Millisecond)}, 3*time.Second, 0, 0, 0))
	}
	waitAgents(t, world, ids, true, 15*time.Second)
	var slowestSingle time.Duration
	for _, id := range ids {
		started := time.Now()
		response := apiJSON(t, world, http.MethodPost, "/api/agents/"+id+"/status", nil, 5*time.Second)
		apiBody(t, response, http.StatusOK)
		if elapsed := time.Since(started); elapsed > slowestSingle {
			slowestSingle = elapsed
		}
	}
	started := time.Now()
	responses := make(chan httpResult, len(ids))
	var workers sync.WaitGroup
	for _, id := range ids {
		id := id
		workers.Add(1)
		go func() {
			defer workers.Done()
			responses <- requestRaw(world, http.MethodPost, "/api/agents/"+id+"/status", nil, 5*time.Second)
		}()
	}
	workers.Wait()
	close(responses)
	for response := range responses {
		if response.err != nil || response.status != http.StatusOK {
			t.Errorf("parallel agent task failed: status=%d err=%v body=%s", response.status, response.err, truncate(string(response.body), 512))
		}
	}
	elapsed := time.Since(started)
	if elapsed > slowestSingle*13/10 {
		t.Fatalf("four-machine fanout took %s; slowest serial task took %s (limit %s)", elapsed, slowestSingle, slowestSingle*13/10)
	}
	t.Logf("four-machine fanout: batch=%s slowest-single=%s", elapsed, slowestSingle)
}

func TestWriteSerializedReadParallel(t *testing.T) {
	inProcess := newInProcessWorld(t)
	world := inProcess.world
	eventLog := filepath.Join(world.root, "task-activity.jsonl")
	const delay = 350 * time.Millisecond
	agent := startHelperAgent(t, world.root, world.global, filepath.Join(world.root, "task-agent"), world.url,
		"task-agent", world.token, fakeExecutorConfig{
			StatusDelayMS: int(delay / time.Millisecond), DiffDelayMS: int(delay / time.Millisecond),
			PushDelayMS: int(delay / time.Millisecond), EventLog: eventLog,
		}, 0, 0, 0, delay)
	waitAgent(t, world, "task-agent", true, 10*time.Second)
	waitForFileEvent(t, eventLog, "status", "end", 5*time.Second)
	waitFor(t, 3*time.Second, "initial executor idle", func() bool {
		return latestTotalActivity(activitiesFromLog(t, eventLog)) == 0
	})
	resetEventLog(t, eventLog)

	upgrade, err := startAsyncRequest(world, http.MethodPost, "/api/agents/task-agent/tool-upgrade", []byte(`{"tool":"pi"}`), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitForFileEvent(t, eventLog, "tool-upgrade", "start", 3*time.Second)
	push, err := startAsyncRequest(world, http.MethodPost, "/api/agents/task-agent/push?confirm=true", []byte("{}"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "both writes pending at the hub", func() bool {
		session, ok := inProcess.dispatcher.Registry.Session("task-agent")
		return ok && session.Stats().Pending >= 2
	})
	if hasFileEvent(eventLog, "push", "start") {
		t.Fatal("push started while tool upgrade was active; agent write tasks must serialize")
	}
	upgradeResult := waitAsyncRequest(t, upgrade, 5*time.Second)
	pushResult := waitAsyncRequest(t, push, 5*time.Second)
	if upgradeResult.err != nil || upgradeResult.status != http.StatusOK {
		t.Fatalf("tool-upgrade write = %d err=%v body=%s", upgradeResult.status, upgradeResult.err, truncate(string(upgradeResult.body), 512))
	}
	if pushResult.err != nil || pushResult.status != http.StatusOK {
		t.Fatalf("push write = %d err=%v body=%s", pushResult.status, pushResult.err, truncate(string(pushResult.body), 512))
	}
	waitForFileEvent(t, eventLog, "push", "end", 3*time.Second)
	if got := maximumActivity(activitiesFromLog(t, eventLog), false); got > 1 {
		t.Fatalf("overlapping write tasks observed %d simultaneous writers, want at most 1", got)
	}

	waitFor(t, 3*time.Second, "writer and heartbeat scans idle", func() bool {
		return latestTotalActivity(activitiesFromLog(t, eventLog)) == 0
	})
	resetEventLog(t, eventLog)
	status, err := startAsyncRequest(world, http.MethodPost, "/api/agents/task-agent/status", nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := startAsyncRequest(world, http.MethodPost, "/api/agents/task-agent/diff?adapter=pi", []byte("{}"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	statusResult := waitAsyncRequest(t, status, 5*time.Second)
	diffResult := waitAsyncRequest(t, diff, 5*time.Second)
	if statusResult.err != nil || statusResult.status != http.StatusOK {
		t.Fatalf("parallel read status = %d err=%v", statusResult.status, statusResult.err)
	}
	if diffResult.err != nil || diffResult.status != http.StatusOK {
		t.Fatalf("parallel read diff = %d err=%v", diffResult.status, diffResult.err)
	}
	if got := maximumActivity(activitiesFromLog(t, eventLog), true); got < 2 {
		t.Fatalf("read executor maximum parallelism = %d, want at least 2", got)
	}
	_ = agent
}

func TestGracefulShutdownClose(t *testing.T) {
	world := newHomerWorld(t)
	home := filepath.Join(world.root, "graceful-agent")
	initAgentWorkspace(t, world.global, home)
	agent := startHomerAgent(t, world.root, world.global, buildHomer(t), home, world.url,
		"graceful-agent", world.token)
	waitAgent(t, world, "graceful-agent", true, 15*time.Second)
	stopGracefully(t, world.process, 8*time.Second)
	waitForLog(t, agent, "1001", 5*time.Second)
	if !strings.Contains(agent.output(), "hub restarting") {
		t.Fatalf("agent close log omitted graceful shutdown reason:\n%s", agent.output())
	}
	world.process = startHomerHub(t, world.root, world.global, world.home, world.addr, world.token)
	waitFor(t, 10*time.Second, "hub restart after graceful close", func() bool {
		status, _, err := httpRequest(world.url, world.token, http.MethodGet, "/api/health", nil, 500*time.Millisecond)
		return err == nil && status == http.StatusOK
	})
	waitAgent(t, world, "graceful-agent", true, 10*time.Second)
	status := apiJSON(t, world, http.MethodPost, "/api/agents/graceful-agent/status", nil, 15*time.Second)
	_ = decodeTaskReport(t, apiBody(t, status, http.StatusOK))
}

func TestStreamThroughWebServer(t *testing.T) {
	world := newHelperWorld(t, 0, 0, 100*time.Millisecond)
	agent := startHomerAgent(t, world.root, world.global, buildHomer(t), filepath.Join(world.root, "web-agent"),
		world.url, "web-server-agent", world.token)
	waitAgent(t, world, "web-server-agent", true, 10*time.Second)
	// The HTTP server's initial write deadline has expired; the WebSocket must
	// stay alive because the full web.Server chain clears it during Hijack.
	writeTimeoutElapsed := time.NewTimer(500 * time.Millisecond)
	select {
	case <-writeTimeoutElapsed.C:
	case <-agent.finished:
		writeTimeoutElapsed.Stop()
		t.Fatalf("agent exited before the server's WriteTimeout: %s", agent.output())
	}
	response := apiJSON(t, world, http.MethodPost, "/api/agents/web-server-agent/status", nil, 10*time.Second)
	if response.err != nil {
		t.Fatal(response.err)
	}
	if response.status != http.StatusOK {
		t.Fatalf("web.Server stream after WriteTimeout = %d %s", response.status, truncate(string(response.body), 512))
	}
	waitAgent(t, world, "web-server-agent", true, 2*time.Second)
	_ = agent
}

func TestHalfOpenDetectedByPing(t *testing.T) {
	testBlackholeConnection(t, "half-open-agent")
}

func TestBlackholeProxy(t *testing.T) {
	testBlackholeConnection(t, "blackhole-agent")
}

func testBlackholeConnection(t *testing.T, agentID string) {
	t.Helper()
	world := newInProcessWorld(t, stream.Options{PingInterval: 40 * time.Millisecond, PingTimeout: 260 * time.Millisecond})
	eventLog := filepath.Join(world.world.root, "blackhole-events.jsonl")
	agent, route, fault := startFaultRoutedAgent(t, world.world, agentID, world.world.token,
		fakeExecutorConfig{BlockDiff: true, EventLog: eventLog}, 0, 40*time.Millisecond, 260*time.Millisecond)
	waitAgent(t, world.world, agentID, true, 10*time.Second)
	request, err := startAsyncRequest(world.world, http.MethodPost, "/api/agents/"+agentID+"/diff?adapter=pi", []byte("{}"), 8*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitForFileEvent(t, eventLog, "diff", "start", 5*time.Second)
	fault.Blackhole()
	started := time.Now()
	response := waitAsyncRequest(t, request, 3*time.Second)
	if response.err != nil || response.status != http.StatusServiceUnavailable || errorCode(t, response.body) != "agent-offline" {
		t.Fatalf("half-open in-flight result = status %d err=%v body=%s", response.status, response.err, truncate(string(response.body), 512))
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("ping timeout detected half-open connection after %s, want scaled timeout", elapsed)
	}
	waitFor(t, 3*time.Second, "ping-timeout diagnostic", func() bool {
		return strings.Contains(agent.output(), "ping timeout") || strings.Contains(world.agentHubLog(), "ping timeout")
	})
	_ = route
}

func helloRawAgent(t *testing.T, conn stream.Conn, agentID string) {
	t.Helper()
	params, err := json.Marshal(hub.HelloParams{
		Proto: stream.ProtocolVersion, AgentID: agentID, Hostname: agentID, Version: "integration",
		Caps: []string{hub.MethodStatus, hub.MethodDiff},
	})
	if err != nil {
		t.Fatalf("encode raw hello: %v", err)
	}
	frame, err := stream.EncodeFrame(&stream.Frame{T: stream.TReq, ID: "raw-hello", M: hub.MethodHello, P: params}, stream.DefaultMaxFrame)
	if err != nil {
		t.Fatalf("encode raw hello frame: %v", err)
	}
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	err = conn.Write(writeCtx, frame)
	writeCancel()
	if err != nil {
		t.Fatalf("write raw hello: %v", err)
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readCancel()
	message, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("read raw welcome: %v", err)
	}
	welcome, err := stream.DecodeFrame(message, stream.DefaultMaxFrame)
	if err != nil || welcome.ID != "raw-hello" || !welcome.OK {
		t.Fatalf("raw hello response = %+v err=%v", welcome, err)
	}
}

func waitForCloseCode(t *testing.T, conn stream.Conn, expected stream.CloseCode, timeout time.Duration) {
	t.Helper()
	if err := readCloseCode(conn, expected, timeout); err != nil {
		t.Fatalf("wait for close code %d: %v", expected, err)
	}
}

func readCloseCode(conn stream.Conn, expected stream.CloseCode, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		message, err := conn.Read(ctx)
		if err != nil {
			var closeErr *stream.CloseError
			if errors.As(err, &closeErr) && closeErr.Code == expected {
				return nil
			}
			return fmt.Errorf("stream close = %v, want close code %d", err, expected)
		}
		frame, decodeErr := stream.DecodeFrame(message, stream.DefaultMaxFrame)
		if decodeErr != nil {
			return fmt.Errorf("decode frame while waiting for close code %d: %w", expected, decodeErr)
		}
		if frame.T != stream.TPing {
			continue
		}
		pong, encodeErr := stream.EncodeFrame(&stream.Frame{T: stream.TPong, ID: frame.ID}, stream.DefaultMaxFrame)
		if encodeErr != nil {
			return fmt.Errorf("encode pong while waiting for close: %w", encodeErr)
		}
		writeCtx, writeCancel := context.WithTimeout(ctx, time.Second)
		writeErr := conn.Write(writeCtx, pong)
		writeCancel()
		if writeErr != nil {
			return fmt.Errorf("write pong while waiting for close: %w", writeErr)
		}
	}
}

func openRawAgentStream(t *testing.T, baseURL, token string) stream.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+token)
	conn, _, err := wsconn.Dial(ctx, agentStreamURL(baseURL), wsconn.DialOptions{Header: header})
	if err != nil {
		t.Fatalf("dial raw agent stream: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func TestMalformedJSONFromAgent(t *testing.T) {
	world := newHomerWorld(t)
	agent := startHomerAgent(t, world.root, world.global, buildHomer(t), filepath.Join(world.root, "other-agent-home"),
		world.url, "unaffected-agent", world.token)
	waitAgent(t, world, "unaffected-agent", true, 10*time.Second)
	conn := openRawAgentStream(t, world.url, world.token)
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := conn.Write(writeCtx, []byte("{not-json"))
	writeCancel()
	if err != nil {
		t.Fatalf("write malformed JSON frame: %v", err)
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err = conn.Read(readCtx)
	readCancel()
	var closeErr *stream.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != stream.CloseProtocol {
		t.Fatalf("malformed frame close = %v, want close 1002", err)
	}
	response := apiJSON(t, world, http.MethodPost, "/api/agents/unaffected-agent/status", nil, 10*time.Second)
	_ = decodeTaskReport(t, apiBody(t, response, http.StatusOK))
	_ = agent
}

func TestOversizeFromAgent(t *testing.T) {
	world := newHomerWorld(t)
	conn := openRawAgentStream(t, world.url, world.token)
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := conn.Write(writeCtx, []byte(strings.Repeat("x", (8<<20)+1)))
	writeCancel()
	if err != nil {
		t.Fatalf("write oversize frame: %v", err)
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = conn.Read(readCtx)
	readCancel()
	var closeErr *stream.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != stream.CloseTooBig {
		t.Fatalf("oversize frame close = %v, want close 1009", err)
	}
}

func TestBinaryFrame(t *testing.T) {
	world := newHomerWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+world.token)
	wsURL := "ws" + strings.TrimPrefix(agentStreamURL(world.url), "http")
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: header, Subprotocols: []string{stream.Subprotocol},
	})
	if err != nil {
		t.Fatalf("dial binary frame test connection: %v", err)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatalf("write binary frame: %v", err)
	}
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusCode(stream.CloseUnsupported) {
		t.Fatalf("binary frame close = %v (%d), want close 1003", err, websocket.CloseStatus(err))
	}
}

func TestFrameBeforeHello(t *testing.T) {
	world := newHomerWorld(t)
	conn := openRawAgentStream(t, world.url, world.token)
	frame, err := stream.EncodeFrame(&stream.Frame{T: stream.TPing, ID: "premature"}, stream.DefaultMaxFrame)
	if err != nil {
		t.Fatal(err)
	}
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	err = conn.Write(writeCtx, frame)
	writeCancel()
	if err != nil {
		t.Fatal(err)
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err = conn.Read(readCtx)
	readCancel()
	var closeErr *stream.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != stream.ClosePolicy {
		t.Fatalf("frame-before-hello close = %v, want close 1008", err)
	}
}

func TestHelloTimeout(t *testing.T) {
	world := newHelperWorld(t, 0, 0, 0)
	conn := openRawAgentStream(t, world.url, world.token)
	readCtx, readCancel := context.WithTimeout(context.Background(), 13*time.Second)
	defer readCancel()
	_, err := conn.Read(readCtx)
	var closeErr *stream.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != stream.ClosePolicy {
		t.Fatalf("hello timeout close = %v, want close 1008", err)
	}
	waitForLog(t, world.process, "hello timed out", 2*time.Second)
}

func TestHelloIDMismatch(t *testing.T) {
	world := newHomerWorld(t)
	code := getJoinCode(t, world)
	// Complete enrollment first so this credential is now bound to one ID.
	home := filepath.Join(world.root, "bound-agent")
	agent := startHomerAgent(t, world.root, world.global, buildHomer(t), home, world.url, "bound-agent", code)
	waitAgent(t, world, "bound-agent", true, 15*time.Second)
	secret := agentSecret(t, home)
	stopGracefully(t, agent, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+secret)
	conn, _, err := wsconn.Dial(ctx, agentStreamURL(world.url), wsconn.DialOptions{Header: header})
	if err != nil {
		t.Fatalf("dial for hello-ID mismatch: %v", err)
	}
	defer conn.CloseNow()
	payload, err := json.Marshal(hub.HelloParams{
		Proto: stream.ProtocolVersion, AgentID: "some-other-id", Hostname: "other", Version: "integration",
		Caps: []string{hub.MethodStatus},
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := stream.EncodeFrame(&stream.Frame{T: stream.TReq, ID: "mismatch", M: hub.MethodHello, P: payload}, stream.DefaultMaxFrame)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, frame); err != nil {
		t.Fatal(err)
	}
	message, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read hello ID mismatch response: %v", err)
	}
	frameResult, err := stream.DecodeFrame(message, stream.DefaultMaxFrame)
	if err != nil || frameResult.ID != "mismatch" || frameResult.E == nil || frameResult.E.Code != "agent-id-mismatch" {
		t.Fatalf("hello ID mismatch response = %+v err=%v", frameResult, err)
	}
	_, err = conn.Read(ctx)
	var closeErr *stream.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != stream.CloseRevoked {
		t.Fatalf("hello ID mismatch close = %v, want 4401", err)
	}
	waitForLog(t, world.process, "identity mismatch", 2*time.Second)
}

func TestReservedIntegrationHelperEnvIsScoped(t *testing.T) {
	if os.Getenv(helperModeEnv) != "" || os.Getenv(helperConfigEnv) != "" {
		t.Fatal("helper environment leaked into the top-level test process")
	}
}
