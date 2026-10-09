package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/resolutions"
)

type resolveRecordSkip struct {
	Adapter string `json:"adapter"`
	Reason  string `json:"reason"`
}

type resolveRecordReport struct {
	OK       bool                `json:"ok"`
	Status   string              `json:"status"` // recorded|cleared|aborted|nothing-to-record|error
	AgentID  string              `json:"agentId"`
	Choice   string              `json:"choice,omitempty"`
	Recorded []resolutions.Entry `json:"recorded"`
	Replaced []resolutions.Entry `json:"replaced"`
	Skipped  []resolveRecordSkip `json:"skipped"`
	Pending  int                 `json:"pending"`
	Errors   []string            `json:"errors"`
}

type resolveRecordListEntry struct {
	resolutions.Entry
	Stale bool `json:"stale"`
}

// centerGeneration returns the current published hub generation, or zero
// when the hub has no snapshot. Zero is fail-safe stale for every decision.
func (s *Server) centerGeneration() int {
	if s == nil {
		return 0
	}
	head, ok := gens.New(s.opts.HomerHome).Read()
	if !ok {
		return 0
	}
	return head.Generation
}

func (s *Server) handleResolveRecord(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleResolveRecordList(w, r)
	case http.MethodPost:
		s.handleResolveRecordPost(w, r)
	default:
		writeMethodNotAllowed(w)
	}
}

func (s *Server) handleResolveRecordList(w http.ResponseWriter, r *http.Request) {
	agentID := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "缺少 agent 参数", nil)
		return
	}
	actor, ok := s.opts.Agents.(ResolutionSource)
	if s.opts.Agents == nil || !ok {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "多机 agent 接口未启用", nil)
		return
	}
	raw, err := actor.AgentResolveRecord(r.Context(), agentID, ResolveRecordRequest{Action: resolutions.ActionList})
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	report, err := parseResolutionTaskReport(raw)
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent-unreachable", "机器没有返回有效的决定列表", []string{err.Error()})
		return
	}
	if !report.OK {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"ok": false, "entries": []resolveRecordListEntry{}, "centerGeneration": s.centerGeneration(),
			"errors": nonNilStrings(report.Errors),
		})
		return
	}
	generation := s.centerGeneration()
	entries := make([]resolveRecordListEntry, 0, len(report.Entries))
	for _, entry := range report.Entries {
		entries = append(entries, resolveRecordListEntry{Entry: entry, Stale: resolutions.Stale(entry, generation)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "entries": entries, "centerGeneration": generation,
	})
}

