package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/resolutions"
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
	// Note explains a deliberate non-action (for example why other machines
	// were not written). It is informational and does not make ok false.
	Note string `json:"note,omitempty"`
	// Recorded is the set of decisions written before an immediate relay.
	Recorded []string `json:"recorded,omitempty"`
	// Pending is the subset retained when an immediate relay fails.
	Pending []string `json:"pending,omitempty"`
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
	recordImmediate := r.URL.Query().Get("record") == "true"
	// Preserve legacy no-agent precedence while enforcing record=true's
	// explicit-selection requirement before reporting optional capability.
	if !recordImmediate && s.opts.Agents == nil {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "多机 agent 接口未启用", nil)
		return
	}
	scope, scopeErr := readSyncScope(r)
	if scopeErr != nil {
		writeError(w, http.StatusBadRequest, "bad-request", scopeErr.Error(), nil)
		return
	}
	if recordImmediate && !scope.Explicit {
		writeError(w, http.StatusBadRequest, "bad-request", "请选择至少一个适配器", nil)
		return
	}
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
	if scope.Explicit {
		scope.Overwrite = true
		if choice == resolveCenter || recordImmediate {
			if err := s.requireCenterAdapters(scope.Adapters); err != nil {
				status, code := http.StatusBadRequest, "bad-request"
				if errors.Is(err, errNoCenterSnapshot) {
					status = http.StatusConflict
					if recordImmediate {
						code = "no-snapshot"
					}
				}
				writeError(w, status, code, err.Error(), nil)
				return
			}
		}
	} else {
		scope.Overwrite = false
	}
	// "Center wins" writes the center's content onto this machine, so it is a
	// dispatch like any other: a bound key must travel with it and be opened
	// first, or nothing is written. The password step is the same one a plain
	// dispatch uses.
	// The key-gap gate guards writes FROM the center onto the machine
	// ("center" writes the center's content, a dispatch in disguise).
	// "local" publishes the machine's own content to the center and must
	// never be blocked by (or drag along) the keyring — the UI deliberately
	// excludes secrets from the local choice.
	if choice == resolveCenter {
		if s.refuseBoundKeyGap(w, scope) {
			return
		}
		if messages := s.dispatchUnlockErrors(scope); len(messages) > 0 {
			code := "unlock-required"
			status := http.StatusUnprocessableEntity
			if len(messages) == 1 && messages[0] == "这台 hub 不能在机器上解开密钥" {
				code = "agents-disabled"
				status = http.StatusNotImplemented
			}
			writeError(w, status, code, "下发带了密钥，要先解开。", messages)
			return
		}
	}

	var recorded []string
	var pending []string
	if recordImmediate {
		actor, ok := s.opts.Agents.(ResolutionSource)
		if !ok {
			writeError(w, http.StatusNotImplemented, "agents-disabled", "多机 agent 接口未启用", nil)
			return
		}
		generation := s.centerGeneration()
		if generation < 1 {
			writeError(w, http.StatusConflict, "no-snapshot", "中心还没有任何快照（先从一台机器收取）", nil)
			return
		}
		recordRaw, recordErr := actor.AgentResolveRecord(r.Context(), agentID, ResolveRecordRequest{
			Action: resolutions.ActionRecord, Choice: string(choice),
			Adapters: append([]string(nil), scope.Adapters...), CenterGeneration: generation,
		})
		if recordErr != nil {
			writeErrorValue(w, recordErr)
			return
		}
		recordReport, parseErr := parseResolutionTaskReport(recordRaw)
		if parseErr != nil {
			writeError(w, http.StatusBadGateway, "agent-unreachable", "机器没有返回有效的记录结果", []string{parseErr.Error()})
			return
		}
		recorded = resolutionAdapterIDs(recordReport.Recorded)
		pending = append([]string(nil), recorded...)
		if !recordReport.OK {
			writeJSON(w, http.StatusUnprocessableEntity, resolveReport{
				OK: false, Status: "error", Choice: string(choice),
				Agents: []agentApplyResult{}, Errors: nonNilStrings(recordReport.Errors),
				Recorded: recorded, Pending: pending,
			})
			return
		}
		if len(recorded) == 0 {
			writeError(w, http.StatusBadGateway, "agent-unreachable", "机器没有返回有效的记录结果", nil)
			return
		}
		scope.ClearResolutions = true
	}

	var raw json.RawMessage
	var err error
	if choice == resolveLocal {
		raw, err = s.opts.Agents.AgentPush(r.Context(), agentID, true, scope)
	} else {
		raw, err = s.opts.Agents.AgentPull(r.Context(), agentID, true, scope)
	}
	if err != nil {
		if recordImmediate {
			status, _ := errorStatusCode(err)
			if status == 0 {
				status = http.StatusBadGateway
			}
			writeJSON(w, status, resolveReport{
				OK: false, Status: "partial", Choice: string(choice),
				Agents: []agentApplyResult{}, Errors: []string{err.Error()},
				Recorded: recorded, Pending: pending,
			})
		} else {
			writeErrorValue(w, err)
		}
		return
	}
	if recordImmediate && remoteReportOK(raw) {
		pending = nil
	}
	if choice == resolveCenter && pullWarrantsUnlock(raw) {
		if messages := s.unlockDispatched(r.Context(), agentID, scope); len(messages) > 0 {
			message := "内容已经写上，但密钥没有解开。"
			details := pullUnlockFailureDetails(raw, message, messages)
			writeError(w, http.StatusUnprocessableEntity, "unlock-failed", message, details)
			return
		}
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
			Errors: secretConfirmLines(paths), Recorded: recorded, Pending: pending,
		})
		return
	}
	if json.Unmarshal(raw, &payload) != nil || !payload.OK {
		result.OK = false
		result.Status = payload.Status
		result.Error = name + "没有应用这次裁决。"
	}
	agents := []agentApplyResult{result}
	fanoutNote := ""
	if result.OK && choice == resolveLocal {
		// The machine's content is already in the center. Delivering it to the
		// other online machines is a dispatch, and an adapter with a key bound
		// to it cannot be delivered whole by a fan-out (there is no password
		// step per machine). Keep the upload, skip the fan-out for that scope,
		// and say so, rather than writing the adapter without its key.
		if gaps := boundKeyGaps(s.opts.HomerHome, scope); len(gaps) > 0 {
			fanoutNote = strings.Join(gaps, "、") + " 绑定了密钥，没法连同密钥自动发给其他机器，所以只写到了中心，没有下发给其他机器。需要时到那台机器上「下发」，勾选绑定了密钥的适配器（如 " + strings.Join(gaps, "、") + "）并填口令，密钥会自动跟过去。"
		} else {
			for _, applied := range fanoutPullOnlineAgents(r.Context(), s.opts.Agents, scope) {
				if applied.AgentID != agentID {
					agents = append(agents, applied)
				}
			}
		}
	}
	status := "resolved"
	errorsOut := []string{}
	if !result.OK {
		status = "partial"
		errorsOut = append(errorsOut, result.Error)
	}
	if fanoutNote != "" {
		// The machine's resolution itself worked; the note only explains why
		// the other machines were not written. It is not an error.
		status = "resolved-not-fanned-out"
	}
	writeJSON(w, reportHTTPStatus(result.OK, status, errorsOut), resolveReport{
		OK:       result.OK,
		Status:   status,
		Choice:   string(choice),
		Agents:   agents,
		Errors:   errorsOut,
		Note:     fanoutNote,
		Recorded: recorded, Pending: pending,
	})
}
