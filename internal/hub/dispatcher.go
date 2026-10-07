package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zzjcool/homer-cli/internal/web"
)

const dispatcherTimeout = 60 * time.Second

var dispatcherTaskSequence uint64

// Dispatcher adapts Registry agents to web.AgentsSource. Listen agents are
// reached directly over HTTP, while connect agents receive tasks through the
// Registry queue and return their result through Registry.Wait.
type Dispatcher struct {
	Registry *Registry
	Client   *http.Client
	Token    string
}

// NewDispatcher creates a dispatcher with the frozen 60-second HTTP/Wait
// timeout. Tests and embedders may replace Client after construction when they
// need a custom transport.
func NewDispatcher(reg *Registry, token string) *Dispatcher {
	return &Dispatcher{
		Registry: reg,
		Client:   &http.Client{Timeout: dispatcherTimeout},
		Token:    token,
	}
}

// ListAgents returns the Registry view in the shape consumed by internal/web.
// Registry.List already computes staleness without removing an agent; web's
// AgentInfo deliberately has no stale bit because the UI derives that from
// LastSeen.
func (d *Dispatcher) ListAgents() []web.AgentInfo {
	if d == nil || d.Registry == nil {
		return []web.AgentInfo{}
	}
	registered := d.Registry.List()
	agents := make([]web.AgentInfo, 0, len(registered))
	for _, info := range registered {
		agent := web.AgentInfo{
			AgentID:  info.AgentID,
			Hostname: info.Hostname,
			Mode:     string(info.Mode),
			Addr:     info.Addr,
			LastSeen: info.LastSeen,
			Version:  info.Version,
			Stale:    info.Stale,
		}
		if info.Drift != nil {
			agent.Drift = &web.AgentDrift{
				Push:      info.Drift.Push,
				Pull:      info.Drift.Pull,
				Conflicts: info.Drift.Conflicts,
				Error:     info.Drift.Error,
			}
		}
		agent.Host = webHost(info.Host)
		agents = append(agents, agent)
	}
	return agents
}

func webHost(host *HostSnapshot) *web.HostSnapshot {
	if host == nil {
		return nil
	}
	raw, err := json.Marshal(host)
	if err != nil {
		return nil
	}
	var out web.HostSnapshot
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return &out
}

func (d *Dispatcher) AgentStatus(ctx context.Context, agentID string) (json.RawMessage, error) {
	if err := d.requireOnline(agentID); err != nil {
		return nil, err
	}
	info, err := d.agentInfo(agentID)
	if err != nil {
		return nil, err
	}
	switch info.Mode {
	case AgentModeListen:
		body, err := d.direct(ctx, info, http.MethodGet, "status", nil, nil, false)
		if err != nil {
			return nil, d.directError(err)
		}
		report, err := statusReport(body)
		if err != nil {
			return nil, d.directError(err)
		}
		return report, nil
	case AgentModeConnect:
		return d.enqueueAndWait(ctx, info.AgentID, TaskKindStatus, TaskOptions{})
	default:
		return nil, newAgentError("agent-unreachable", http.StatusBadGateway, fmt.Errorf("agent %q has invalid mode %q", agentID, info.Mode))
	}
}

func (d *Dispatcher) AgentDiff(ctx context.Context, agentID string, params web.DiffParams) (string, error) {
	info, err := d.agentInfo(agentID)
	if err != nil {
		return "", err
	}
	options := TaskOptions{Adapter: params.Adapter, Category: params.Category}
	switch info.Mode {
	case AgentModeListen:
		query := url.Values{}
		query.Set("adapter", params.Adapter)
		query.Set("category", params.Category)
		body, err := d.direct(ctx, info, http.MethodGet, "diff", query, nil, false)
		if err != nil {
			return "", d.directError(err)
		}
		text, err := responseText(body)
		if err != nil {
			return "", d.directError(err)
		}
		return text, nil
	case AgentModeConnect:
		body, err := d.enqueueAndWait(ctx, info.AgentID, TaskKindDiff, options)
		if err != nil {
			return "", err
		}
		text, err := responseText(body)
		if err != nil {
			return "", newAgentError("agent-unreachable", http.StatusBadGateway, err)
		}
		return text, nil
	default:
		return "", newAgentError("agent-unreachable", http.StatusBadGateway, fmt.Errorf("agent %q has invalid mode %q", agentID, info.Mode))
	}
}

func (d *Dispatcher) AgentPush(ctx context.Context, agentID string, confirm bool, scope web.SyncScope) (json.RawMessage, error) {
	return d.writeAgent(ctx, agentID, TaskKindPush, taskOptionsForScope(confirm, scope))
}

func (d *Dispatcher) AgentPull(ctx context.Context, agentID string, confirm bool, scope web.SyncScope) (json.RawMessage, error) {
	return d.writeAgent(ctx, agentID, TaskKindPull, taskOptionsForScope(confirm, scope))
}

