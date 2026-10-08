package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/agentd"
	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
	"github.com/zzjcool/homer-cli/internal/stream/wsconn"
	"github.com/zzjcool/homer-cli/internal/toolctl"
	"github.com/zzjcool/homer-cli/internal/web"
)

const (
	helperModeEnv   = "HOMER_INTEGRATION_HELPER_MODE"
	helperConfigEnv = "HOMER_INTEGRATION_HELPER_CONFIG"
)

var homerBinary string

// TestMain builds the same CLI binary used by the repository's e2e tests.
// Helper processes are instances of this test binary and deliberately bypass
// the build, so a test can use an injected executor without changing product
// code or spawning nested `go test` commands.
func TestMain(m *testing.M) {
	if os.Getenv(helperModeEnv) != "" {
		os.Exit(m.Run())
	}

	buildDir, err := os.MkdirTemp("", "homer-integration-build-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create integration build dir:", err)
		os.Exit(2)
	}
	global := filepath.Join(buildDir, "gitconfig")
	if err := os.WriteFile(global, []byte("[user]\n\tname = Homer Integration\n\temail = integration@example.invalid\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "create integration git config:", err)
		_ = os.RemoveAll(buildDir)
		os.Exit(2)
	}
	homerBinary = filepath.Join(buildDir, "homer")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	command := exec.CommandContext(buildCtx, "go", "build", "-o", homerBinary, "./cmd/homer")
	command.Dir = repositoryRoot()
	command.Env = processEnv(map[string]string{
		"GIT_CONFIG_GLOBAL":   global,
		"GIT_CONFIG_NOSYSTEM": "1",
	})
	output, err := command.CombinedOutput()
	buildCancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "go build ./cmd/homer: %v\n%s", err, output)
		_ = os.RemoveAll(buildDir)
		os.Exit(2)
	}

	code := m.Run()
	_ = os.RemoveAll(buildDir)
	os.Exit(code)
}

func repositoryRoot() string {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
}

func buildHomer(t *testing.T) string {
	t.Helper()
	if homerBinary == "" {
		t.Fatal("TestMain did not build homer")
	}
	return homerBinary
}

type childProcess struct {
	cmd       *exec.Cmd
	logPath   string
	logFile   *os.File
	readLog   *os.File
	finished  chan struct{}
	waitErr   error
	waitMu    sync.Mutex
	closeOnce sync.Once
	killMu    sync.Mutex
	killErr   error
}

func startChild(t *testing.T, binary, logPath, global string, env map[string]string, args ...string) *childProcess {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatalf("create log directory: %v", err)
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open process log: %v", err)
	}
	childEnv := make(map[string]string, len(env)+4)
	for key, value := range env {
		childEnv[key] = value
	}
	if global != "" {
		childEnv["GIT_CONFIG_GLOBAL"] = global
	}
	childEnv["GIT_CONFIG_NOSYSTEM"] = "1"
	if _, ok := childEnv["HOME"]; !ok {
		childEnv["HOME"] = filepath.Dir(logPath)
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = repositoryRoot()
	cmd.Env = processEnv(childEnv)
	cmd.Stdout = file
	cmd.Stderr = file
	if err := cmd.Start(); err != nil {
		_ = file.Close()
		t.Fatalf("start %s %v: %v", binary, args, err)
	}
	process := &childProcess{cmd: cmd, logPath: logPath, logFile: file, finished: make(chan struct{})}
	if runtime.GOOS == "linux" {
		process.readLog, _ = os.Open(fmt.Sprintf("/proc/%d/fd/1", cmd.Process.Pid))
	}
	t.Cleanup(func() {
		process.killAndWait(t)
	})
	go func() {
		err := cmd.Wait()
		process.waitMu.Lock()
		process.waitErr = err
		process.waitMu.Unlock()
		close(process.finished)
	}()
	return process
}

func (p *childProcess) pid() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *childProcess) signal(signal os.Signal) error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return errors.New("process is not running")
	}
	select {
	case <-p.finished:
		return errors.New("process has exited")
	default:
		return p.cmd.Process.Signal(signal)
	}
}

func (p *childProcess) wait(timeout time.Duration) error {
	if p == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.finished:
		p.waitMu.Lock()
		err := p.waitErr
		p.waitMu.Unlock()
		return err
	case <-timer.C:
		return fmt.Errorf("process %d did not exit within %s", p.pid(), timeout)
	}
}

