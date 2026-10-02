package hub

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const agentHeartbeatSeconds = 60

// AgentAPI implements the hub side of the HTTP long-polling protocol. It is
// intentionally a standalone handler so a hub can mount it below
// /agent/v1/ through web.ServeOptions.AgentEndpoint or use it directly in a
// test/server.
type AgentAPI struct {
	Registry   *Registry
	Token      string
	Enrollment *EnrollmentManager
}

func NewAgentAPI(reg *Registry, token string) *AgentAPI {
	return &AgentAPI{Registry: reg, Token: token, Enrollment: NewEnrollmentManager()}
}

// SetEnrollment wires a shared EnrollmentManager (hub process singleton).
func (a *AgentAPI) SetEnrollment(m *EnrollmentManager) *AgentAPI {
	a.Enrollment = m
	return a
}

func (a *AgentAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.Registry == nil {
		writeAgentError(w, http.StatusInternalServerError, "internal", "hub registry 未初始化")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/agent/v1")
	if path == r.URL.Path {
		// Accept the stripped form as well. This makes the handler usable both
		// directly and behind http.StripPrefix without changing the wire path.
		path = strings.TrimPrefix(path, "/")
		path = "/" + path
	}
	path = strings.TrimSuffix(path, "/")

	// Enrollment is the only endpoint reachable with an enrollment code
	// instead of a live credential.
	if path == "/enroll" {
		if r.Method != http.MethodPost {
			writeAgentError(w, http.StatusMethodNotAllowed, "method-not-allowed", "不支持的请求方法")
			return
		}
		a.handleEnroll(w, r)
		return
	}

	if !a.authorized(r) {
		writeAgentError(w, http.StatusUnauthorized, "unauthorized", "未授权：请提供有效的 Bearer token")
		return
	}
	if r.Method != http.MethodPost {
		writeAgentError(w, http.StatusMethodNotAllowed, "method-not-allowed", "不支持的请求方法")
		return
	}

	switch path {
	case "/register":
		a.handleRegister(w, r)
	case "/poll":
		a.handlePoll(w, r)
	case "/report":
		a.handleReport(w, r)
	default:
		writeAgentError(w, http.StatusNotFound, "not-found", "请求的资源不存在")
	}
}

// Authorized is the exported machine-credential check the web layer
// consults for its /agent/v1/* gate: per-agent enrollment secrets (the
// normal path) or the management hub token (migration window for
// agents that predate per-agent credentials).
func (a *AgentAPI) Authorized(r *http.Request) bool { return a.authorized(r) }

// authorized accepts either a per-agent enrollment secret (the normal
// path) or the management hub token (migration window for agents that
// predate per-agent credentials).
func bearerToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(value, prefix))
}

func (a *AgentAPI) authorized(r *http.Request) bool {
	if a.Token == "" && a.Enrollment == nil {
		return true
	}
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	bearer := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	if a.Enrollment != nil && a.Enrollment.AuthorizedAgent(bearer) != "" {
		return true
	}
	return a.Token != "" && bearer == a.Token
}

type registerRequest struct {
	AgentID  string        `json:"agentId"`
	Hostname string        `json:"hostname"`
	Mode     AgentMode     `json:"mode"`
	Addr     string        `json:"addr"`
	Version  string        `json:"version"`
	Drift    *AgentDrift   `json:"drift,omitempty"`
	Host     *HostSnapshot `json:"host,omitempty"`
}

// enrollRequest is the Tailscale-style onboarding payload: an agent
// presents a one-time enrollment code and its identity, and receives a
// per-agent secret that replaces the shared hub token from then on.
type enrollRequest struct {
	Code     string        `json:"code"`
	AgentID  string        `json:"agentId"`
	Hostname string        `json:"hostname"`
	Mode     AgentMode     `json:"mode"`
	Addr     string        `json:"addr"`
	Version  string        `json:"version"`
	Drift    *AgentDrift   `json:"drift,omitempty"`
	Host     *HostSnapshot `json:"host,omitempty"`
}

