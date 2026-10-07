package agentd

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/keyring"
	"github.com/zzjcool/homer-cli/internal/sshkey"
	"github.com/zzjcool/homer-cli/internal/web"
)

type Config struct {
	HomerHome     string
	Token         string
	ListenAddr    string
	AdvertiseURL  string
	ConnectURL    string
	HubURL        string
	AgentID       string
	PollInterval  time.Duration
	PollWait      time.Duration
	ReportTimeout time.Duration
	Home          string
	// AgentSecret: persisted per-agent credential (agent.json) that
	// supersedes the shared token once this machine has enrolled.
	AgentSecret string
	// EnrollCode: one-time code from the install command; consumed at
	// first successful registration, never persisted.
	EnrollCode string
}

func (c Config) Mode() (hub.AgentMode, error) {
	listen := strings.TrimSpace(c.ListenAddr) != ""
	connect := strings.TrimSpace(c.ConnectURL) != ""
	if listen == connect {
		return "", errors.New("agent 必须恰好指定 --listen 或 --connect")
	}
	if listen {
		return hub.AgentModeListen, nil
	}
	return hub.AgentModeConnect, nil
}

var agentIDInvalidChar = regexp.MustCompile(`[^a-z0-9-]+`)
var agentIDSequence uint32

func DefaultAgentID() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		hostname = "agent"
	}
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	hostname = agentIDInvalidChar.ReplaceAllString(hostname, "-")
	hostname = strings.Trim(hostname, "-")
	if hostname == "" {
		hostname = "agent"
	}
	if hostname[0] < 'a' || hostname[0] > 'z' {
		if hostname[0] < '0' || hostname[0] > '9' {
			hostname = "agent-" + hostname
		}
	}
	var suffix [2]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		fallback := atomic.AddUint32(&agentIDSequence, 1)
		suffix[0] = byte(fallback >> 8)
		suffix[1] = byte(fallback)
	}
	return hostname + "-" + hex.EncodeToString(suffix[:])
}

type Daemon struct {
	cfg  Config
	exec Executor
	// drift caching (throttled status summaries uploaded with polls)
	driftMu     sync.Mutex
	lastDrift   *hub.AgentDrift
	lastDriftAt time.Time
	// hosts keeps the previous CPU sample for utilization deltas.
	hosts hostCollector
	// retries logs one line per failing key per minute — the silent-401
	// lesson: a doomed retry loop must be visible in the log, never spam.
	retries *retryLogger
	// dialSecret is the per-agent bearer this listen server accepts when
	// the hub dials back. It is published after enrollment.
	dialSecret atomic.Value
}

func New(cfg Config, exec Executor) *Daemon {
	if strings.TrimSpace(cfg.AgentID) == "" {
		cfg.AgentID = DefaultAgentID()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.PollWait <= 0 {
		cfg.PollWait = 25 * time.Second
	}
	if cfg.ReportTimeout <= 0 {
		// Deliberately below the hub-side dispatcherTimeout (60s) and web
		// WriteTimeout (90s): an agent execution must finish and report
		// before the hub gives up waiting, otherwise a task burns the full
		// window only to deliver its result into a closed connection.
		cfg.ReportTimeout = 50 * time.Second
	}
	if exec == nil {
		exec = NewLocalExecutor(cfg.HomerHome)
	}
	return &Daemon{cfg: cfg, exec: exec, retries: newRetryLogger(time.Minute)}
}

func (d *Daemon) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if d == nil {
		return errors.New("nil agent daemon")
	}
	mode, err := d.cfg.Mode()
	if err != nil {
		return err
	}
	if d.exec == nil {
		return errors.New("agent executor is required")
	}
	switch mode {
	case hub.AgentModeListen:
		return d.runListen(ctx)
	case hub.AgentModeConnect:
		return d.runConnect(ctx)
	default:
		return fmt.Errorf("unsupported agent mode %q", mode)
	}
}

