package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/gens"
)

// Conflict resolution (planner-frozen MVP step 4): the console never
// sends anyone to a terminal. Two buttons — keep this machine's content
// or apply the center's — map onto the existing merge pipeline.

type resolveChoice string

const (
	resolveLocal  resolveChoice = "local"
	resolveCenter resolveChoice = "center"
)

type resolveReport struct {
	OK     bool               `json:"ok"`
	Status string             `json:"status"`
	Choice string             `json:"choice"`
	Agents []agentApplyResult `json:"agents"`
	Errors []string           `json:"errors"`
}

// handleResolve backs POST /api/resolve?choice=local|center&confirm=true.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	choice := r.URL.Query().Get("choice")
	if choice != string(resolveLocal) && choice != string(resolveCenter) {
		writeError(w, http.StatusBadRequest, "bad-request", "choice 必须是 local 或 center", nil)
		return
	}
	if !confirmValue(r) {
		writeJSON(w, http.StatusConflict, resolveReport{
			OK: false, Status: "aborted", Choice: choice,
			Agents: []agentApplyResult{}, Errors: []string{msgNotConfirmed},
		})
		return
	}
	// Pure-server semantics: with ?agent= given the choice executes on
	// THAT machine — the server relays, never merges locally.
	if agentID := r.URL.Query().Get("agent"); agentID != "" {
		s.resolveOnMachine(w, r, resolveChoice(choice), agentID)
		return
	}
	writeMutex.Lock()
	defer writeMutex.Unlock()

	// No-git data plane: remote is the hub's current generation; a local
	// resolution publishes back through the same generation machinery.
	deps := &commands.MergeDeps{UI: commands.HeadlessUI{}, NoFetch: true}
	if head, ok := gens.New(s.opts.HomerHome).Read(); ok {
		if snapshot, err := readSnapshotFromGeneration(head); err == nil {
			deps.HubSnapshot = snapshot
		}
	}
	if choice == string(resolveLocal) {
		deps.HubSink = s.publishGeneration
	}
	report := commands.RunMerge(commands.MergeOptions{
		HomerHome:    s.opts.HomerHome,
		AcceptLocal:  choice == string(resolveLocal),
		AcceptRemote: choice == string(resolveCenter),
	}, deps)
	status := string(report.Status)

	switch {
	case !report.OK && status == "aborted":
		writeJSON(w, http.StatusConflict, resolveReport{
			OK: false, Status: "aborted", Choice: choice,
			Agents: []agentApplyResult{}, Errors: []string{msgNotConfirmed},
		})
	case !report.OK:
		writeJSON(w, http.StatusUnprocessableEntity, resolveReport{
			OK: false, Status: "error", Choice: choice,
			Agents: []agentApplyResult{}, Errors: []string{humanStatusSentence("error", report.Errors)},
		})
	case status == "no-conflicts":
		writeJSON(w, http.StatusOK, resolveReport{
			OK: true, Status: "no-conflicts", Choice: choice,
			Agents: []agentApplyResult{}, Errors: []string{},
		})
	case choice == string(resolveCenter):
		// Center wins: machine content was overwritten (with backup). The
		// center already holds the winning content — no fan-out.
		writeJSON(w, http.StatusOK, resolveReport{
			OK: true, Status: "resolved", Choice: choice,
			Agents: []agentApplyResult{}, Errors: []string{},
		})
	default:
		// local wins: content must also reach the center, then the fleet.
		// The no-git seam reports a failed publication through status
		// (error) — only the legacy git path leaks it via warnings and a
		// missing commit.
		pushFailed := false
		for _, warning := range report.Warnings {
			if strings.Contains(warning, "git push 失败") {
				pushFailed = true
			}
		}
		if pushFailed {
			writeJSON(w, http.StatusOK, resolveReport{
				OK: false, Status: "partial", Choice: choice,
				Agents: []agentApplyResult{},
				Errors: []string{"已保留本机内容，但还没写到中心，其他机器这次不会更新。"},
			})
			return
		}
		results := fanoutPullOnlineAgents(r.Context(), s.opts.Agents, SyncScope{})
		allOK := true
		errors := []string{}
		for _, result := range results {
			if !result.Skipped && !result.OK {
				allOK = false
				if result.Error != "" {
					errors = append(errors, result.Error)
				}
			}
		}
		if !allOK {
			writeJSON(w, http.StatusOK, resolveReport{
				OK: false, Status: "partial", Choice: choice,
				Agents: results, Errors: errors,
			})
			return
		}
		writeJSON(w, http.StatusOK, resolveReport{
			OK: true, Status: "resolved", Choice: choice,
			Agents: results, Errors: []string{},
		})
	}
}