func (p *childProcess) killAndWait(t *testing.T) {
	if p == nil {
		return
	}
	p.killMu.Lock()
	defer p.killMu.Unlock()
	select {
	case <-p.finished:
	default:
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		select {
		case <-p.finished:
		case <-time.After(5 * time.Second):
			p.killErr = fmt.Errorf("process %d did not exit after Kill", p.pid())
			if t != nil {
				t.Errorf("reap child pid %d: %v", p.pid(), p.killErr)
			}
		}
	}
	p.closeOnce.Do(func() {
		if p.logFile != nil {
			_ = p.logFile.Close()
		}
	})
	if p.readLog != nil {
		_ = p.readLog.Close()
	}
}

func (p *childProcess) output() string {
	if p == nil || p.logPath == "" {
		return ""
	}
	body, err := os.ReadFile(p.logPath)
	if err != nil {
		return "<read log: " + err.Error() + ">"
	}
	if p.readLog != nil {
		_ = p.readLog.SetDeadline(time.Now().Add(50 * time.Millisecond))
		live, _ := io.ReadAll(p.readLog)
		_ = p.readLog.SetDeadline(time.Time{})
		body = append(body, live...)
	}
	return string(body)
}

func (p *childProcess) waitForExit(t *testing.T, timeout time.Duration) {
	t.Helper()
	if err := p.wait(timeout); err != nil {
		t.Fatalf("wait for pid %d: %v\n%s", p.pid(), err, p.output())
	}
}

func processEnv(overrides map[string]string) []string {
	env := os.Environ()
	for key, value := range overrides {
		prefix := key + "="
		filtered := env[:0]
		for _, item := range env {
			if !strings.HasPrefix(item, prefix) {
				filtered = append(filtered, item)
			}
		}
		env = append(filtered, prefix+value)
	}
	return env
}

func testRoot(t *testing.T) (root, global string) {
	t.Helper()
	root = t.TempDir()
	global = filepath.Join(root, "gitconfig")
	if err := os.WriteFile(global, []byte("[user]\n\tname = Homer Integration\n\temail = integration@example.invalid\n"), 0o600); err != nil {
		t.Fatalf("write temporary git config: %v", err)
	}
	return root, global
}

func reserveAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release loopback address: %v", err)
	}
	return addr
}

func loopbackURL(addr string) string { return "http://" + addr }

func startHomerHub(t *testing.T, root, global, home, addr, token string) *childProcess {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create hub home: %v", err)
	}
	logPath := filepath.Join(root, "hub.log")
	return startChild(t, buildHomer(t), logPath, global, map[string]string{
		"HOME": home, "HOMER_HOME": home,
	}, "serve", "--addr", addr, "--token", token, "--home", home)
}

func startHomerAgent(t *testing.T, root, global, binary, home, hubURL, agentID, token string, extra ...string) *childProcess {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create agent home: %v", err)
	}
	args := []string{"agent", "--hub", hubURL, "--id", agentID, "--home", home}
	if token != "" {
		args = append(args, "--token", token)
	}
	args = append(args, extra...)
	logPath := filepath.Join(root, "agent-"+sanitizeName(agentID)+"-"+fmt.Sprint(time.Now().UnixNano())+".log")
	return startChild(t, binary, logPath, global, map[string]string{
		"HOME": home, "HOMER_HOME": home,
	}, args...)
}

func sanitizeName(value string) string {
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "agent"
	}
	return b.String()
}

type hubWorld struct {
	root    string
	global  string
	home    string
	addr    string
	url     string
	token   string
	process *childProcess
}

type inProcessWorld struct {
	world      *hubWorld
	dispatcher *hub.Dispatcher
	agentHub   *hub.AgentHub
	server     *http.Server
	serveDone  chan error
	logMu      sync.Mutex
	logLines   []string
}

func (w *inProcessWorld) agentHubLog() string {
	if w == nil {
		return ""
	}
	w.logMu.Lock()
	defer w.logMu.Unlock()
	return strings.Join(w.logLines, "\n")
}

type inProcessLogWriter struct {
	write func(string)
}

func (w inProcessLogWriter) Write(data []byte) (int, error) {
	if w.write != nil {
		w.write(strings.TrimSuffix(string(data), "\n"))
	}
	return len(data), nil
}