func (s *Server) handleResolveRecordPost(w http.ResponseWriter, r *http.Request) {
	agentID := strings.TrimSpace(r.URL.Query().Get("agent"))
	choice := strings.TrimSpace(r.URL.Query().Get("choice"))
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "缺少 agent 参数", nil)
		return
	}
	if choice != resolutions.ChoiceCenter && choice != resolutions.ChoiceLocal {
		writeError(w, http.StatusBadRequest, "bad-request", "choice 必须是 local 或 center", nil)
		return
	}
	if !confirmValue(r) {
		writeJSON(w, http.StatusConflict, resolveRecordReport{
			OK: false, Status: "aborted", AgentID: agentID, Choice: choice,
			Recorded: []resolutions.Entry{}, Replaced: []resolutions.Entry{},
			Skipped: []resolveRecordSkip{}, Errors: []string{msgNotConfirmed},
		})
		return
	}
	scope, err := readSyncScope(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
		return
	}
	if !scope.Explicit {
		writeError(w, http.StatusBadRequest, "bad-request", "请选择至少一个适配器", nil)
		return
	}
	if s.opts.Agents == nil {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "多机 agent 接口未启用", nil)
		return
	}
	actor, ok := s.opts.Agents.(ResolutionSource)
	if !ok {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "多机 agent 接口未启用", nil)
		return
	}
	if s.centerGeneration() < 1 {
		writeError(w, http.StatusConflict, "no-snapshot", "中心还没有任何快照（先从一台机器收取）", nil)
		return
	}
	statusRaw, err := s.opts.Agents.AgentStatus(r.Context(), agentID)
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	statusReport, err := parseStatusReport(statusRaw)
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent-unreachable", "机器没有返回有效的冲突情况", []string{err.Error()})
		return
	}
	conflicts := map[string]bool{}
	for _, adapter := range statusReport.Adapters {
		if adapter.Conflicts > 0 {
			conflicts[adapter.ID] = true
		}
	}
	selected := make([]string, 0, len(scope.Adapters))
	skipped := make([]resolveRecordSkip, 0)
	for _, adapter := range scope.Adapters {
		if conflicts[adapter] {
			selected = append(selected, adapter)
		} else {
			skipped = append(skipped, resolveRecordSkip{Adapter: adapter, Reason: "当前没有冲突"})
		}
	}
	if len(selected) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, resolveRecordReport{
			OK: false, Status: "nothing-to-record", AgentID: agentID, Choice: choice,
			Recorded: []resolutions.Entry{}, Replaced: []resolutions.Entry{},
			Skipped: skipped, Errors: []string{},
		})
		return
	}
	generation := s.centerGeneration()
	if generation < 1 {
		writeError(w, http.StatusConflict, "no-snapshot", "中心还没有任何快照（先从一台机器收取）", nil)
		return
	}
	raw, err := actor.AgentResolveRecord(r.Context(), agentID, ResolveRecordRequest{
		Action: resolutions.ActionRecord, Choice: choice,
		Adapters: selected, CenterGeneration: generation,
	})
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	record, err := parseResolutionTaskReport(raw)
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent-unreachable", "机器没有返回有效的记录结果", []string{err.Error()})
		return
	}
	response := resolveRecordReport{
		OK: record.OK, Status: "recorded", AgentID: agentID, Choice: choice,
		Recorded: nonNilEntries(record.Recorded), Replaced: nonNilEntries(record.Replaced),
		Skipped: skipped, Pending: record.Pending, Errors: nonNilStrings(record.Errors),
	}
	if !record.OK {
		response.Status = "error"
		writeJSON(w, http.StatusUnprocessableEntity, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleResolveClear(w http.ResponseWriter, r *http.Request) {
	agentID := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "缺少 agent 参数", nil)
		return
	}
	if !confirmValue(r) {
		writeJSON(w, http.StatusConflict, resolveRecordReport{
			OK: false, Status: "aborted", AgentID: agentID,
			Recorded: []resolutions.Entry{}, Replaced: []resolutions.Entry{},
			Skipped: []resolveRecordSkip{}, Errors: []string{msgNotConfirmed},
		})
		return
	}
	scope, err := readSyncScope(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
		return
	}
	if !scope.Explicit {
		writeError(w, http.StatusBadRequest, "bad-request", "请选择至少一个适配器", nil)
		return
	}
	if s.opts.Agents == nil {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "多机 agent 接口未启用", nil)
		return
	}
	actor, ok := s.opts.Agents.(ResolutionSource)
	if !ok {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "多机 agent 接口未启用", nil)
		return
	}
	if s.centerGeneration() < 1 {
		writeError(w, http.StatusConflict, "no-snapshot", "中心还没有任何快照（先从一台机器收取）", nil)
		return
	}
	raw, err := actor.AgentResolveRecord(r.Context(), agentID, ResolveRecordRequest{
		Action: resolutions.ActionClear, Adapters: append([]string(nil), scope.Adapters...),
	})
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	report, err := parseResolutionTaskReport(raw)
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent-unreachable", "机器没有返回有效的清除结果", []string{err.Error()})
		return
	}
	response := resolveRecordReport{
		OK: report.OK, Status: "cleared", AgentID: agentID,
		Recorded: []resolutions.Entry{}, Replaced: []resolutions.Entry{},
		Skipped: []resolveRecordSkip{}, Pending: report.Pending, Errors: nonNilStrings(report.Errors),
	}
	if !report.OK {
		response.Status = "error"
		writeJSON(w, http.StatusUnprocessableEntity, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func parseResolutionTaskReport(raw json.RawMessage) (resolutions.Report, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return resolutions.Report{}, errors.New("empty or invalid JSON")
	}
	var report resolutions.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		return resolutions.Report{}, err
	}
	if report.Entries == nil {
		report.Entries = []resolutions.Entry{}
	}
	if report.Recorded == nil {
		report.Recorded = []resolutions.Entry{}
	}
	if report.Replaced == nil {
		report.Replaced = []resolutions.Entry{}
	}
	if report.Errors == nil {
		report.Errors = []string{}
	}
	return report, nil
}

func nonNilEntries(entries []resolutions.Entry) []resolutions.Entry {
	if entries == nil {
		return []resolutions.Entry{}
	}
	return entries
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func resolutionAdapterIDs(entries []resolutions.Entry) []string {
	ids := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.Adapter == "" {
			continue
		}
		if _, ok := seen[entry.Adapter]; ok {
			continue
		}
		seen[entry.Adapter] = struct{}{}
		ids = append(ids, entry.Adapter)
	}
	return ids
}
