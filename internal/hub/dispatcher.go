package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/homer-cli/internal/keyring"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/toolctl"
	"github.com/zzjcool/homer-cli/internal/web"
)

const agentCallConcurrency = 64

// Dispatcher adapts stream-connected agents to the web-facing interfaces.
type Dispatcher struct {
	Registry *Registry
	Token    string
	Hub      *AgentHub

	limitsMu sync.Mutex
	limits   map[string]*agentCallLimit
}

type agentCallLimit struct {
	active chan struct{}
	refs   int
}

func NewDispatcher(reg *Registry, token string) *Dispatcher {
	return &Dispatcher{Registry: reg, Token: token, limits: make(map[string]*agentCallLimit)}
}

func (d *Dispatcher) ListAgents() []web.AgentInfo {
	if d == nil || d.Registry == nil {
		return []web.AgentInfo{}
	}
	registered := d.Registry.List()
	agents := make([]web.AgentInfo, 0, len(registered))
	for _, info := range registered {
		agent := web.AgentInfo{
			AgentID: info.AgentID, Hostname: info.Hostname, LastSeen: info.LastSeen,
			Version: info.Version, Stale: info.Stale,
		}
		if info.Drift != nil {
			agent.Drift = &web.AgentDrift{
				Push: info.Drift.Push, Pull: info.Drift.Pull,
				Conflicts: info.Drift.Conflicts, Error: info.Drift.Error,
			}
		}
		agent.Host = webHost(info.Host)
		agent.Tools = webTools(info.Tools)
		agents = append(agents, agent)
	}
	return agents
}

func webTools(tools []toolctl.Status) []web.AgentTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]web.AgentTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, web.AgentTool{Status: tool})
	}
	return out
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
	return d.callTask(ctx, agentID, TaskKindStatus, TaskOptions{})
}

func (d *Dispatcher) AgentDiff(ctx context.Context, agentID string, params web.DiffParams) (string, error) {
	raw, err := d.callTask(ctx, agentID, TaskKindDiff, TaskOptions{
		Adapter: params.Adapter, Category: params.Category, Path: params.Path,
	})
	if err != nil {
		return "", err
	}
	text, err := responseText(raw)
	if err != nil {
		return "", newAgentError("agent-unreachable", http.StatusBadGateway, err)
	}
	return text, nil
}

func (d *Dispatcher) AgentPush(ctx context.Context, agentID string, confirm bool, scope web.SyncScope) (json.RawMessage, error) {
	return d.writeAgent(ctx, agentID, TaskKindPush, taskOptionsForScope(confirm, scope))
}

func (d *Dispatcher) AgentPull(ctx context.Context, agentID string, confirm bool, scope web.SyncScope) (json.RawMessage, error) {
	return d.writeAgent(ctx, agentID, TaskKindPull, taskOptionsForScope(confirm, scope))
}

func taskOptionsForScope(confirm bool, scope web.SyncScope) TaskOptions {
	options := TaskOptions{Confirm: confirm, Overwrite: scope.Overwrite, AllowSecrets: scope.AllowSecrets}
	if scope.Explicit {
		options.Adapters = append([]string(nil), scope.Adapters...)
	}
	return options
}

func (d *Dispatcher) AgentInstallSSHKeys(ctx context.Context, agentID, githubUser string, keys []string) (json.RawMessage, error) {
	return d.callTask(ctx, agentID, TaskKindSSHKey, TaskOptions{
		Confirm: true, GitHubUser: githubUser, SSHKeys: append([]string(nil), keys...),
	})
}

func (d *Dispatcher) AgentUpgrade(ctx context.Context, agentID string) (json.RawMessage, error) {
	return d.callTask(ctx, agentID, TaskKindUpgrade, TaskOptions{Confirm: true})
}

func (d *Dispatcher) AgentToolUpgrade(ctx context.Context, agentID, tool string) (json.RawMessage, error) {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return nil, newAgentError("bad-request", http.StatusBadRequest, errors.New("tool is required"))
	}
	raw, err := d.callTask(ctx, agentID, TaskKindToolUpgrade, TaskOptions{Tool: tool})
	if err != nil {
		return nil, err
	}
	d.noteToolUpgrade(agentID, tool, raw)
	return raw, nil
}