func newInProcessWorld(t *testing.T, sessionOptions ...stream.Options) *inProcessWorld {
	t.Helper()
	root, global := testRoot(t)
	home := filepath.Join(root, "hub-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen local test hub: %v", err)
	}
	token := "integration-" + sanitizeName(t.Name())
	registry := hub.NewRegistry()
	enrollment, err := hub.OpenEnrollment(home)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("open local test enrollment: %v", err)
	}
	auth := &hub.Authenticator{Token: token, Enrollment: enrollment}
	fixture := &inProcessWorld{logLines: make([]string, 0)}
	logger := log.New(inProcessLogWriter{write: func(line string) {
		fixture.logMu.Lock()
		fixture.logLines = append(fixture.logLines, line)
		fixture.logMu.Unlock()
	}}, "in-process integration hub: ", log.LstdFlags|log.Lmicroseconds)
	dispatcher := hub.NewDispatcher(registry, token)
	var session stream.Options
	if len(sessionOptions) > 0 {
		session = sessionOptions[0]
	}
	session.Logger = logger
	agentHub := hub.NewAgentHub(registry, auth, enrollment, hub.HubOptions{Logger: logger, Session: session})
	dispatcher.Hub = agentHub
	webServer, err := web.NewServer(web.ServeOptions{
		Addr: listener.Addr().String(), HomerHome: home, Token: token,
		Agents: dispatcher, AgentEndpoint: agentHub, Enrollment: enrollment,
		AgentEndpointAuthorized: auth.Authorized,
	})
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create in-process web server: %v", err)
	}
	server := &http.Server{Handler: webServer.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	fixture.world = &hubWorld{root: root, global: global, home: home, addr: listener.Addr().String(),
		url: loopbackURL(listener.Addr().String()), token: token}
	fixture.dispatcher = dispatcher
	fixture.agentHub = agentHub
	fixture.server = server
	fixture.serveDone = serveDone
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := agentHub.Shutdown(ctx); err != nil {
			t.Errorf("shutdown in-process AgentHub: %v", err)
		}
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown in-process web server: %v", err)
			_ = server.Close()
		}
		select {
		case err := <-serveDone:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("in-process web server returned: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("in-process web server did not exit")
		}
	})
	return fixture
}

func newHomerWorld(t *testing.T) *hubWorld {
	t.Helper()
	root, global := testRoot(t)
	home := filepath.Join(root, "hub-home")
	addr := reserveAddr(t)
	token := "integration-" + sanitizeName(t.Name())
	process := startHomerHub(t, root, global, home, addr, token)
	world := &hubWorld{root: root, global: global, home: home, addr: addr, url: loopbackURL(addr), token: token, process: process}
	waitFor(t, 10*time.Second, "homer hub ready", func() bool {
		status, _, err := httpRequest(world.url, token, http.MethodGet, "/api/health", nil, 500*time.Millisecond)
		return err == nil && status == http.StatusOK
	})
	return world
}

type helperConfig struct {
	Mode             string             `json:"mode"`
	Addr             string             `json:"addr,omitempty"`
	HomerHome        string             `json:"homerHome"`
	Home             string             `json:"home"`
	HubURL           string             `json:"hubUrl,omitempty"`
	DataURL          string             `json:"dataUrl,omitempty"`
	AgentID          string             `json:"agentId,omitempty"`
	Token            string             `json:"token,omitempty"`
	TaskTimeoutMS    int                `json:"taskTimeoutMs,omitempty"`
	PingIntervalMS   int                `json:"pingIntervalMs,omitempty"`
	PingTimeoutMS    int                `json:"pingTimeoutMs,omitempty"`
	WriteTimeoutMS   int                `json:"writeTimeoutMs,omitempty"`
	Executor         fakeExecutorConfig `json:"executor,omitempty"`
	ToolkitUpgradeMS int                `json:"toolkitUpgradeMs,omitempty"`
	BackoffBaseMS    int                `json:"backoffBaseMs,omitempty"`
	BackoffMaxMS     int                `json:"backoffMaxMs,omitempty"`
}

type fakeExecutorConfig struct {
	StatusDelayMS int    `json:"statusDelayMs,omitempty"`
	DiffDelayMS   int    `json:"diffDelayMs,omitempty"`
	PushDelayMS   int    `json:"pushDelayMs,omitempty"`
	PullDelayMS   int    `json:"pullDelayMs,omitempty"`
	PayloadBytes  int    `json:"payloadBytes,omitempty"`
	BlockDiff     bool   `json:"blockDiff,omitempty"`
	EventLog      string `json:"eventLog,omitempty"`
}

