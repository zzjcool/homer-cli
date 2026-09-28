package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/" {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		s.serveIndex(w)
		return
	}
	if strings.HasPrefix(path, "/static/") {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		s.serveStatic(w, path)
		return
	}
	if path == "/api/health" {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		s.handleHealth(w)
		return
	}
	if strings.HasPrefix(path, "/api/auth/") {
		s.handleAuthAPI(w, r, path)
		return
	}
	// Tailscale-style bootstrap endpoints. /install.sh is public — the
	// script never embeds the token (it arrives as the --token argument on
	// the agent machine). /dl/homer streams the hub's own binary and needs
	// the hub token like every other API.
	if path == "/install.sh" {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		s.serveInstallScript(w, r)
		return
	}
	if path == "/dl/homer" {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		if !s.requireAuth(w, r) {
			return
		}
		s.serveSelfBinary(w, r)
		return
	}
	if strings.HasPrefix(path, "/api/") || path == "/api" || (strings.HasPrefix(path, "/agent/") && path != "/agent/v1/enroll") {
		// /agent/v1/enroll carries its own one-time-code credential —
		// the whole point is that the machine has nothing else yet.
		if !s.requireAuth(w, r) {
			return
		}
	}

	switch {
	case path == "/api/status":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		s.handleStatus(w, r)
	case path == "/api/diff":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		s.handleDiff(w, r)
	case path == "/api/push":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		s.handlePush(w, r)
	case path == "/api/pull":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		s.handlePull(w, r)
	case path == "/api/sync":
		// Manual sync (MVP): one action, human sentences only.
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		s.handleSync(w, r)
	case path == "/api/config":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		s.handleConfig(w, r)
	case path == "/api/agents":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		s.handleAgents(w, r)
	case path == "/api/agents/revoke":
		// Tailscale-style machine revocation: drops the per-agent secret.
		// The machine's next poll 401s immediately; siblings unaffected.
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		s.handleAgentRevoke(w, r)
	case path == "/api/agents/remove":
		// Machine removal: drops the registry entry entirely (vs revoke
		// which only kills the credential and keeps the row for audit).
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		s.handleAgentRemove(w, r)
	case path == "/agent/v1/info":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		s.handleAgentInfo(w, r)
	case strings.HasPrefix(path, "/api/agents/"):
		s.handleAgentRoute(w, r)
	case strings.HasPrefix(path, "/agent/v1/"):
		s.handleAgentEndpoint(w, r)
	default:
		writeError(w, http.StatusNotFound, "not-found", "请求的资源不存在", nil)
	}
}

func (s *Server) serveIndex(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(staticIndex)
}

func (s *Server) serveStatic(w http.ResponseWriter, path string) {
	// There is deliberately only one embedded resource. Do not use filepath or
	// a filesystem handler here: accepting an arbitrary path would turn a
	// harmless static endpoint into a path traversal boundary.
	if path != "/static/index.html" {
		writeError(w, http.StatusNotFound, "not-found", "请求的资源不存在", nil)
		return
	}
	s.serveIndex(w)
}

func (s *Server) handleHealth(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"status":    "healthy",
		"version":   Version,
		"homerHome": s.homePath(),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	report, err := commands.RunStatus(commands.StatusOptions{HomerHome: s.opts.HomerHome, JSON: true})
	if err != nil {
		writeCommandError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK     bool                  `json:"ok"`
		Report commands.StatusReport `json:"report"`
	}{true, report})
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	text, err := commands.RunDiff(commands.DiffOptions{
		HomerHome: s.opts.HomerHome,
		Adapter:   query.Get("adapter"),
		Category:  query.Get("category"),
	})
	if err != nil {
		writeCommandError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK    bool   `json:"ok"`
		Empty bool   `json:"empty"`
		Text  string `json:"text"`
	}{true, text == "", text})
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	writeMutex.Lock()
	defer writeMutex.Unlock()
	report := commands.RunPush(commands.PushOptions{
		HomerHome: s.opts.HomerHome,
		Yes:       confirmValue(r),
	}, &commands.PushDeps{UI: commands.HeadlessUI{}})
	writeWriteReport(w, report.OK, string(report.Status), report)
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	writeMutex.Lock()
	defer writeMutex.Unlock()
	report := commands.RunPull(commands.PullOptions{
		HomerHome: s.opts.HomerHome,
		Yes:       confirmValue(r),
	}, &commands.PullDeps{UI: commands.HeadlessUI{}})
	writeWriteReport(w, report.OK, string(report.Status), report)
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	paths := s.paths()
	if _, err := core.LoadConfig(paths); err != nil {
		writeConfigError(w, err)
		return
	}
	data, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	value, err := orderedjson.Parse(data)
	if err != nil {
		writeError(w, http.StatusConflict, "invalid-config", "homer.json 配置无效", []string{err.Error()})
		return
	}
	object, ok := value.(*orderedjson.Object)
	if !ok || object == nil {
		writeError(w, http.StatusConflict, "invalid-config", "homer.json 配置无效", []string{"配置顶层必须是对象"})
		return
	}
	response := &orderedjson.Object{
		Keys: []string{"ok", "config"},
		M: map[string]orderedjson.Value{
			"ok":     true,
			"config": object,
		},
	}
	writeJSONBytes(w, http.StatusOK, orderedjson.Serialize(response))
}