// noteToolUpgrade shows the version the machine just measured without waiting
// for its next heartbeat.
func (d *Dispatcher) noteToolUpgrade(agentID, tool string, raw json.RawMessage) {
	if d == nil || d.Registry == nil || len(raw) == 0 {
		return
	}
	var report struct {
		After string `json:"after"`
	}
	if json.Unmarshal(raw, &report) != nil || report.After == "" {
		return
	}
	d.Registry.NoteToolVersion(agentID, tool, report.After)
}

// AgentKey runs a keyring command on one machine. The password remains inside
// this encrypted stream request and is not copied into the response.
func (d *Dispatcher) AgentKey(ctx context.Context, agentID string, cmd keyring.Command) (json.RawMessage, error) {
	payload, err := json.Marshal(cmd)
	if err != nil {
		return nil, newAgentError("bad-request", http.StatusBadRequest, err)
	}
	return d.callTask(ctx, agentID, TaskKindSecret, TaskOptions{
		SecretAction: cmd.Action, SecretPayload: payload,
	})
}

func (d *Dispatcher) AgentResolve(ctx context.Context, agentID, choice string) (json.RawMessage, error) {
	kind := TaskKindPull
	if choice == "local" {
		kind = TaskKindPush
	}
	return d.writeAgent(ctx, agentID, kind, TaskOptions{Confirm: true, Resolve: choice})
}

func (d *Dispatcher) writeAgent(ctx context.Context, agentID string, kind TaskKind, options TaskOptions) (json.RawMessage, error) {
	raw, err := d.callTask(ctx, agentID, kind, options)
	if err != nil {
		return nil, err
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

func (d *Dispatcher) AgentInspect(ctx context.Context, agentID string, params web.InspectParams, onEvent func(web.InspectEvent)) (web.InspectResult, error) {
	var progress func(json.RawMessage)
	if onEvent != nil {
		progress = func(payload json.RawMessage) {
			var event web.InspectEvent
			if err := json.Unmarshal(payload, &event); err != nil {
				return
			}
			onEvent(event)
		}
	}
	raw, err := d.call(ctx, agentID, MethodInspect, params, CallBudgetDefault, progress)
	if err != nil {
		return web.InspectResult{}, err
	}
	var result web.InspectResult
	if len(raw) == 0 || !json.Valid(raw) {
		return web.InspectResult{}, newAgentError("agent-unreachable", http.StatusBadGateway, errors.New("agent returned an empty or invalid inspect result"))
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return web.InspectResult{}, newAgentError("agent-unreachable", http.StatusBadGateway, err)
	}
	return result, nil
}

func (d *Dispatcher) callTask(ctx context.Context, agentID string, kind TaskKind, options TaskOptions) (json.RawMessage, error) {
	return d.call(ctx, agentID, string(kind), options, CallBudget(kind), nil)
}

// call is the single stream path for every machine RPC. It enforces liveness,
// method capabilities, an end-to-end budget and per-agent concurrency before
// multiplexing the request over the current session.
func (d *Dispatcher) call(ctx context.Context, agentID, method string, params any, budget time.Duration, progress func(json.RawMessage)) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, mapDispatcherError(agentID, err)
	}
	if budget <= 0 {
		budget = CallBudgetDefault
	}
	callCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	info, session, err := d.requireOnline(agentID)
	if err != nil {
		return nil, err
	}
	if err := d.acquireAgentCall(callCtx, agentID); err != nil {
		return nil, mapDispatcherError(agentID, err)
	}
	defer d.releaseAgentCall(agentID)

	info, session, err = d.requireOnline(agentID)
	if err != nil {
		return nil, err
	}
	if !hasCapability(info.caps, method) {
		return nil, mapDispatcherError(agentID, &stream.Error{
			Code: "unsupported", Message: fmt.Sprintf("agent %q does not advertise method %q", agentID, method),
		})
	}
	if err := callCtx.Err(); err != nil {
		return nil, mapDispatcherError(agentID, err)
	}
	select {
	case <-session.Done():
		return nil, mapDispatcherError(agentID, &stream.SessionClosedError{Cause: session.Err()})
	default:
	}

	deadline, ok := callCtx.Deadline()
	if !ok {
		return nil, mapDispatcherError(agentID, context.DeadlineExceeded)
	}
	remaining := time.Until(deadline)
	remoteBudget := remaining - ReqDeadlineSlack
	if remoteBudget <= 0 {
		return nil, mapDispatcherError(agentID, context.DeadlineExceeded)
	}

	options := []stream.CallOption{stream.WithBudget(remoteBudget)}
	if progress != nil {
		options = append(options, stream.WithProgress(progress))
	}
	raw, err := session.Call(callCtx, method, params, options...)
	if err != nil {
		return nil, mapDispatcherError(agentID, err)
	}
	return append(json.RawMessage(nil), raw...), nil
}