func startHelperHub(t *testing.T, root, global, home, addr, token string, pingInterval, pingTimeout, writeTimeout time.Duration) *childProcess {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create helper hub home: %v", err)
	}
	cfg := helperConfig{Mode: "hub", Addr: addr, HomerHome: home, Home: home, Token: token}
	if pingInterval > 0 {
		cfg.PingIntervalMS = int(pingInterval / time.Millisecond)
	}
	if pingTimeout > 0 {
		cfg.PingTimeoutMS = int(pingTimeout / time.Millisecond)
	}
	if writeTimeout > 0 {
		cfg.WriteTimeoutMS = int(writeTimeout / time.Millisecond)
	}
	return startHelper(t, root, global, cfg, "test-hub.log")
}

func startHelperAgent(t *testing.T, root, global, home, hubURL, agentID, token string, execCfg fakeExecutorConfig, taskTimeout, pingInterval, pingTimeout, toolkitUpgrade time.Duration) *childProcess {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create helper agent home: %v", err)
	}
	cfg := helperConfig{
		Mode: "agent", HomerHome: home, Home: home, HubURL: hubURL, DataURL: hubURL,
		AgentID: agentID, Token: token, Executor: execCfg,
	}
	if taskTimeout > 0 {
		cfg.TaskTimeoutMS = int(taskTimeout / time.Millisecond)
	}
	if pingInterval > 0 {
		cfg.PingIntervalMS = int(pingInterval / time.Millisecond)
	}
	if pingTimeout > 0 {
		cfg.PingTimeoutMS = int(pingTimeout / time.Millisecond)
	}
	if toolkitUpgrade > 0 {
		cfg.ToolkitUpgradeMS = int(toolkitUpgrade / time.Millisecond)
	}
	cfg.BackoffBaseMS = 100
	cfg.BackoffMaxMS = 500
	return startHelper(t, root, global, cfg, "helper-agent-"+sanitizeName(agentID)+".log")
}

func startHelper(t *testing.T, root, global string, cfg helperConfig, logName string) *childProcess {
	t.Helper()
	configPath := filepath.Join(root, "helper-"+sanitizeName(logName)+".json")
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("encode helper config: %v", err)
	}
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		t.Fatalf("write helper config: %v", err)
	}
	binary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("resolve integration test binary: %v", err)
	}
	return startChild(t, binary, filepath.Join(root, logName), global, map[string]string{
		helperModeEnv: "1", helperConfigEnv: configPath,
		"HOME": cfg.Home, "HOMER_HOME": cfg.HomerHome,
	}, "-test.run=^TestHelperProcess$")
}

// TestHelperProcess is a real child process around the production agentd and
// hub packages. Its executor and timer settings are configurable for fault
// injection, without changing or exporting any production test seams.
func TestHelperProcess(t *testing.T) {
	configPath := os.Getenv(helperConfigEnv)
	if configPath == "" {
		return
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read helper config: %v", err)
	}
	var cfg helperConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode helper config: %v", err)
	}
	switch cfg.Mode {
	case "agent":
		runHelperAgent(t, cfg)
	case "silent-agent":
		runSilentAgent(t, cfg)
	case "hub":
		runHelperHub(t, cfg)
	default:
		t.Fatalf("unknown helper mode %q", cfg.Mode)
	}
}

func runHelperAgent(t *testing.T, cfg helperConfig) {
	logger := log.New(os.Stderr, "integration agent: ", log.LstdFlags|log.Lmicroseconds)
	fmt.Fprintf(os.Stderr, "integration agent boot agent=%s pid=%d\n", cfg.AgentID, os.Getpid())
	config := helperAgentConfig(cfg, cfg.AgentID, cfg.Home, logger)
	resolved, err := agentd.ResolveConfig(config)
	if err != nil {
		t.Fatalf("resolve helper agent config: %v", err)
	}
	executor := &processExecutor{cfg: cfg.Executor, active: make(map[string]int)}
	daemon := agentd.New(resolved, executor)
	daemon.SetToolkit(helperToolkit(executor, time.Duration(cfg.ToolkitUpgradeMS)*time.Millisecond))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatalf("agent daemon stopped unexpectedly: %v", err)
	}
}