func taskOptionsForScope(confirm bool, scope web.SyncScope) TaskOptions {
	options := TaskOptions{Confirm: confirm, Overwrite: scope.Overwrite}
	if scope.Explicit {
		options.Adapters = append([]string(nil), scope.Adapters...)
	}
	return options
}

// AgentInstallSSHKeys asks a machine to append keys to the agent user's
// authorized_keys. Connect-mode machines receive a task; listen-mode
// machines are dialed directly. The keys were already fetched by the hub.
func (d *Dispatcher) AgentInstallSSHKeys(ctx context.Context, agentID, githubUser string, keys []string) (json.RawMessage, error) {
	info, err := d.agentInfo(agentID)
	if err != nil {
		return nil, err
	}
	if err := d.requireOnline(agentID); err != nil {
		return nil, err
	}
	options := TaskOptions{Confirm: true, GitHubUser: githubUser, SSHKeys: append([]string(nil), keys...)}
	if info.Mode == AgentModeListen {
		payload, err := json.Marshal(options)
		if err != nil {
			return nil, err
		}
		body, err := d.direct(ctx, info, http.MethodPost, "ssh-key", nil, payload, true)
		if err != nil {
			return nil, d.directError(err)
		}
		return body, nil
	}
	return d.enqueueAndWait(ctx, info.AgentID, TaskKindSSHKey, options)
}

// AgentResolve asks one machine to apply a conflict choice through merge.
// Scoped console buttons use AgentPush/AgentPull with an explicit selection
// instead, so this path stays available for a whole-machine choice.
func (d *Dispatcher) AgentResolve(ctx context.Context, agentID, choice string) (json.RawMessage, error) {
	kind := TaskKindPull
	if choice == "local" {
		kind = TaskKindPush
	}
	return d.writeAgent(ctx, agentID, kind, TaskOptions{Confirm: true, Resolve: choice})
}

func (d *Dispatcher) writeAgent(ctx context.Context, agentID string, kind TaskKind, options TaskOptions) (json.RawMessage, error) {
	info, err := d.agentInfo(agentID)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	switch info.Mode {
	case AgentModeListen:
		query := url.Values{}
		query.Set("confirm", strconv.FormatBool(options.Confirm))
		if len(options.Adapters) > 0 {
			query.Set("adapters", strings.Join(options.Adapters, ","))
		}
		if options.Overwrite {
			query.Set("overwrite", "true")
		}
		if options.Resolve != "" {
			query.Set("resolve", options.Resolve)
		}
		path := "push"
		if kind == TaskKindPull {
			path = "pull"
		}
		body, err := d.direct(ctx, info, http.MethodPost, path, query, nil, true)
		if err != nil {
			return nil, d.directError(err)
		}
		raw = body
	case AgentModeConnect:
		body, err := d.enqueueAndWait(ctx, info.AgentID, kind, options)
		if err != nil {
			return nil, err
		}
		raw = body
	default:
		return nil, newAgentError("agent-unreachable", http.StatusBadGateway, fmt.Errorf("agent %q has invalid mode %q", agentID, info.Mode))
	}
	d.noteWriteOutcome(agentID, raw)
	return raw, nil
}

func (d *Dispatcher) noteWriteOutcome(agentID string, raw json.RawMessage) {
	if d == nil || d.Registry == nil || len(raw) == 0 {
		return
	}
	var report struct {
		OK        bool              `json:"ok"`
		Status    string            `json:"status"`
		Conflicts []json.RawMessage `json:"conflicts"`
	}
	if json.Unmarshal(raw, &report) != nil {
		return
	}
	d.Registry.NoteWriteOutcome(agentID, report.Status, report.OK, len(report.Conflicts))
}

func (d *Dispatcher) agentInfo(agentID string) (AgentInfo, error) {
	if d == nil || d.Registry == nil {
		return AgentInfo{}, newAgentError("agent-not-found", http.StatusNotFound, fmt.Errorf("agent %q is not registered", agentID))
	}
	info, ok := d.Registry.Get(agentID)
	if !ok {
		return AgentInfo{}, newAgentError("agent-not-found", http.StatusNotFound, fmt.Errorf("agent %q is not registered", agentID))
	}
	return info, nil
}

// requireOnline fails fast for stale machines. A task enqueued for a
// disconnected agent burns the full dispatcherTimeout (60s) before
// 504ing — the user clicked one button and waited a minute for
// nothing. Connectivity tasks (AgentStatus) must refuse immediately.
func (d *Dispatcher) requireOnline(agentID string) error {
	info, err := d.agentInfo(agentID)
	if err != nil {
		return err
	}
	if info.Stale {
		return newAgentError("agent-offline", http.StatusServiceUnavailable,
			fmt.Errorf("机器 %s 离线（最后心跳 %s 前）。请等它重新上线或先在机器上重启 agent", agentID, time.Since(info.LastSeen).Round(time.Second)))
	}
	return nil
}

