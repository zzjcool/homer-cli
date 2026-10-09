package agentd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/wsconn"
	"github.com/zzjcool/homer-cli/internal/web"
)

type Config struct {
	HomerHome, Home, Token, AgentSecret, EnrollCode string
	HubURL                                          string
	DataURL                                         string
	AgentID                                         string
	TaskTimeout                                     time.Duration
	Backoff                                         stream.Backoff
	Stream                                          stream.Options
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

	driftMu     sync.Mutex
	lastDrift   *hub.AgentDrift
	lastDriftAt time.Time

	hosts       hostCollector
	hostMu      sync.RWMutex
	lastHost    *hub.HostSnapshot
	lastHostAt  time.Time
	hostProbing bool

	execMu        sync.RWMutex
	reexecMu      sync.Mutex
	reexecPending bool
	authMu        sync.RWMutex
	agentSecret   string
	enrollCode    string

	toolkit Toolkit
	tools   toolState

	retries *stream.ThrottledLogger
	logger  stream.Logger

	statusFlight statusFlight
	readSem      chan struct{}
	writeGate    *taskGate

	heartbeatWake  chan struct{}
	heartbeatEvery time.Duration

	dialStream streamDialer
	retryWait  func(context.Context, time.Duration) bool
	reexec     func()
}

func New(cfg Config, exec Executor) *Daemon {
	if strings.TrimSpace(cfg.AgentID) == "" {
		cfg.AgentID = DefaultAgentID()
	}
	if cfg.TaskTimeout <= 0 {
		// Keep ordinary task reports ahead of the hub's 60 second budget.
		cfg.TaskTimeout = 50 * time.Second
	}
	if cfg.Backoff == (stream.Backoff{}) {
		cfg.Backoff = stream.Backoff{Base: time.Second, Max: time.Minute, Factor: 2, Jitter: 0.5}
	}
	logger := cfg.Stream.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "homer agent: ", log.LstdFlags)
		cfg.Stream.Logger = logger
	}
	if exec == nil {
		dataURL := strings.TrimSpace(cfg.DataURL)
		if dataURL == "" {
			dataURL = strings.TrimSpace(cfg.HubURL)
		}
		exec = NewLocalExecutorWithHub(cfg.HomerHome, dataURL, configuredCredential(cfg))
	}
	d := &Daemon{
		cfg:            cfg,
		exec:           exec,
		agentSecret:    strings.TrimSpace(cfg.AgentSecret),
		enrollCode:     strings.TrimSpace(cfg.EnrollCode),
		logger:         logger,
		retries:        stream.NewThrottledLoggerWithClock(logger, time.Minute, cfg.Stream.Clock),
		readSem:        make(chan struct{}, 4),
		writeGate:      newTaskGate(),
		heartbeatWake:  make(chan struct{}, 1),
		heartbeatEvery: DriftInterval,
		dialStream:     wsconn.Dial,
		retryWait:      sleepContext,
		reexec:         reexecAgent,
	}
	// The executor's existing constructor accepts one data-plane URL. Keep
	// the constructor signature stable and use Config's explicit override,
	// falling back to the control-plane URL as specified by the protocol.
	if local, ok := exec.(*localExecutor); ok {
		dataURL := strings.TrimSpace(cfg.DataURL)
		if dataURL == "" {
			dataURL = strings.TrimSpace(cfg.HubURL)
		}
		if dataURL != "" {
			local.hubURL = strings.TrimRight(dataURL, "/")
		}
		if credential := configuredCredential(cfg); credential != "" {
			local.credential = credential
		}
	}
	return d
}

func configuredCredential(cfg Config) string {
	if secret := strings.TrimSpace(cfg.AgentSecret); secret != "" {
		return secret
	}
	return strings.TrimSpace(cfg.Token)
}

func (d *Daemon) bearerCredential() string {
	if d == nil {
		return ""
	}
	d.authMu.RLock()
	secret := d.agentSecret
	d.authMu.RUnlock()
	if secret != "" {
		return secret
	}
	return strings.TrimSpace(d.cfg.Token)
}

// streamCredential allows an enrollment code only on the control-plane
// WebSocket handshake. Data-plane /api/snapshot and /dl requests never reuse
// a one-time code; they require the returned per-agent secret or a hub token.
func (d *Daemon) streamCredential() string {
	if d == nil {
		return ""
	}
	d.authMu.RLock()
	enrollCode := d.enrollCode
	d.authMu.RUnlock()
	if enrollCode != "" {
		// An explicit one-time credential starts a new enrollment, even if an
		// older per-agent secret is still present in agent.json.
		return enrollCode
	}
	return d.bearerCredential()
}

func (d *Daemon) setAgentSecret(secret string) {
	if d == nil {
		return
	}
	d.authMu.Lock()
	d.agentSecret = strings.TrimSpace(secret)
	d.enrollCode = ""
	d.authMu.Unlock()
}

func (d *Daemon) hasEnrollCode() bool {
	if d == nil {
		return false
	}
	d.authMu.RLock()
	defer d.authMu.RUnlock()
	return d.enrollCode != ""
}

func (d *Daemon) persistedAgentSecret() string {
	if d == nil {
		return ""
	}
	d.authMu.RLock()
	defer d.authMu.RUnlock()
	return d.agentSecret
}

// DriftInterval controls the cached status/drift refresh frequency.
const DriftInterval = 30 * time.Second

// DriftTimeout bounds a scan shared by status requests, inspect, and heartbeat.
const DriftTimeout = 20 * time.Second