func helperAgentConfig(cfg helperConfig, agentID, home string, logger *log.Logger) agentd.Config {
	config := agentd.Config{
		Home: home, HomerHome: cfg.HomerHome, HubURL: cfg.HubURL, DataURL: cfg.DataURL,
		AgentID: agentID, Token: cfg.Token,
		Stream: stream.Options{
			Logger:       logger,
			PingInterval: time.Duration(cfg.PingIntervalMS) * time.Millisecond,
			PingTimeout:  time.Duration(cfg.PingTimeoutMS) * time.Millisecond,
		},
		Backoff: stream.Backoff{Base: time.Duration(cfg.BackoffBaseMS) * time.Millisecond, Max: time.Duration(cfg.BackoffMaxMS) * time.Millisecond, Factor: 2, Jitter: 0},
	}
	if cfg.TaskTimeoutMS > 0 {
		config.TaskTimeout = time.Duration(cfg.TaskTimeoutMS) * time.Millisecond
	}
	return config
}

func helperToolkit(executor *processExecutor, upgradeDelay time.Duration) agentd.Toolkit {
	return agentd.Toolkit{
		Probe: func(context.Context) []toolctl.Status {
			return []toolctl.Status{{ID: "pi", Version: "1.0.0", Upgradable: true}}
		},
		Upgrade: func(ctx context.Context, id string) toolctl.UpgradeResult {
			if err := executor.run(ctx, "tool-upgrade", upgradeDelay, false); err != nil {
				return toolctl.UpgradeResult{Status: "failed", Tool: id, Note: err.Error()}
			}
			return toolctl.UpgradeResult{OK: true, Status: "upgraded", Tool: id, Before: "1.0.0", After: "2.0.0"}
		},
	}
}

func agentStreamURL(base string) string {
	return strings.TrimRight(base, "/") + "/agent/v1/stream"
}

func runSilentAgent(t *testing.T, cfg helperConfig) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	header := make(http.Header)
	if cfg.Token != "" {
		header.Set("Authorization", "Bearer "+cfg.Token)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	conn, _, err := wsconn.Dial(dialCtx, agentStreamURL(cfg.HubURL), wsconn.DialOptions{Header: header})
	cancel()
	if err != nil {
		t.Fatalf("silent agent dial: %v", err)
	}
	params, err := json.Marshal(hub.HelloParams{
		Proto: stream.ProtocolVersion, AgentID: cfg.AgentID, Hostname: "silent-consumer", Version: "integration",
		Caps: []string{hub.MethodStatus, hub.MethodDiff},
	})
	if err != nil {
		_ = conn.CloseNow()
		t.Fatalf("encode silent hello: %v", err)
	}
	frame, err := stream.EncodeFrame(&stream.Frame{T: stream.TReq, ID: "silent-hello", M: hub.MethodHello, P: params}, stream.DefaultMaxFrame)
	if err != nil {
		_ = conn.CloseNow()
		t.Fatalf("encode silent hello frame: %v", err)
	}
	writeCtx, writeCancel := context.WithTimeout(ctx, 3*time.Second)
	err = conn.Write(writeCtx, frame)
	writeCancel()
	if err != nil {
		_ = conn.CloseNow()
		t.Fatalf("write silent hello: %v", err)
	}
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	welcomeFrame, err := conn.Read(readCtx)
	readCancel()
	if err != nil {
		_ = conn.CloseNow()
		t.Fatalf("read silent hello response: %v", err)
	}
	decoded, err := stream.DecodeFrame(welcomeFrame, stream.DefaultMaxFrame)
	if err != nil || !decoded.OK || decoded.ID != "silent-hello" {
		_ = conn.CloseNow()
		t.Fatalf("invalid silent hello response: frame=%+v err=%v", decoded, err)
	}
	fmt.Fprintf(os.Stderr, "silent agent connected agent=%s; intentionally not reading tasks\n", cfg.AgentID)
	<-ctx.Done()
	_ = conn.Close(stream.CloseGoingAway, "integration agent exiting")
}