func (d *Daemon) runListen(ctx context.Context) error {
	if strings.TrimSpace(d.cfg.AdvertiseURL) == "" {
		advertiseURL, err := DeriveAdvertiseURL(d.cfg.ListenAddr)
		if err != nil {
			return err
		}
		d.cfg.AdvertiseURL = advertiseURL
	}
	if secret := strings.TrimSpace(d.cfg.AgentSecret); secret != "" {
		d.dialSecret.Store(secret)
	}
	hostname := localHostname()
	identity := &web.AgentIdentity{
		AgentID:  d.cfg.AgentID,
		Hostname: hostname,
		Version:  web.Version,
	}
	webServer, err := web.NewServer(web.ServeOptions{
		Addr:      d.cfg.ListenAddr,
		HomerHome: d.cfg.HomerHome,
		Token:     d.cfg.Token,
		Identity:  identity,
		SyncDeps:  d.syncDeps(),
		// The hub dials this port with the per-agent secret from enrollment,
		// which does not exist yet when the listener starts.
		AgentEndpointAuthorized: d.listenAuthorized,
		LocalResolve:            d.resolveLocal,
		AfterLocalWrite:         d.forgetDrift,
	})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", d.cfg.ListenAddr)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Handler:           webServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}
	serveDone := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveDone <- err
	}()

	registerCtx, stopRegister := context.WithCancel(ctx)
	var registerWG sync.WaitGroup
	if strings.TrimSpace(d.cfg.HubURL) != "" {
		registerWG.Add(1)
		go func() {
			defer registerWG.Done()
			d.listenRegisterLoop(registerCtx, hostname)
		}()
	}

	select {
	case <-ctx.Done():
		stopRegister()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		cancel()
		registerWG.Wait()
		if shutdownErr != nil {
			return shutdownErr
		}
		return nil
	case err := <-serveDone:
		stopRegister()
		registerWG.Wait()
		return err
	}
}

func (d *Daemon) listenRegisterLoop(ctx context.Context, hostname string) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := d.register(ctx, hub.AgentModeListen, hostname); err != nil {
			if !sleepContext(ctx, backoff) {
				return
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}
		backoff = time.Second
		ticker := time.NewTicker(60 * time.Second)
		select {
		case <-ctx.Done():
			ticker.Stop()
			return
		case <-ticker.C:
			ticker.Stop()
		}
	}
}

func (d *Daemon) runConnect(ctx context.Context) error {
	hostname := localHostname()
	if err := d.registerWithRetry(ctx, hub.AgentModeConnect, hostname); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	client := d.agentHTTPClient()
	for {
		if ctx.Err() != nil {
			return nil
		}
		task, ok, err := d.poll(ctx, client)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// The registry is in-memory on the hub side: a hub restart makes
			// every poll 404 until this agent re-registers. Recover instead
			// of retrying a doomed poll forever.
			if IsAgentNotFound(err) {
				if regErr := d.registerWithRetry(ctx, hub.AgentModeConnect, localHostname()); regErr != nil {
					if !sleepContext(ctx, d.cfg.PollInterval) {
						return nil
					}
				}
				continue
			}
			// A transient hub/network error should not terminate a long-running
			// daemon. Keep the retry bounded by PollInterval — but never
			// silently: the silent-401 lesson says a doomed loop must be
			// visible in the log (one line per minute).
			d.retries.log("poll-failed", "poll 失败（重试中）: "+err.Error())
			if !sleepContext(ctx, d.cfg.PollInterval) {
				return nil
			}
			continue
		}
		if !ok {
			if !sleepContext(ctx, d.cfg.PollInterval) {
				return nil
			}
			continue
		}
		result := d.execute(ctx, task)
		if ctx.Err() != nil {
			return nil
		}
		if err := d.report(ctx, client, task, result); err != nil && ctx.Err() != nil {
			return nil
		}
		if !sleepContext(ctx, d.cfg.PollInterval) {
			return nil
		}
	}
}

// DriftInterval throttles the drift-summary computation: one full local
// status per interval is plenty for a console that refreshes every 30s.
const DriftInterval = 30 * time.Second

// DriftTimeout bounds a single drift computation so a hung executor can
// never stall the poll loop.
const DriftTimeout = 20 * time.Second