func (a *AgentAPI) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var request enrollRequest
	if err := decodeAgentJSON(r, &request); err != nil {
		writeAgentError(w, http.StatusBadRequest, "bad-request", "请求参数无效")
		return
	}
	if a.Enrollment == nil {
		writeAgentError(w, http.StatusInternalServerError, "enroll-disabled", "接入未启用（hub 版本不匹配）")
		return
	}
	if strings.TrimSpace(request.AgentID) == "" {
		writeAgentError(w, http.StatusBadRequest, "bad-request", "agentId 不能为空")
		return
	}
	secret, err := a.Enrollment.Redeem(strings.TrimSpace(request.Code))
	if err != nil {
		writeAgentError(w, http.StatusForbidden, "enroll-rejected", err.Error())
		return
	}
	// Bind then register: the agent's first authenticated call may race
	// with this response, so the binding must exist before we answer.
	a.Enrollment.BindAgent(request.AgentID, secret)
	if err := a.Registry.Register(AgentInfo{
		AgentID:  request.AgentID,
		Hostname: request.Hostname,
		Mode:     request.Mode,
		Addr:     request.Addr,
		Version:  request.Version,
		Host:     request.Host,
	}); err != nil {
		// Registration validates mode/addr shape; the code stays burned —
		// a malformed agent should not get a second try with the same code.
		writeAgentError(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	a.Registry.Touch(request.AgentID)
	a.Registry.SetDialSecret(request.AgentID, secret)
	if request.Drift != nil {
		a.Registry.UpdateDrift(request.AgentID, *request.Drift)
	}
	writeAgentJSON(w, http.StatusOK, struct {
		OK               bool   `json:"ok"`
		AgentSecret      string `json:"agentSecret"`
		HeartbeatSeconds int    `json:"heartbeatSeconds"`
	}{true, secret, agentHeartbeatSeconds})
}

func (a *AgentAPI) handleRegister(w http.ResponseWriter, r *http.Request) {
	var request registerRequest
	if err := decodeAgentJSON(r, &request); err != nil {
		writeAgentError(w, http.StatusBadRequest, "bad-request", "请求参数无效")
		return
	}
	if err := a.Registry.Register(AgentInfo{
		AgentID:  request.AgentID,
		Hostname: request.Hostname,
		Mode:     request.Mode,
		Addr:     request.Addr,
		Version:  request.Version,
		Host:     request.Host,
	}); err != nil {
		writeAgentError(w, http.StatusBadRequest, "bad-request", "请求参数无效")
		return
	}
	// Register sets LastSeen for new entries, and Touch makes the heartbeat
	// semantics explicit for an upsert as well.
	a.Registry.Touch(request.AgentID)
	if secret := bearerToken(r); a.Enrollment != nil && a.Enrollment.VerifySecret(request.AgentID, secret) {
		a.Registry.SetDialSecret(request.AgentID, secret)
	}
	if request.Drift != nil {
		a.Registry.UpdateDrift(request.AgentID, *request.Drift)
	}
	writeAgentJSON(w, http.StatusOK, struct {
		OK               bool `json:"ok"`
		HeartbeatSeconds int  `json:"heartbeatSeconds"`
	}{true, agentHeartbeatSeconds})
}

type pollRequest struct {
	AgentID     string        `json:"agentId"`
	WaitSeconds *int          `json:"waitSeconds"`
	Drift       *AgentDrift   `json:"drift,omitempty"`
	Host        *HostSnapshot `json:"host,omitempty"`
}

type pollResponse struct {
	OK   bool  `json:"ok"`
	Task *Task `json:"task"`
}

func (a *AgentAPI) handlePoll(w http.ResponseWriter, r *http.Request) {
	var request pollRequest
	if err := decodeAgentJSON(r, &request); err != nil || strings.TrimSpace(request.AgentID) == "" {
		writeAgentError(w, http.StatusBadRequest, "bad-request", "请求参数无效")
		return
	}
	if _, ok := a.Registry.Get(request.AgentID); !ok {
		writeAgentError(w, http.StatusNotFound, "agent-not-found", "agent 不存在")
		return
	}
	waitSeconds := agentDefaultPollWaitSeconds
	if request.WaitSeconds != nil {
		waitSeconds = *request.WaitSeconds
	}
	// The ceiling keeps a hostile or buggy poller from parking its handler
	// far beyond the server WriteTimeout (90s), which would otherwise turn a
	// legitimate long poll into an opaque connection reset.
	if waitSeconds < 0 || waitSeconds > agentMaxPollWaitSeconds {
		writeAgentError(w, http.StatusBadRequest, "bad-request", "请求参数无效")
		return
	}

	if request.Drift != nil {
		a.Registry.UpdateDrift(request.AgentID, *request.Drift)
	}
	if request.Host != nil {
		a.Registry.UpdateHost(request.AgentID, *request.Host)
	}
	task, ok := a.Registry.Poll(request.AgentID, time.Duration(waitSeconds)*time.Second, r.Context())
	if r.Context().Err() != nil {
		// The client disconnected or the server is shutting down. There is no
		// useful response left to write, and Registry.Poll has already stopped.
		return
	}
	response := pollResponse{OK: true}
	if ok {
		response.Task = &task
	}
	writeAgentJSON(w, http.StatusOK, response)
}

const agentDefaultPollWaitSeconds = 25

// agentMaxPollWaitSeconds bounds waitSeconds from clients. The hub-side web
// server writes with a 90s timeout, so a poll asking to wait longer than
// that would only end in an opaque connection reset.
const agentMaxPollWaitSeconds = 80

type reportRequest struct {
	AgentID string          `json:"agentId"`
	TaskID  string          `json:"taskId"`
	OK      bool            `json:"ok"`
	Report  json.RawMessage `json:"report"`
	Error   string          `json:"error"`
}

func (a *AgentAPI) handleReport(w http.ResponseWriter, r *http.Request) {
	var request reportRequest
	if err := decodeAgentJSON(r, &request); err != nil || strings.TrimSpace(request.AgentID) == "" || strings.TrimSpace(request.TaskID) == "" {
		writeAgentError(w, http.StatusBadRequest, "bad-request", "请求参数无效")
		return
	}
	if _, ok := a.Registry.Get(request.AgentID); !ok {
		writeAgentError(w, http.StatusNotFound, "agent-not-found", "agent 不存在")
		return
	}
	task, taskAgent, ok := a.registryTask(request.TaskID)
	if !ok || taskAgent != request.AgentID {
		writeAgentError(w, http.StatusNotFound, "task-not-found", "task 不存在或已失效")
		return
	}
	if err := a.Registry.Submit(TaskResult{
		TaskID:  request.TaskID,
		AgentID: request.AgentID,
		OK:      request.OK,
		Kind:    task.Kind,
		Report:  append(json.RawMessage(nil), request.Report...),
		Error:   request.Error,
	}); err != nil {
		writeAgentError(w, http.StatusNotFound, "task-not-found", "task 不存在或已失效")
		return
	}
	a.Registry.Touch(request.AgentID)
	writeAgentJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{true})
}

func (a *AgentAPI) registryTask(taskID string) (Task, string, bool) {
	return a.Registry.taskInfo(taskID)
}

func decodeAgentJSON(r *http.Request, value any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

type agentErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type agentErrorEnvelope struct {
	Error agentErrorBody `json:"error"`
}

func writeAgentError(w http.ResponseWriter, status int, code, message string) {
	writeAgentJSON(w, status, agentErrorEnvelope{Error: agentErrorBody{Code: code, Message: message}})
}

func writeAgentJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte(`{"error":{"code":"internal","message":"内部错误"}}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// Compile-time check keeps accidental changes from turning this into a plain
// helper that no longer satisfies the mounting contract.
var _ http.Handler = (*AgentAPI)(nil)