func runHelperHub(t *testing.T, cfg helperConfig) {
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		t.Fatalf("listen test hub: %v", err)
	}
	registry := hub.NewRegistry()
	enrollment, err := hub.OpenEnrollment(cfg.HomerHome)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("open test enrollment: %v", err)
	}
	auth := &hub.Authenticator{Token: cfg.Token, Enrollment: enrollment}
	logger := log.New(os.Stderr, "integration hub: ", log.LstdFlags|log.Lmicroseconds)
	dispatcher := hub.NewDispatcher(registry, cfg.Token)
	agentHub := hub.NewAgentHub(registry, auth, enrollment, hub.HubOptions{
		Logger: logger,
		Session: stream.Options{
			Logger:       logger,
			PingInterval: time.Duration(cfg.PingIntervalMS) * time.Millisecond,
			PingTimeout:  time.Duration(cfg.PingTimeoutMS) * time.Millisecond,
		},
	})
	dispatcher.Hub = agentHub
	webServer, err := web.NewServer(web.ServeOptions{
		Addr: listener.Addr().String(), HomerHome: cfg.HomerHome, Token: cfg.Token,
		Agents: dispatcher, AgentEndpoint: agentHub,
		Enrollment: enrollment, AgentEndpointAuthorized: auth.Authorized,
	})
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create test web server: %v", err)
	}
	writeTimeout := time.Duration(cfg.WriteTimeoutMS) * time.Millisecond
	if writeTimeout <= 0 {
		writeTimeout = 13 * time.Minute
	}
	httpServer := &http.Server{
		Handler: webServer.Handler(), ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout: writeTimeout,
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- httpServer.Serve(listener) }()
	fmt.Fprintf(os.Stderr, "integration hub ready addr=%s write_timeout=%s\n", listener.Addr(), writeTimeout)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-serveDone:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("test hub Serve: %v", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		if err := agentHub.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "integration hub stream shutdown: %v\n", err)
		}
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "integration hub HTTP shutdown: %v\n", err)
			_ = httpServer.Close()
		}
		cancel()
		select {
		case err := <-serveDone:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("test hub Serve after shutdown: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("test hub Serve did not stop after shutdown")
			_ = httpServer.Close()
		}
	}
}

type processExecutor struct {
	cfg         fakeExecutorConfig
	mu          sync.Mutex
	active      map[string]int
	activeTotal int
	writeActive int
	readActive  int
}

func (e *processExecutor) run(ctx context.Context, method string, delay time.Duration, block bool) error {
	e.mu.Lock()
	e.active[method]++
	e.activeTotal++
	if isReadActivity(method) {
		e.readActive++
	} else {
		e.writeActive++
	}
	active := e.active[method]
	e.writeEventLocked("start", method, active)
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.active[method]--
		e.activeTotal--
		if isReadActivity(method) {
			e.readActive--
		} else {
			e.writeActive--
		}
		e.writeEventLocked("end", method, e.active[method])
		e.mu.Unlock()
	}()

	if ctx == nil {
		ctx = context.Background()
	}
	if block {
		<-ctx.Done()
		e.writeEvent("canceled", method, active)
		return ctx.Err()
	}
	if delay <= 0 {
		select {
		case <-ctx.Done():
			e.writeEvent("canceled", method, active)
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		e.writeEvent("canceled", method, active)
		return ctx.Err()
	}
}

func (e *processExecutor) writeEvent(event, method string, active int) {
	e.mu.Lock()
	e.writeEventLocked(event, method, active)
	e.mu.Unlock()
}

func (e *processExecutor) writeEventLocked(event, method string, active int) {
	if e.cfg.EventLog == "" {
		return
	}
	file, err := os.OpenFile(e.cfg.EventLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"event": event, "method": method, "active": active, "totalActive": e.activeTotal,
		"writeActive": e.writeActive, "readActive": e.readActive, "at": time.Now().UnixNano(),
	})
	_, _ = file.Write(append(body, '\n'))
	_ = file.Close()
}

func (e *processExecutor) Status(ctx context.Context) (commands.StatusReport, error) {
	if err := e.run(ctx, "status", time.Duration(e.cfg.StatusDelayMS)*time.Millisecond, false); err != nil {
		return commands.StatusReport{}, err
	}
	errorsList := []string{}
	if e.cfg.PayloadBytes > 0 {
		errorsList = []string{strings.Repeat("x", e.cfg.PayloadBytes)}
	}
	return commands.StatusReport{
		Adapters: []commands.StatusAdapterReport{{ID: "integration", Categories: []commands.StatusCategoryReport{}}},
		Errors:   errorsList,
	}, nil
}