// driftSummary computes this machine's status summary for the hub's
// machine list. It is throttled (one full status per DriftInterval) and
// bounded (a hung executor never blocks the poll loop for longer than
// DriftTimeout); a failure reuses the previous summary or reports the
// error, and the poll itself never waits on it.
func (d *Daemon) driftSummary(ctx context.Context) *hub.AgentDrift {
	now := time.Now()
	d.driftMu.Lock()
	if d.lastDrift != nil && now.Sub(d.lastDriftAt) < DriftInterval {
		cached := d.lastDrift
		d.driftMu.Unlock()
		return cached
	}
	d.driftMu.Unlock()

	summaryCtx, cancel := context.WithTimeout(ctx, DriftTimeout)
	defer cancel()
	report, err := d.exec.Status(summaryCtx)
	drift := &hub.AgentDrift{}
	if err != nil {
		// Keep the last known summary when it exists; otherwise surface
		// the error in place of numbers.
		d.driftMu.Lock()
		if d.lastDrift != nil {
			cached := d.lastDrift
			d.driftMu.Unlock()
			return cached
		}
		d.driftMu.Unlock()
		return &hub.AgentDrift{Error: err.Error()}
	}
	for _, adapter := range report.Adapters {
		drift.Push += adapter.Push
		drift.Pull += adapter.Pull
		drift.Conflicts += adapter.Conflicts
	}
	// Fresh-machine marker (fallback scan carries the "no homer.json
	// yet" line in Errors) must surface in the drift Error — the
	// console keys its "新机器 · 等待下发" badge on it, and a fresh
	// machine must not read as "↑N 项未收取" before a baseline exists.
	for _, message := range report.Errors {
		if strings.Contains(message, "未找到 homer 配置") {
			drift.Error = message
			break
		}
	}
	d.driftMu.Lock()
	d.lastDrift = drift
	d.lastDriftAt = now
	d.driftMu.Unlock()
	return drift
}

// forgetDrift drops the throttled summary. Call it after a local push,
// pull, or conflict choice so the next heartbeat describes the machine
// as it is now.
func (d *Daemon) forgetDrift() {
	if d == nil {
		return
	}
	d.driftMu.Lock()
	d.lastDrift = nil
	d.lastDriftAt = time.Time{}
	d.driftMu.Unlock()
}

func (d *Daemon) hostSnapshot() *hub.HostSnapshot {
	if d == nil {
		return nil
	}
	return d.hosts.snapshot()
}

