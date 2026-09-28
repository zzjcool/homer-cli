package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
)

// Manual sync (planner-frozen MVP step 2): one console action collapses
// "apply this machine's changes to the center" (to-others) or "apply the
// center's changes here" (from-center). Responses speak in human
// sentences — git vocabulary never crosses the wire.

type syncDirection string

const (
	syncToOthers   syncDirection = "to-others"
	syncFromCenter syncDirection = "from-center"
)

// agentApplyResult is one machine's fan-out outcome. Error carries only
// the planner-approved human sentences — never commands errors or git
// output.
type agentApplyResult struct {
	AgentID  string `json:"agentId"`
	Hostname string `json:"hostname"`
	OK       bool   `json:"ok"`
	Skipped  bool   `json:"skipped"`
	Status   string `json:"status,omitempty"`
	Error    string `json:"error,omitempty"`
}

type syncReport struct {
	OK        bool               `json:"ok"`
	Status    string             `json:"status"`
	Direction string             `json:"direction"`
	Agents    []agentApplyResult `json:"agents"`
	Errors    []string           `json:"errors"`
}

// Human sentences (planner §2.5). These strings are the entire user-facing
// error vocabulary of sync — nothing else may be interpolated.
const (
	msgSecretsStopped   = "检测到疑似密钥，已停止。请从文件里去掉密钥后再试。"
	msgBothChanged      = "两边都改过同一处，这次没有写入。"
	msgCenterAhead      = "中心有这台机器还没有的改动，已停止写入。请改选从中心同步，或先处理冲突。"
	msgNotConfirmed     = "没有确认，未做任何改动。"
	msgDivergedHistory  = "这台机器和中心的历史已经分叉，这次没有改文件。"
	msgGenericFailure   = "同步没有完成。"
	msgAgentNotApplied  = "没有应用这次同步。"
	msgAgentUnreachable = "没有连上，这次没能更新它。"
	msgAgentTimeout     = "响应超时，这次没能更新它。"
)

// handleSync backs POST /api/sync?direction=to-others|from-center&confirm=true.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	direction := r.URL.Query().Get("direction")
	if direction != string(syncToOthers) && direction != string(syncFromCenter) {
		writeError(w, http.StatusBadRequest, "bad-request", "direction 必须是 to-others 或 from-center", nil)
		return
	}
	confirmed := confirmValue(r)
	writeMutex.Lock()
	defer writeMutex.Unlock()
	if direction == string(syncFromCenter) {
		s.syncFromCenter(w, confirmed)
		return
	}
	s.syncToOthers(w, r, confirmed)
}

// syncFromCenter applies the center's content onto this machine. Other
// machines are never touched.
func (s *Server) syncFromCenter(w http.ResponseWriter, confirmed bool) {
	if !confirmed {
		writeJSON(w, http.StatusConflict, syncReport{
			OK: false, Status: "aborted", Direction: string(syncFromCenter),
			Agents: []agentApplyResult{}, Errors: []string{msgNotConfirmed},
		})
		return
	}
	report := commands.RunPull(commands.PullOptions{
		HomerHome: s.opts.HomerHome,
		Yes:       true,
	}, &commands.PullDeps{UI: commands.HeadlessUI{}})
	status := string(report.Status)
	errors := []string{}
	if !report.OK {
		errors = []string{humanStatusSentence(status, report.Errors)}
	}
	writeJSON(w, reportHTTPStatus(report.OK, status, nil), syncReport{
		OK:        report.OK,
		Status:    status,
		Direction: string(syncFromCenter),
		Agents:    []agentApplyResult{},
		Errors:    errors,
	})
}

// syncToOthers writes this machine's changes to the center, then lets
// every online machine apply them. Offline machines are skipped; one
// failure never stops the rest.
func (s *Server) syncToOthers(w http.ResponseWriter, r *http.Request, confirmed bool) {
	if !confirmed {
		writeJSON(w, http.StatusConflict, syncReport{
			OK: false, Status: "aborted", Direction: string(syncToOthers),
			Agents: []agentApplyResult{}, Errors: []string{msgNotConfirmed},
		})
		return
	}
	push := commands.RunPush(commands.PushOptions{
		HomerHome: s.opts.HomerHome,
		Yes:       true,
	}, &commands.PushDeps{UI: commands.HeadlessUI{}})
	if !push.OK {
		status := string(push.Status)
		writeJSON(w, http.StatusUnprocessableEntity, syncReport{
			OK: false, Status: status, Direction: string(syncToOthers),
			Agents: []agentApplyResult{}, Errors: []string{humanStatusSentence(status, push.Errors)},
		})
		return
	}
	results := fanoutPullOnlineAgents(r.Context(), s.opts.Agents)
	allOK := true
	for _, result := range results {
		if !result.Skipped && !result.OK {
			allOK = false
		}
	}
	status := "synced"
	ok := true
	errors := []string{}
	if !allOK {
		status = "partial"
		ok = false
		for _, result := range results {
			if result.Error != "" {
				errors = append(errors, result.Error)
			}
		}
	}
	writeJSON(w, http.StatusOK, syncReport{
		OK: ok, Status: status, Direction: string(syncToOthers),
		Agents: results, Errors: errors,
	})
}

// fanoutPullOnlineAgents asks every online machine to apply the center's
// content. Stale machines are reported skipped; single failures never
// break the loop.
func fanoutPullOnlineAgents(ctx context.Context, agents AgentsSource) []agentApplyResult {
	results := []agentApplyResult{}
	if agents == nil {
		return results
	}
	for _, info := range agents.ListAgents() {
		name := info.Hostname
		if name == "" {
			name = info.AgentID
		}
		result := agentApplyResult{AgentID: info.AgentID, Hostname: name, OK: true}
		if info.Stale {
			result.Skipped = true
			results = append(results, result)
			continue
		}
		raw, err := agents.AgentPull(ctx, info.AgentID, true)
		switch {
		case err != nil:
			result.OK = false
			var agentErr *AgentError
			if errors.As(err, &agentErr) && agentErr.Code == "agent-timeout" {
				result.Error = name + msgAgentTimeout
			} else {
				result.Error = name + msgAgentUnreachable
			}
		default:
			var payload struct {
				OK     bool   `json:"ok"`
				Status string `json:"status"`
			}
			if json.Unmarshal(raw, &payload) != nil || !payload.OK {
				result.OK = false
				result.Status = payload.Status
				result.Error = name + msgAgentNotApplied
			}
		}
		results = append(results, result)
	}
	return results
}

// humanStatusSentence maps an internal status to its user sentence. The
// raw messages slice only feeds the divergence heuristic.
func humanStatusSentence(status string, messages []string) string {
	switch status {
	case "secrets-rejected":
		return msgSecretsStopped
	case "conflicts", "conflicts-remain":
		return msgBothChanged
	case "remote-ahead":
		return msgCenterAhead
	case "aborted":
		return msgNotConfirmed
	}
	for _, message := range messages {
		if strings.Contains(message, "分叉") {
			return msgDivergedHistory
		}
	}
	return msgGenericFailure
}