func (e *processExecutor) Diff(ctx context.Context, _ web.DiffParams) (string, error) {
	if err := e.run(ctx, "diff", time.Duration(e.cfg.DiffDelayMS)*time.Millisecond, e.cfg.BlockDiff); err != nil {
		return "", err
	}
	return "integration diff", nil
}

func (e *processExecutor) Push(ctx context.Context, _ bool, _ []string, _, _ bool) (commands.PushReport, error) {
	if err := e.run(ctx, "push", time.Duration(e.cfg.PushDelayMS)*time.Millisecond, false); err != nil {
		return commands.PushReport{}, err
	}
	return commands.PushReport{OK: true, Status: commands.PushStatusNoDrift, Secrets: []commands.SecretFinding{}, Errors: []string{}}, nil
}

func (e *processExecutor) Pull(ctx context.Context, _ bool, _ []string, _ bool) (commands.PullReport, error) {
	if err := e.run(ctx, "pull", time.Duration(e.cfg.PullDelayMS)*time.Millisecond, false); err != nil {
		return commands.PullReport{}, err
	}
	return commands.PullReport{OK: true, Status: commands.PullStatusNoDrift}, nil
}

func isReadActivity(method string) bool { return method == "status" || method == "diff" }

// routeProxy is a stable front listener used when a TCPProxy.Cut() is
// intentionally one-way. It can atomically direct subsequent reconnects to a
// live target while the already-established proxy connection is reset.
type routeProxy struct {
	listener   net.Listener
	mu         sync.RWMutex
	target     string
	conns      map[net.Conn]struct{}
	closed     chan struct{}
	acceptDone chan struct{}
	wg         sync.WaitGroup
	once       sync.Once
}

func newRouteProxy(target string) (*routeProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	proxy := &routeProxy{
		listener: listener, target: target, conns: make(map[net.Conn]struct{}),
		closed: make(chan struct{}), acceptDone: make(chan struct{}),
	}
	go proxy.acceptLoop()
	return proxy, nil
}

func (p *routeProxy) Addr() string { return p.listener.Addr().String() }

func (p *routeProxy) setTarget(target string) {
	p.mu.Lock()
	p.target = target
	p.mu.Unlock()
}

func (p *routeProxy) acceptLoop() {
	defer close(p.acceptDone)
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.RLock()
		target := p.target
		p.mu.RUnlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		upstream, dialErr := (&net.Dialer{}).DialContext(ctx, "tcp", target)
		cancel()
		if dialErr != nil {
			_ = client.Close()
			continue
		}
		select {
		case <-p.closed:
			_ = client.Close()
			_ = upstream.Close()
			return
		default:
		}
		p.mu.Lock()
		p.conns[client] = struct{}{}
		p.conns[upstream] = struct{}{}
		p.wg.Add(2)
		p.mu.Unlock()
		go p.forward(client, upstream)
		go p.forward(upstream, client)
	}
}

func (p *routeProxy) forward(source, destination net.Conn) {
	defer p.wg.Done()
	_, _ = io.Copy(destination, source)
	_ = source.Close()
	_ = destination.Close()
	p.mu.Lock()
	delete(p.conns, source)
	delete(p.conns, destination)
	p.mu.Unlock()
}

func (p *routeProxy) close() error {
	if p == nil {
		return nil
	}
	var closeErr error
	p.once.Do(func() {
		close(p.closed)
		closeErr = p.listener.Close()
	})
	select {
	case <-p.acceptDone:
	case <-time.After(2 * time.Second):
		return errors.New("route proxy accept loop did not stop")
	}
	p.mu.Lock()
	for conn := range p.conns {
		_ = conn.Close()
	}
	p.mu.Unlock()
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		return errors.New("route proxy forwarders did not stop")
	}
	if errors.Is(closeErr, net.ErrClosed) {
		return nil
	}
	return closeErr
}

func closeTCPProxy(t *testing.T, proxy *streamtest.TCPProxy) {
	t.Helper()
	if proxy == nil {
		return
	}
	done := make(chan error, 1)
	go func() { done <- proxy.CloseGraceful() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("close TCP proxy: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Errorf("TCP proxy %s did not close within timeout", proxy.Addr())
	}
}

func waitFor(t *testing.T, timeout time.Duration, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s after %s", description, timeout)
}

type httpResult struct {
	status int
	body   []byte
	err    error
}

func httpRequest(baseURL, token, method, path string, body io.Reader, timeout time.Duration) (int, []byte, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, body)
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil {
		return response.StatusCode, nil, err
	}
	if len(data) > 16<<20 {
		return response.StatusCode, nil, fmt.Errorf("HTTP response exceeded integration test read limit")
	}
	return response.StatusCode, data, nil
}