func (s *Server) handleAgents(w http.ResponseWriter, _ *http.Request) {
	agents := []AgentInfo{}
	if s.opts.Agents != nil {
		agents = append(agents, s.opts.Agents.ListAgents()...)
	}
	writeJSON(w, http.StatusOK, struct {
		OK     bool        `json:"ok"`
		Agents []AgentInfo `json:"agents"`
	}{true, agents})
}

func (s *Server) handleAgentInfo(w http.ResponseWriter, _ *http.Request) {
	if s.opts.Identity == nil {
		writeError(w, http.StatusNotFound, "not-found", "请求的资源不存在", nil)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK       bool           `json:"ok"`
		Identity *AgentIdentity `json:"identity"`
	}{true, s.opts.Identity})
}

func (s *Server) handleAgentEndpoint(w http.ResponseWriter, r *http.Request) {
	if s.opts.AgentEndpoint == nil {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "多机 agent 接口未启用", nil)
		return
	}
	s.opts.AgentEndpoint.ServeHTTP(w, r)
}

func (s *Server) handleAgentRoute(w http.ResponseWriter, r *http.Request) {
	if s.opts.Agents == nil {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "多机 agent 视图未启用", nil)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/agents/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeError(w, http.StatusNotFound, "not-found", "请求的资源不存在", nil)
		return
	}
	agentID, operation := parts[0], parts[1]
	switch operation {
	case "status":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		s.handleAgentStatus(w, r, agentID)
	case "diff":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		s.handleAgentDiff(w, r, agentID)
	case "push":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		writeMutex.Lock()
		defer writeMutex.Unlock()
		s.handleAgentPush(w, r, agentID)
	case "pull":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		writeMutex.Lock()
		defer writeMutex.Unlock()
		s.handleAgentPull(w, r, agentID)
	default:
		writeError(w, http.StatusNotFound, "not-found", "请求的资源不存在", nil)
	}
}

func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request, agentID string) {
	raw, err := s.opts.Agents.AgentStatus(r.Context(), agentID)
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	writeRawReportEnvelope(w, http.StatusOK, raw)
}

func (s *Server) handleAgentDiff(w http.ResponseWriter, r *http.Request, agentID string) {
	query := r.URL.Query()
	text, err := s.opts.Agents.AgentDiff(r.Context(), agentID, DiffParams{Adapter: query.Get("adapter"), Category: query.Get("category")})
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK   bool   `json:"ok"`
		Text string `json:"text"`
	}{true, text})
}

func (s *Server) handleAgentPush(w http.ResponseWriter, r *http.Request, agentID string) {
	raw, err := s.opts.Agents.AgentPush(r.Context(), agentID, confirmValue(r))
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	writeRemoteWriteReport(w, raw)
}

func (s *Server) handleAgentPull(w http.ResponseWriter, r *http.Request, agentID string) {
	raw, err := s.opts.Agents.AgentPull(r.Context(), agentID, confirmValue(r))
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	writeRemoteWriteReport(w, raw)
}

func writeWriteReport(w http.ResponseWriter, ok bool, status string, report any) {
	statusCode := reportHTTPStatus(ok, status, reportErrors(report))
	writeJSON(w, statusCode, report)
}

func reportHTTPStatus(ok bool, status string, messages []string) int {
	if ok {
		return http.StatusOK
	}
	if status == "aborted" || hasConfigError(messages) {
		return http.StatusConflict
	}
	return http.StatusUnprocessableEntity
}

func reportErrors(report any) []string {
	switch typed := report.(type) {
	case commands.PushReport:
		return typed.Errors
	case commands.PullReport:
		return typed.Errors
	default:
		return nil
	}
}

func hasConfigError(messages []string) bool {
	for _, message := range messages {
		if strings.Contains(message, "未找到 homer 配置") ||
			strings.Contains(message, "未找到 ") ||
			strings.Contains(message, "配置无效") ||
			strings.Contains(message, "不是合法 JSON") ||
			strings.Contains(message, "store 不完整") {
			return true
		}
	}
	return false
}