func (d *Daemon) driftSummary(ctx context.Context) *hub.AgentDrift {
	if d == nil {
		return nil
	}
	now := time.Now()
	d.driftMu.Lock()
	if d.lastDrift != nil && now.Sub(d.lastDriftAt) < DriftInterval {
		cached := cloneDrift(d.lastDrift)
		d.driftMu.Unlock()
		return cached
	}
	d.driftMu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	summaryCtx, cancel := context.WithTimeout(ctx, DriftTimeout)
	defer cancel()
	report, err := d.statusReport(summaryCtx)
	if err != nil {
		d.driftMu.Lock()
		cached := cloneDrift(d.lastDrift)
		d.driftMu.Unlock()
		if cached != nil {
			return cached
		}
		return &hub.AgentDrift{Error: err.Error()}
	}
	return driftFromStatus(report)
}

func driftFromStatus(report commands.StatusReport) *hub.AgentDrift {
	drift := &hub.AgentDrift{}
	for _, adapter := range report.Adapters {
		drift.Push += adapter.Push
		drift.Pull += adapter.Pull
		drift.Conflicts += adapter.Conflicts
	}
	// Keep the fresh-machine marker used by the console's "新机器 · 等待下发" badge.
	for _, message := range report.Errors {
		if strings.Contains(message, "未找到 homer 配置") {
			drift.Error = message
			break
		}
	}
	return drift
}

func (d *Daemon) forgetDrift() {
	if d == nil {
		return
	}
	d.driftMu.Lock()
	d.lastDrift = nil
	d.lastDriftAt = time.Time{}
	d.driftMu.Unlock()
}

func (d *Daemon) setDrift(drift *hub.AgentDrift) {
	if d == nil || drift == nil {
		return
	}
	d.driftMu.Lock()
	d.lastDrift = cloneDrift(drift)
	d.lastDriftAt = time.Now()
	d.driftMu.Unlock()
}

func (d *Daemon) cachedDrift() *hub.AgentDrift {
	if d == nil {
		return nil
	}
	d.driftMu.Lock()
	defer d.driftMu.Unlock()
	return cloneDrift(d.lastDrift)
}

func cloneDrift(drift *hub.AgentDrift) *hub.AgentDrift {
	if drift == nil {
		return nil
	}
	copy := *drift
	return &copy
}

func (d *Daemon) hostSnapshot() *hub.HostSnapshot {
	if d == nil {
		return nil
	}
	snapshot := d.hosts.snapshot()
	d.setHost(snapshot)
	return snapshot
}

func (d *Daemon) setHost(snapshot *hub.HostSnapshot) {
	if d == nil {
		return
	}
	d.hostMu.Lock()
	if snapshot == nil {
		d.lastHost = nil
	} else {
		copy := *snapshot
		d.lastHost = &copy
		d.lastHostAt = time.Now()
	}
	d.hostMu.Unlock()
}

func (d *Daemon) cachedHost() *hub.HostSnapshot {
	if d == nil {
		return nil
	}
	d.hostMu.RLock()
	defer d.hostMu.RUnlock()
	if d.lastHost == nil {
		return nil
	}
	copy := *d.lastHost
	return &copy
}

func (d *Daemon) signalHeartbeat() {
	if d == nil || d.heartbeatWake == nil {
		return
	}
	select {
	case d.heartbeatWake <- struct{}{}:
	default:
	}
}

func (d *Daemon) logf(format string, args ...any) {
	if d == nil || d.logger == nil {
		return
	}
	d.logger.Printf(format, args...)
}

// syncExecutorCredential updates the snapshot/data-plane bearer once the
// hello response exchanges an enrollment code for the agent secret.
func (d *Daemon) syncExecutorCredential() {
	if d == nil || d.exec == nil {
		return
	}
	credential := d.bearerCredential()
	if setter, ok := d.exec.(interface{ SetHubCredential(string) }); ok && credential != "" {
		d.execMu.Lock()
		setter.SetHubCredential(credential)
		d.execMu.Unlock()
	}
}

func (d *Daemon) executorStatus(ctx context.Context) (commands.StatusReport, error) {
	d.execMu.RLock()
	defer d.execMu.RUnlock()
	return d.exec.Status(ctx)
}

func (d *Daemon) executorDiff(ctx context.Context, params web.DiffParams) (string, error) {
	d.execMu.RLock()
	defer d.execMu.RUnlock()
	return d.exec.Diff(ctx, params)
}

func (d *Daemon) executorPush(ctx context.Context, confirm bool, adapters []string, overwrite, allowSecrets bool) (commands.PushReport, error) {
	d.execMu.RLock()
	defer d.execMu.RUnlock()
	return d.exec.Push(ctx, confirm, adapters, overwrite, allowSecrets)
}

func (d *Daemon) executorPull(ctx context.Context, confirm bool, adapters []string, preferRemote bool) (commands.PullReport, error) {
	d.execMu.RLock()
	defer d.execMu.RUnlock()
	return d.exec.Pull(ctx, confirm, adapters, preferRemote)
}

func (d *Daemon) executorResolve(ctx context.Context, choice string) (commands.MergeReport, error) {
	d.execMu.RLock()
	defer d.execMu.RUnlock()
	local, ok := d.exec.(*localExecutor)
	if !ok {
		return commands.MergeReport{}, fmt.Errorf("该 executor 不能在机器上裁决冲突")
	}
	return local.Resolve(ctx, choice)
}