func (d *Dispatcher) requireOnline(agentID string) (AgentInfo, *stream.Session, error) {
	info, err := d.agentInfo(agentID)
	if err != nil {
		return AgentInfo{}, nil, err
	}
	session, ok := d.Registry.Session(agentID)
	if !ok || info.Stale {
		return AgentInfo{}, nil, newAgentError("agent-offline", http.StatusServiceUnavailable,
			fmt.Errorf("机器 %s 离线（最后心跳 %s 前）。请等它重新上线或先在机器上重启 agent", agentID, time.Since(info.LastSeen).Round(time.Second)))
	}
	return info, session, nil
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

func (d *Dispatcher) acquireAgentCall(ctx context.Context, agentID string) error {
	if d == nil {
		return errors.New("dispatcher is nil")
	}
	d.limitsMu.Lock()
	if d.limits == nil {
		d.limits = make(map[string]*agentCallLimit)
	}
	limit := d.limits[agentID]
	if limit == nil {
		limit = &agentCallLimit{active: make(chan struct{}, agentCallConcurrency)}
		d.limits[agentID] = limit
	}
	limit.refs++
	d.limitsMu.Unlock()

	select {
	case limit.active <- struct{}{}:
		return nil
	case <-ctx.Done():
		d.dropAgentCallRef(agentID, limit)
		return ctx.Err()
	}
}

func (d *Dispatcher) releaseAgentCall(agentID string) {
	d.limitsMu.Lock()
	limit := d.limits[agentID]
	if limit != nil {
		<-limit.active
		limit.refs--
		if limit.refs == 0 {
			delete(d.limits, agentID)
		}
	}
	d.limitsMu.Unlock()
}

func (d *Dispatcher) dropAgentCallRef(agentID string, limit *agentCallLimit) {
	d.limitsMu.Lock()
	defer d.limitsMu.Unlock()
	limit.refs--
	if limit.refs == 0 && d.limits[agentID] == limit {
		delete(d.limits, agentID)
	}
}

func hasCapability(caps []string, method string) bool {
	for _, capability := range caps {
		if capability == method {
			return true
		}
	}
	return false
}

func mapDispatcherError(agentID string, err error) error {
	if err == nil {
		return nil
	}
	var remoteErr *stream.Error
	if errors.As(err, &remoteErr) {
		message := remoteErr.Message
		if message == "" {
			message = remoteErr.Error()
		}
		switch remoteErr.Code {
		case stream.CodeBadRequest:
			return newAgentError("bad-request", http.StatusBadRequest, errors.New(message))
		case stream.CodeTimeout:
			return newAgentError("agent-timeout", http.StatusGatewayTimeout, errors.New(message))
		case stream.CodeExecFailed, stream.CodeInternal, stream.CodeUnknownMethod, stream.CodeOverloaded, "unsupported", stream.CodeUnsupported:
			return newAgentError("agent-unreachable", http.StatusBadGateway, errors.New(message))
		default:
			return newAgentError("agent-unreachable", http.StatusBadGateway, errors.New(message))
		}
	}
	var sessionClosed *stream.SessionClosedError
	if errors.As(err, &sessionClosed) {
		return newAgentError("agent-offline", http.StatusServiceUnavailable,
			fmt.Errorf("agent %q stream closed: %w", agentID, err))
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return newAgentError("agent-timeout", http.StatusGatewayTimeout, err)
	}
	return newAgentError("agent-unreachable", http.StatusBadGateway, err)
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

func newAgentError(code string, status int, err error) *web.AgentError {
	return &web.AgentError{Code: code, Status: status, Err: err}
}

// RemoveAgent removes the cached identity and actively closes its session so
// pending calls fail immediately instead of waiting for their budgets.
func (d *Dispatcher) RemoveAgent(agentID string) bool {
	if d == nil || d.Registry == nil {
		return false
	}
	if d.Hub != nil {
		d.Hub.Kick(agentID, stream.CloseRemoved, "agent removed from hub")
	}
	return d.Registry.Remove(agentID)
}

var _ web.AgentsSource = (*Dispatcher)(nil)
var _ web.InspectSource = (*Dispatcher)(nil)