func writeRemoteWriteReport(w http.ResponseWriter, raw json.RawMessage) {
	var report struct {
		OK     bool     `json:"ok"`
		Status string   `json:"status"`
		Errors []string `json:"errors"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &report) != nil {
		writeError(w, http.StatusInternalServerError, "internal", "agent 返回了无效报告", nil)
		return
	}

	statusCode := reportHTTPStatus(report.OK, report.Status, report.Errors)
	writeJSONBytes(w, statusCode, raw)
}

func writeRawReportEnvelope(w http.ResponseWriter, status int, raw json.RawMessage) {
	if len(raw) == 0 || !json.Valid(raw) {
		writeError(w, http.StatusInternalServerError, "internal", "agent 返回了无效报告", nil)
		return
	}
	writeJSON(w, status, struct {
		OK     bool            `json:"ok"`
		Report json.RawMessage `json:"report"`
	}{true, raw})
}

func writeCommandError(w http.ResponseWriter, err error) {
	code, status := commandErrorCode(err)
	details := []string{}
	if err != nil {
		details = []string{err.Error()}
	}
	writeError(w, status, code, errorMessage(code), details)
}

func writeConfigError(w http.ResponseWriter, err error) {
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusConflict, "not-initialized", "请先运行 homer init 完成初始化", []string{err.Error()})
		return
	}
	writeError(w, http.StatusConflict, "invalid-config", "homer.json 配置无效", []string{err.Error()})
}

func commandErrorCode(err error) (string, int) {
	if err == nil {
		return "internal", http.StatusInternalServerError
	}
	if errors.Is(err, os.ErrNotExist) {
		return "not-initialized", http.StatusConflict
	}
	message := err.Error()
	if strings.Contains(message, "未找到 homer 配置") || strings.Contains(message, "store 不完整") || strings.Contains(message, "请先运行 `homer init`") {
		return "not-initialized", http.StatusConflict
	}
	if strings.Contains(message, "配置无效") || strings.Contains(message, "不是合法 JSON") {
		return "invalid-config", http.StatusConflict
	}
	return "internal", http.StatusInternalServerError
}

func writeMethodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, POST")
	writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "不支持的请求方法", nil)
}

func confirmValue(r *http.Request) bool { return r.URL.Query().Get("confirm") == "true" }

func (s *Server) paths() core.HomerPaths {
	if s.opts.HomerHome == "" {
		return core.GetHomerPaths(nil)
	}
	return core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return s.opts.HomerHome
		}
		return os.Getenv(key)
	})
}

func (s *Server) homePath() string { return s.paths().Home }

// requestBaseURL reconstructs how THIS request reached the hub (scheme
// honors X-Forwarded-Proto behind the reverse proxy; host from the request
// itself) so the install script points agents at a URL that actually works.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// serveInstallScript renders the public Tailscale-style bootstrap script.
// The hub token is never embedded — the script's usage line tells the user
// to copy the complete command (with token) from the console.
func (s *Server) serveInstallScript(w http.ResponseWriter, r *http.Request) {
	script := RenderInstallScript(requestBaseURL(r), runtime.GOOS, runtime.GOARCH)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, script)
}

// serveSelfBinary streams the hub's own executable for same-platform
// agents (already token-authenticated by the router).
func (s *Server) serveSelfBinary(w http.ResponseWriter, r *http.Request) {
	self, err := os.Executable()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "no-binary", "定位 homer 二进制失败: "+err.Error(), nil)
		return
	}
	file, err := os.Open(self)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "no-binary", "读取 homer 二进制失败: "+err.Error(), nil)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		writeError(w, http.StatusInternalServerError, "no-binary", "homer 二进制不可读", nil)
		return
	}
	// The executable may have been replaced since boot (deploy); stream
	// what is on disk now, not a boot-time snapshot.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "homer", info.ModTime(), file)
}

// handleAgentRevoke drops a machine's per-agent enrollment secret. The
// revoked machine 401s on its next poll; other machines are unaffected.
// The agentID stays eligible for a fresh enrollment (re-install scenario).
func (s *Server) handleAgentRevoke(w http.ResponseWriter, r *http.Request) {
	if s.opts.Enrollment == nil {
		writeError(w, http.StatusNotImplemented, "enroll-disabled", "此 hub 未启用机器接入（旧版本）", nil)
		return
	}
	var payload struct {
		AgentID string `json:"agentId"`
	}
	if err := readJSONBody(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
		return
	}
	if strings.TrimSpace(payload.AgentID) == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "agentId 不能为空", nil)
		return
	}
	if !s.opts.Enrollment.Revoke(strings.TrimSpace(payload.AgentID)) {
		writeError(w, http.StatusConflict, "not-enrolled", "该机器没有有效凭证（可能从未接入或已吊销）", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAgentRemove drops a machine from the list entirely: credential
// revocation (when the enrollment service is wired) plus registry removal.
// The removed machine disappears from the console; if it still runs the
// agent daemon it re-registers on its next poll recovery — re-appearing
// only when its credential remains valid, which revocation prevents.
func (s *Server) handleAgentRemove(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		AgentID string `json:"agentId"`
	}
	if err := readJSONBody(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
		return
	}
	agentID := strings.TrimSpace(payload.AgentID)
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "agentId 不能为空", nil)
		return
	}
	if s.opts.Agents == nil {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "多机视图未启用", nil)
		return
	}
	// Kill the credential first (no-op for never-enrolled machines), then
	// drop the row. Order matters: remove-then-revoke would leave a window
	// where the daemon re-registers before its credential dies.
	if s.opts.Enrollment != nil {
		s.opts.Enrollment.Revoke(agentID)
	}
	if !s.opts.Agents.RemoveAgent(agentID) {
		writeError(w, http.StatusNotFound, "not-found", "机器不存在或已移除", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