func apiJSON(t *testing.T, world *hubWorld, method, path string, body any, timeout time.Duration) httpResult {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode API request: %v", err)
		}
		reader = strings.NewReader(string(data))
	}
	status, response, err := httpRequest(world.url, world.token, method, path, reader, timeout)
	return httpResult{status: status, body: response, err: err}
}

func decodeObject(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode JSON %q: %v", truncate(string(data), 512), err)
	}
	return payload
}

func errorCode(t *testing.T, data []byte) string {
	t.Helper()
	payload := decodeObject(t, data)
	errorPayload, _ := payload["error"].(map[string]any)
	code, _ := errorPayload["code"].(string)
	return code
}

func waitAgents(t *testing.T, world *hubWorld, ids []string, online bool, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, fmt.Sprintf("%d agents online=%t", len(ids), online), func() bool {
		status, body, err := httpRequest(world.url, world.token, http.MethodGet, "/api/agents", nil, time.Second)
		if err != nil || status != http.StatusOK {
			return false
		}
		var payload struct {
			Agents []struct {
				AgentID string `json:"agentId"`
				Stale   bool   `json:"stale"`
			} `json:"agents"`
		}
		if json.Unmarshal(body, &payload) != nil {
			return false
		}
		states := make(map[string]bool, len(payload.Agents))
		for _, agent := range payload.Agents {
			states[agent.AgentID] = !agent.Stale
		}
		for _, id := range ids {
			if state, ok := states[id]; !ok || state != online {
				return false
			}
		}
		return true
	})
}

func waitAgent(t *testing.T, world *hubWorld, agentID string, online bool, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, fmt.Sprintf("agent %s online=%t", agentID, online), func() bool {
		status, body, err := httpRequest(world.url, world.token, http.MethodGet, "/api/agents", nil, time.Second)
		if err != nil || status != http.StatusOK {
			return false
		}
		var payload struct {
			Agents []struct {
				AgentID string `json:"agentId"`
				Stale   bool   `json:"stale"`
			} `json:"agents"`
		}
		if json.Unmarshal(body, &payload) != nil {
			return false
		}
		for _, agent := range payload.Agents {
			if agent.AgentID == agentID {
				return !agent.Stale == online
			}
		}
		return false
	})
}

func waitForFileEvent(t *testing.T, path, method, event string, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, fmt.Sprintf("agent event %s/%s in %s", method, event, path), func() bool {
		return hasFileEvent(path, method, event)
	})
}

func hasFileEvent(path, method, event string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		var item struct {
			Method string `json:"method"`
			Event  string `json:"event"`
		}
		if json.Unmarshal(scanner.Bytes(), &item) == nil && item.Method == method && item.Event == event {
			return true
		}
	}
	return false
}

func getJoinCode(t *testing.T, world *hubWorld) string {
	t.Helper()
	status, body, err := httpRequest(world.url, world.token, http.MethodGet, "/api/auth/join", nil, 3*time.Second)
	if err != nil || status != http.StatusOK {
		t.Fatalf("mint enrollment code: status=%d err=%v body=%s", status, err, truncate(string(body), 512))
	}
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &response); err != nil || !strings.HasPrefix(response.Code, "hr_") {
		t.Fatalf("join response has no enrollment code: %s err=%v", body, err)
	}
	return response.Code
}

func codeStillActive(t *testing.T, world *hubWorld, code string) bool {
	t.Helper()
	path := "/api/auth/join/status?code=" + code
	status, body, err := httpRequest(world.url, world.token, http.MethodGet, path, nil, 2*time.Second)
	if err != nil || status != http.StatusOK {
		t.Fatalf("enrollment status: status=%d err=%v body=%s", status, err, truncate(string(body), 512))
	}
	var response struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode enrollment status: %v (%s)", err, body)
	}
	return response.Active
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

func stopGracefully(t *testing.T, process *childProcess, timeout time.Duration) {
	t.Helper()
	if err := process.signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM pid %d: %v\n%s", process.pid(), err, process.output())
	}
	process.waitForExit(t, timeout)
}