func (d *Dispatcher) enqueueAndWait(ctx context.Context, agentID string, kind TaskKind, options TaskOptions) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, newAgentError("agent-timeout", http.StatusGatewayTimeout, err)
	}
	if d == nil || d.Registry == nil {
		return nil, newAgentError("agent-not-found", http.StatusNotFound, fmt.Errorf("agent %q is not registered", agentID))
	}
	task := Task{
		TaskID:    nextDispatcherTaskID(),
		Kind:      kind,
		Options:   options,
		CreatedAt: time.Now(),
	}
	if err := d.Registry.Enqueue(agentID, task); err != nil {
		// The agent was looked up immediately before this call. A failed
		// enqueue therefore means that the queue cannot accept work (or the
		// registry was concurrently replaced), rather than a missing agent.
		return nil, newAgentError("agent-unreachable", http.StatusBadGateway, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, dispatcherTimeout)
	defer cancel()
	result, err := d.Registry.Wait(waitCtx, task.TaskID)
	if err != nil {
		return nil, newAgentError("agent-timeout", http.StatusGatewayTimeout, err)
	}
	if len(result.Report) == 0 || !json.Valid(result.Report) {
		if result.Error != "" {
			return nil, newAgentError("agent-unreachable", http.StatusBadGateway, errors.New(result.Error))
		}
		return nil, newAgentError("agent-unreachable", http.StatusBadGateway, errors.New("agent returned an empty report"))
	}
	return append(json.RawMessage(nil), result.Report...), nil
}

func (d *Dispatcher) direct(ctx context.Context, info AgentInfo, method string, endpoint string, query url.Values, payload []byte, writeOperation bool) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	endpointURL, err := agentEndpointURL(info.Addr, endpoint, query)
	if err != nil {
		return nil, err
	}
	var bodyReader io.Reader
	if len(payload) > 0 {
		bodyReader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpointURL, bodyReader)
	if err != nil {
		return nil, err
	}
	if len(payload) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	token := ""
	if d != nil {
		token = d.Token
	}
	if info.DialSecret != "" {
		token = info.DialSecret
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := d.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, readErr
	}
	accepted := resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
	if writeOperation && (resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusUnprocessableEntity) {
		accepted = true
	}
	if !accepted {
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		return nil, fmt.Errorf("agent returned %s: %s", resp.Status, message)
	}
	if len(body) == 0 || !json.Valid(body) {
		return nil, errors.New("agent returned invalid JSON")
	}
	return body, nil
}

func (d *Dispatcher) httpClient() *http.Client {
	if d != nil && d.Client != nil {
		return d.Client
	}
	return &http.Client{Timeout: dispatcherTimeout}
}

func (d *Dispatcher) directError(err error) error {
	if timeoutLike(err) {
		return newAgentError("agent-timeout", http.StatusGatewayTimeout, err)
	}
	return newAgentError("agent-unreachable", http.StatusBadGateway, err)
}

func statusReport(body []byte) (json.RawMessage, error) {
	var envelope struct {
		Report json.RawMessage `json:"report"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Report) > 0 && string(envelope.Report) != "null" {
		if !json.Valid(envelope.Report) {
			return nil, errors.New("agent status report is invalid JSON")
		}
		return append(json.RawMessage(nil), envelope.Report...), nil
	}
	return append(json.RawMessage(nil), body...), nil
}

func responseText(body []byte) (string, error) {
	var response struct {
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("agent response is invalid JSON: %w", err)
	}
	if response.Text == nil {
		return "", errors.New("agent response has no text")
	}
	return *response.Text, nil
}

func agentEndpointURL(addr, endpoint string, query url.Values) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("agent address is empty")
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("invalid agent address: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid agent address %q", addr)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/" + strings.TrimLeft(endpoint, "/")
	u.RawPath = ""
	if query != nil {
		u.RawQuery = query.Encode()
	} else {
		u.RawQuery = ""
	}
	u.Fragment = ""
	return u.String(), nil
}

func timeoutLike(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func newAgentError(code string, status int, err error) *web.AgentError {
	return &web.AgentError{Code: code, Status: status, Err: err}
}

func nextDispatcherTaskID() string {
	return fmt.Sprintf("t-%06d", atomic.AddUint64(&dispatcherTaskSequence, 1))
}

// RemoveAgent drops a machine from the registry (console "移除" button).
func (d *Dispatcher) RemoveAgent(agentID string) bool {
	return d.Registry.Remove(agentID)
}

var _ web.AgentsSource = (*Dispatcher)(nil)