func (d *Daemon) registerWithRetry(ctx context.Context, mode hub.AgentMode, hostname string) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := d.register(ctx, mode, hostname); err == nil {
			return nil
		} else {
			d.retries.log("register-failed", "注册失败（重试中）: "+err.Error())
		}
		if !sleepContext(ctx, backoff) {
			return ctx.Err()
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (d *Daemon) register(ctx context.Context, mode hub.AgentMode, hostname string) error {
	payload := struct {
		AgentID  string            `json:"agentId"`
		Hostname string            `json:"hostname"`
		Mode     hub.AgentMode     `json:"mode"`
		Addr     string            `json:"addr,omitempty"`
		Version  string            `json:"version,omitempty"`
		Code     string            `json:"code,omitempty"`
		Drift    *hub.AgentDrift   `json:"drift,omitempty"`
		Host     *hub.HostSnapshot `json:"host,omitempty"`
	}{
		AgentID:  d.cfg.AgentID,
		Hostname: hostname,
		Mode:     mode,
		Version:  web.Version,
		Code:     d.cfg.EnrollCode,
		Host:     d.hostSnapshot(),
	}
	if mode == hub.AgentModeListen {
		payload.Drift = d.driftSummary(ctx)
	}
	if mode == hub.AgentModeListen {
		if strings.TrimSpace(d.cfg.AdvertiseURL) == "" {
			advertiseURL, err := DeriveAdvertiseURL(d.cfg.ListenAddr)
			if err != nil {
				return err
			}
			d.cfg.AdvertiseURL = advertiseURL
		}
		payload.Addr = d.cfg.AdvertiseURL
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	client := d.agentHTTPClient()
	// With an enrollment code, hit /enroll: the hub binds a fresh
	// per-agent secret to this agentID and registers it in one step. The
	// code is one-shot; on success it is dropped from memory for good.
	if strings.TrimSpace(d.cfg.EnrollCode) != "" {
		err := d.redeemEnrollment(ctx, client, mode, body)
		if err == nil {
			return nil
		}
		// keys/hub-token keeps the one-time code after a successful enroll.
		// The next start presents that burned code and would retry forever,
		// even though agent.json already holds the live secret. Fall
		// through and register with the secret.
		if strings.TrimSpace(d.cfg.AgentSecret) == "" {
			return err
		}
		d.cfg.EnrollCode = ""
	}
	// Plain registration (already enrolled, or legacy hub-token agents).
	if _, err := d.postJSONWithClient(ctx, client, d.endpointURL("register"), body); err != nil {
		return err
	}
	d.syncExecutorCredential()
	if err := d.saveAgentConfig(string(mode)); err != nil {
		return err
	}
	return nil
}

func (d *Daemon) redeemEnrollment(ctx context.Context, client *http.Client, mode hub.AgentMode, body []byte) error {
	responseBody, err := d.postJSONWithClient(ctx, client, d.endpointURL("enroll"), body)
	if err != nil {
		return err
	}
	var enrolled struct {
		OK          bool   `json:"ok"`
		AgentSecret string `json:"agentSecret"`
		Error       *struct {
			Code    string   `json:"code"`
			Message string   `json:"message"`
			Details []string `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(responseBody, &enrolled); err != nil {
		return err
	}
	if enrolled.Error != nil {
		return fmt.Errorf("接入失败: %s", enrolled.Error.Message)
	}
	if !enrolled.OK || enrolled.AgentSecret == "" {
		return fmt.Errorf("接入失败: hub 未返回 agent 密钥")
	}
	d.cfg.AgentSecret = enrolled.AgentSecret
	d.cfg.EnrollCode = ""
	d.syncExecutorCredential()
	return d.saveAgentConfig(string(mode))
}

// syncExecutorCredential copies the live bearer onto the task executor.
// Enrollment learns the per-agent secret only after the executor has
// already been constructed (often with an empty credential, because the
// one-time code is not a snapshot bearer). Poll uses cfg.AgentSecret
// directly and stays healthy; push/pull go through the executor and would
// otherwise keep calling /api/snapshot unauthenticated.
func (d *Daemon) syncExecutorCredential() {
	if d == nil || d.exec == nil {
		return
	}
	credential := strings.TrimSpace(d.cfg.AgentSecret)
	if credential == "" {
		credential = strings.TrimSpace(d.cfg.Token)
	}
	if secret := strings.TrimSpace(d.cfg.AgentSecret); secret != "" {
		d.dialSecret.Store(secret)
	}
	if credential == "" {
		return
	}
	setter, ok := d.exec.(interface{ SetHubCredential(string) })
	if !ok {
		return
	}
	setter.SetHubCredential(credential)
}

func (d *Daemon) poll(ctx context.Context, client *http.Client) (hub.Task, bool, error) {
	waitSeconds := durationSeconds(d.cfg.PollWait)
	payload := struct {
		AgentID     string            `json:"agentId"`
		WaitSeconds int               `json:"waitSeconds"`
		Drift       *hub.AgentDrift   `json:"drift,omitempty"`
		Host        *hub.HostSnapshot `json:"host,omitempty"`
	}{AgentID: d.cfg.AgentID, WaitSeconds: waitSeconds, Drift: d.driftSummary(ctx), Host: d.hostSnapshot()}
	body, err := json.Marshal(payload)
	if err != nil {
		return hub.Task{}, false, err
	}
	responseBody, err := d.postJSONWithClient(ctx, client, d.endpointURL("poll"), body)
	if err != nil {
		return hub.Task{}, false, err
	}
	var response struct {
		OK    bool                   `json:"ok"`
		Task  *hub.Task              `json:"task"`
		Error *agentAPIErrorEnvelope `json:"error"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return hub.Task{}, false, err
	}
	if response.Error != nil {
		return hub.Task{}, false, response.Error.AgentError()
	}
	if response.Task == nil {
		return hub.Task{}, false, nil
	}
	return *response.Task, true, nil
}

func (d *Daemon) execute(parent context.Context, task hub.Task) hub.TaskResult {
	ctx, cancel := context.WithTimeout(parent, d.cfg.ReportTimeout)
	defer cancel()
	// Note: exceeding the timeout reports an error but does NOT abort the
	// underlying execution — the command layer is not context-aware yet, so
	// a timed-out push may still commit locally in the background. The hub
	// treats the task as failed; see plan risk R2 for the ctx-aware follow-up.
	result := hub.TaskResult{TaskID: task.TaskID, AgentID: d.cfg.AgentID, Kind: task.Kind}
	type executionResult struct {
		report any
		err    error
	}
	completed := make(chan executionResult, 1)
	go func() {
		var output any
		var err error
		if task.Options.Resolve == "local" || task.Options.Resolve == "center" {
			output, err = resolveOnExecutor(ctx, d.exec, task.Options.Resolve)
			completed <- executionResult{report: output, err: err}
			return
		}
		switch task.Kind {
		case hub.TaskKindStatus:
			output, err = d.exec.Status(ctx)
		case hub.TaskKindDiff:
			output, err = d.exec.Diff(ctx, web.DiffParams{Adapter: task.Options.Adapter, Category: task.Options.Category})
			if err == nil {
				output = struct {
					Text string `json:"text"`
				}{output.(string)}
			}
		case hub.TaskKindPush:
			output, err = d.exec.Push(ctx, task.Options.Confirm, task.Options.Adapters, task.Options.Overwrite)
		case hub.TaskKindPull:
			output, err = d.exec.Pull(ctx, task.Options.Confirm, task.Options.Adapters, task.Options.Overwrite)
		case hub.TaskKindSSHKey:
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				output = sshkey.Report{GitHubUser: task.Options.GitHubUser, Errors: []string{"无法确定用户主目录: " + homeErr.Error()}}
				break
			}
			output = sshkey.Install(home, task.Options.GitHubUser, task.Options.SSHKeys)
		case hub.TaskKindSecret:
			var command keyring.Command
			if err := json.Unmarshal(task.Options.SecretPayload, &command); err != nil {
				output = keyring.Result{Status: "bad-action", Errors: []string{"密钥请求不是合法 JSON"}}
				break
			}
			output = keyring.Apply(d.cfg.HomerHome, command)
		default:
			err = fmt.Errorf("unsupported task kind %q", task.Kind)
		}
		completed <- executionResult{report: output, err: err}
	}()
	select {
	case output := <-completed:
		if task.Kind == hub.TaskKindPush || task.Kind == hub.TaskKindPull {
			// The write changed this machine. Drop the throttled summary
			// so the next poll does not put the pre-write badge back.
			d.forgetDrift()
		}
		if output.err != nil {
			result.Error = output.err.Error()
			return result
		}
		encoded, err := json.Marshal(output.report)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		if task.Kind == hub.TaskKindSecret {
			var probe struct {
				OK bool `json:"ok"`
			}
			_ = json.Unmarshal(encoded, &probe)
			result.OK = probe.OK
			result.Report = encoded
			return result
		}
		switch report := output.report.(type) {
		case commands.MergeReport:
			if task.Options.Resolve != "" {
				result.OK = report.OK
			} else {
				result.OK = taskResultOK(task.Kind, output.report)
			}
		case sshkey.Report:
			result.OK = report.OK
		default:
			result.OK = taskResultOK(task.Kind, output.report)
		}
		result.Report = encoded
		return result
	case <-ctx.Done():
		result.Error = fmt.Sprintf("task execution timeout: %v", ctx.Err())
		return result
	}
}

func (d *Daemon) report(ctx context.Context, client *http.Client, task hub.Task, result hub.TaskResult) error {
	payload := struct {
		AgentID string          `json:"agentId"`
		TaskID  string          `json:"taskId"`
		OK      bool            `json:"ok"`
		Report  json.RawMessage `json:"report,omitempty"`
		Error   string          `json:"error,omitempty"`
	}{
		AgentID: d.cfg.AgentID,
		TaskID:  result.TaskID,
		OK:      result.OK,
		Report:  result.Report,
		Error:   result.Error,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	responseBody, err := d.postJSONWithClient(ctx, client, d.endpointURL("report"), body)
	if err != nil {
		return err
	}
	var response struct {
		OK    bool                   `json:"ok"`
		Error *agentAPIErrorEnvelope `json:"error"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return err
	}
	if response.Error != nil {
		return response.Error.AgentError()
	}
	if !response.OK {
		return errors.New("hub rejected agent report")
	}
	return nil
}

func (d *Daemon) postJSONWithClient(ctx context.Context, client *http.Client, endpoint string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	// Per-agent secret (enrolled machines) supersedes the shared hub token.
	bearer := d.cfg.AgentSecret
	if bearer == "" {
		bearer = d.cfg.Token
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var apiErr agentAPIErrorEnvelope
		if json.Unmarshal(responseBody, &apiErr) == nil && apiErr.Error != nil && apiErr.Error.Message != "" {
			return nil, apiErr.AgentError()
		}
		return nil, fmt.Errorf("hub returned %s", response.Status)
	}
	return responseBody, nil
}

// syncDeps exposes the executor's hub transport for the listen agent's
// own web endpoints (/api/push, /api/pull).
func (d *Daemon) syncDeps() web.SyncDepsSource {
	if d == nil || d.exec == nil {
		return nil
	}
	executor, ok := d.exec.(*localExecutor)
	if !ok {
		return nil
	}
	return agentSyncDeps{executor: executor}
}

func (d *Daemon) endpointURL(operation string) string {
	base := strings.TrimRight(strings.TrimSpace(d.cfg.ConnectURL), "/")
	if base == "" {
		base = strings.TrimRight(strings.TrimSpace(d.cfg.HubURL), "/")
	}
	return base + "/agent/v1/" + strings.TrimLeft(operation, "/")
}

func (d *Daemon) agentHTTPClient() *http.Client {
	timeout := 60 * time.Second
	if d.cfg.PollWait+5*time.Second > timeout {
		timeout = d.cfg.PollWait + 5*time.Second
	}
	return &http.Client{Timeout: timeout}
}

type agentAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type agentAPIErrorEnvelope struct {
	Error *agentAPIError `json:"error"`
}

// IsAgentNotFound reports whether err is the hub's 404 "agent not found"
// response, which means the hub lost this agent's registration (typically a
// hub restart: the registry is in-memory) and the agent must re-register.
func IsAgentNotFound(err error) bool {
	apiErr, ok := err.(*agentAPIError)
	return ok && apiErr != nil && apiErr.Code == "agent-not-found"
}

func (e *agentAPIError) Error() string {
	if e == nil {
		return "hub request failed"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return e.Code
	}
	return "hub request failed"
}

func (e *agentAPIErrorEnvelope) AgentError() error {
	if e == nil || e.Error == nil {
		return errors.New("hub request failed")
	}
	return e.Error
}

func durationSeconds(value time.Duration) int {
	if value <= 0 {
		return 0
	}
	// The wire protocol carries whole seconds. For a deliberately short test
	// or embedding interval, zero is preferable to rounding up to a full
	// second: the hub then performs an immediate poll and the caller's
	// PollInterval controls the cadence.
	return int(value / time.Second)
}

func (d *Daemon) listenAuthorized(r *http.Request) bool {
	want, _ := d.dialSecret.Load().(string)
	if want == "" {
		return false
	}
	got := bearerFromRequest(r)
	if got == "" || len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func bearerFromRequest(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(value, prefix))
}

func (d *Daemon) resolveLocal(ctx context.Context, choice string) (json.RawMessage, error) {
	report, err := resolveOnExecutor(ctx, d.exec, choice)
	if err != nil {
		return nil, err
	}
	return json.Marshal(report)
}

func resolveOnExecutor(ctx context.Context, exec Executor, choice string) (commands.MergeReport, error) {
	local, ok := exec.(*localExecutor)
	if !ok {
		return commands.MergeReport{}, fmt.Errorf("该 executor 不能在机器上裁决冲突")
	}
	return local.Resolve(ctx, choice)
}

func taskResultOK(kind hub.TaskKind, report any) bool {
	switch kind {
	case hub.TaskKindPush:
		return report.(commands.PushReport).OK
	case hub.TaskKindPull:
		return report.(commands.PullReport).OK
	default:
		return true
	}
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func localHostname() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "localhost"
	}
	return hostname
}

// retryLogger throttles repeated failure logs: one line per key per
// window. The lesson from the silent 401 death loop — an agent retrying
// a doomed poll forever must SAY so in its log, but never spam it.
type retryLogger struct {
	mu     sync.Mutex
	window time.Duration
	at     map[string]time.Time
}

func newRetryLogger(window time.Duration) *retryLogger {
	return &retryLogger{window: window, at: map[string]time.Time{}}
}

// log reports whether the line was emitted (true) or suppressed by the
// window (false).
func (l *retryLogger) log(key, line string) bool {
	now := time.Now()
	l.mu.Lock()
	if last, ok := l.at[key]; ok && now.Sub(last) < l.window {
		l.mu.Unlock()
		return false
	}
	l.at[key] = now
	l.mu.Unlock()
	fmt.Fprintf(os.Stderr, "homer agent: %s\n", line)
	return true
}