// resolveOnMachine relays a conflict resolution to the machine that owns
// the conflict. "local" asks the machine to run its resolution (the
// machine's executor pushes the resolved content into the storage, then
// the server fans the new generation out to the other online machines);
// "center" asks it to apply the storage's current generation.
func (s *Server) resolveOnMachine(w http.ResponseWriter, r *http.Request, choice resolveChoice, agentID string) {
	if s.opts.Agents == nil {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "多机 agent 接口未启用", nil)
		return
	}
	name := agentID
	for _, info := range s.opts.Agents.ListAgents() {
		if info.AgentID == agentID && info.Hostname != "" {
			name = info.Hostname
		}
	}
	// "local" lets the machine resolve and publish (its executor's push
	// path applies the local-wins merge and uploads the result);
	// "center" has it apply the storage's current generation.
	scope, scopeErr := readSyncScope(r)
	if scopeErr != nil {
		writeError(w, http.StatusBadRequest, "bad-request", scopeErr.Error(), nil)
		return
	}
	if scope.Explicit {
		scope.Overwrite = true
		if choice == resolveCenter {
			if err := s.requireCenterAdapters(scope.Adapters); err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, errNoCenterSnapshot) {
					status = http.StatusConflict
				}
				writeError(w, status, "bad-request", err.Error(), nil)
				return
			}
		}
	} else {
		scope.Overwrite = false
	}
	// "Center wins" writes the center's content onto this machine, so it is a
	// dispatch like any other and must not leave a bound key behind.
	if choice == resolveCenter && s.refuseBoundKeyGap(w, scope) {
		return
	}
	var raw json.RawMessage
	var err error
	if choice == resolveLocal {
		raw, err = s.opts.Agents.AgentPush(r.Context(), agentID, true, scope)
	} else {
		raw, err = s.opts.Agents.AgentPull(r.Context(), agentID, true, scope)
	}
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	result := agentApplyResult{AgentID: agentID, Hostname: name, OK: true}
	var payload struct {
		OK      bool   `json:"ok"`
		Status  string `json:"status"`
		Secrets []struct {
			Path string `json:"path"`
		} `json:"secrets"`
	}
	// The machine's push stopped at the secret scanner. Say so, with the
	// files, so the console can offer "write anyway" instead of a vague
	// failure.
	if json.Unmarshal(raw, &payload) == nil && payload.Status == "secrets-rejected" {
		paths := make([]string, 0, len(payload.Secrets))
		for _, item := range payload.Secrets {
			paths = append(paths, item.Path)
		}
		writeJSON(w, http.StatusUnprocessableEntity, resolveReport{
			OK: false, Status: "secrets-rejected", Choice: string(choice),
			Agents: []agentApplyResult{{
				AgentID: agentID, Hostname: name, Status: "secrets-rejected",
			}},
			Errors: secretConfirmLines(paths),
		})
		return
	}
	if json.Unmarshal(raw, &payload) != nil || !payload.OK {
		result.OK = false
		result.Status = payload.Status
		result.Error = name + "没有应用这次裁决。"
	}
	agents := []agentApplyResult{result}
	if result.OK && choice == resolveLocal {
		// The machine published a new generation — deliver it to everyone
		// else who is online.
		for _, applied := range fanoutPullOnlineAgents(r.Context(), s.opts.Agents, scope) {
			if applied.AgentID != agentID {
				agents = append(agents, applied)
			}
		}
	}
	status := "resolved"
	errorsOut := []string{}
	if !result.OK {
		status = "partial"
		errorsOut = append(errorsOut, result.Error)
	}
	writeJSON(w, reportHTTPStatus(result.OK, status, errorsOut), resolveReport{
		OK:     result.OK,
		Status: status,
		Choice: string(choice),
		Agents: agents,
		Errors: errorsOut,
	})
}
