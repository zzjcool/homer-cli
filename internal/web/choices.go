package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
)

const (
	reasonConflict     = "有冲突，先选择保留哪一边"
	reasonNotOnMachine = "这台机器还没有这个适配器"
)

// AdapterChoice is one row in the collect, dispatch, or resolve picker.
type AdapterChoice struct {
	ID        string `json:"id"`
	Push      int    `json:"push"`
	Pull      int    `json:"pull"`
	Conflicts int    `json:"conflicts"`
	Checked   bool   `json:"checked"`
	Enabled   bool   `json:"enabled"`
	Detail    string `json:"detail,omitempty"`
	Reason    string `json:"reason,omitempty"`
	OnMachine bool   `json:"onMachine"`
	InCenter  bool   `json:"inCenter"`
	// Categories is the same outline the storage drawer shows: manifest
	// categories are a plugin list, never the virtual manifest file.
	Categories []OutlineCategory `json:"categories,omitempty"`
}

// BuildCollectChoices lists the adapters a machine can upload. Adapters
// with uncollected local changes start checked. Conflicts are shown but
// cannot be collected until someone picks a side.
func BuildCollectChoices(adapters []commands.StatusAdapterReport) []AdapterChoice {
	ordered := uniqueAdapterReports(adapters)
	out := make([]AdapterChoice, 0, len(ordered))
	for _, item := range ordered {
		choice := AdapterChoice{
			ID:        item.ID,
			Push:      item.Push,
			Pull:      item.Pull,
			Conflicts: item.Conflicts,
			OnMachine: true,
			Detail:    choiceDetail(item.Push, item.Pull, item.Conflicts),
			Enabled:   item.Conflicts == 0,
			Checked:   item.Conflicts == 0 && item.Push > 0,
		}
		if item.Conflicts > 0 {
			choice.Reason = reasonConflict
		}
		choice.Categories = outlineFromStatus(item.Categories)
		out = append(out, choice)
	}
	return out
}

// BuildDispatchChoices lists adapters the center can send to one machine.
// Only center adapters appear. Ones the machine is waiting on start
// checked, unless they conflict.
func BuildDispatchChoices(centerIDs []string, machine []commands.StatusAdapterReport) []AdapterChoice {
	onMachine := map[string]commands.StatusAdapterReport{}
	for _, adapter := range machine {
		if !core.ValidAdapterID(adapter.ID) {
			continue
		}
		if _, ok := onMachine[adapter.ID]; !ok {
			onMachine[adapter.ID] = adapter
		}
	}
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(centerIDs))
	for _, id := range centerIDs {
		id = strings.TrimSpace(id)
		if !core.ValidAdapterID(id) {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]AdapterChoice, 0, len(ids))
	for _, id := range ids {
		item, known := onMachine[id]
		choice := AdapterChoice{ID: id, InCenter: true, OnMachine: known, Enabled: true}
		if !known {
			choice.Reason = reasonNotOnMachine
			out = append(out, choice)
			continue
		}
		choice.Push = item.Push
		choice.Pull = item.Pull
		choice.Conflicts = item.Conflicts
		choice.Detail = choiceDetail(item.Push, item.Pull, item.Conflicts)
		if item.Conflicts > 0 {
			choice.Enabled = false
			choice.Checked = false
			choice.Reason = reasonConflict
		} else if item.Pull > 0 {
			choice.Checked = true
		}
		out = append(out, choice)
	}
	return out
}

// BuildResolveChoices lists adapters that currently conflict. They start
// checked; the user can narrow the resolution to a subset.
func BuildResolveChoices(adapters []commands.StatusAdapterReport) []AdapterChoice {
	out := make([]AdapterChoice, 0)
	for _, choice := range BuildCollectChoices(adapters) {
		if choice.Conflicts <= 0 {
			continue
		}
		choice.Enabled = true
		choice.Checked = true
		choice.Reason = ""
		out = append(out, choice)
	}
	return out
}

// OutlineFile is one leaf in the console's shared tree. A manifest
// category lists plugin names here; other categories list store paths.
type OutlineFile struct {
	Path   string `json:"path"`
	Size   int    `json:"size,omitempty"`
	Status string `json:"status,omitempty"`
}

// OutlineCategory is one folder under an adapter. Manifest categories
// are labeled 插件 so storage, collect, and dispatch never disagree
// about whether the user is looking at a virtual file or a plugin list.
type OutlineCategory struct {
	Name      string        `json:"name"`
	Label     string        `json:"label,omitempty"`
	Kind      string        `json:"kind,omitempty"`
	Push      int           `json:"push,omitempty"`
	Pull      int           `json:"pull,omitempty"`
	Conflicts int           `json:"conflicts,omitempty"`
	Files     []OutlineFile `json:"files"`
}

func outlineFromStatus(cats []commands.StatusCategoryReport) []OutlineCategory {
	if len(cats) == 0 {
		return nil
	}
	out := make([]OutlineCategory, 0, len(cats))
	for _, cat := range cats {
		item := OutlineCategory{
			Name:      cat.Name,
			Kind:      cat.Kind,
			Push:      cat.Push,
			Pull:      cat.Pull,
			Conflicts: cat.Conflicts,
		}
		if cat.Kind == "manifest" {
			item.Label = "插件"
		}
		for _, file := range cat.Files {
			if strings.HasSuffix(file.Path, ".manifest.txt") {
				continue
			}
			item.Files = append(item.Files, OutlineFile{Path: file.Path, Status: file.Status})
		}
		if item.Files == nil {
			item.Files = []OutlineFile{}
		}
		out = append(out, item)
	}
	return out
}

func choiceDetail(push, pull, conflicts int) string {
	parts := make([]string, 0, 3)
	if push > 0 {
		parts = append(parts, fmt.Sprintf("↑%d 未收取", push))
	}
	if pull > 0 {
		parts = append(parts, fmt.Sprintf("↓%d 待下发", pull))
	}
	if conflicts > 0 {
		parts = append(parts, fmt.Sprintf("%d 项冲突", conflicts))
	}
	return strings.Join(parts, " · ")
}

func uniqueAdapterReports(adapters []commands.StatusAdapterReport) []commands.StatusAdapterReport {
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(adapters))
	byID := map[string]commands.StatusAdapterReport{}
	for _, adapter := range adapters {
		if !core.ValidAdapterID(adapter.ID) {
			continue
		}
		if _, ok := seen[adapter.ID]; ok {
			continue
		}
		seen[adapter.ID] = struct{}{}
		ids = append(ids, adapter.ID)
		byID[adapter.ID] = adapter
	}
	sort.Strings(ids)
	out := make([]commands.StatusAdapterReport, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}

func parseStatusReport(raw json.RawMessage) (commands.StatusReport, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return commands.StatusReport{}, fmt.Errorf("机器没有返回配置情况")
	}
	var report commands.StatusReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return commands.StatusReport{}, err
	}
	if report.Adapters == nil {
		var envelope struct {
			Report commands.StatusReport `json:"report"`
		}
		if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Report.Adapters != nil {
			report = envelope.Report
		}
	}
	if report.Adapters == nil {
		report.Adapters = []commands.StatusAdapterReport{}
	}
	if report.Errors == nil {
		report.Errors = []string{}
	}
	return report, nil
}

func (s *Server) handleSyncChoices(w http.ResponseWriter, r *http.Request) {
	direction := r.URL.Query().Get("direction")
	agentID := strings.TrimSpace(r.URL.Query().Get("agent"))
	switch direction {
	case "collect", "dispatch", "resolve":
	default:
		writeError(w, http.StatusBadRequest, "bad-request", "direction 必须是 collect、dispatch 或 resolve", nil)
		return
	}
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "缺少 agent 参数", nil)
		return
	}
	if s.opts.Agents == nil {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "多机 agent 接口未启用", nil)
		return
	}
	raw, err := s.opts.Agents.AgentStatus(r.Context(), agentID)
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	report, err := parseStatusReport(raw)
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent-unreachable", err.Error(), nil)
		return
	}
	choices := []AdapterChoice{}
	hint := ""
	switch direction {
	case "collect":
		choices = BuildCollectChoices(report.Adapters)
		hint = collectChoiceHint(report, choices)
	case "dispatch":
		ids, published, err := s.centerAdapterIDs()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "storage-read", "读取存储内容失败: "+err.Error(), nil)
			return
		}
		if !published {
			hint = "中心还没有任何内容。先从一台机器收取。"
		} else {
			choices = BuildDispatchChoices(ids, report.Adapters)
			if _, adapters, outlineErr := s.readStorageOutline(); outlineErr == nil {
				attachStorageOutline(choices, adapters)
			}
			if len(choices) == 0 {
				hint = "中心还没有可下发的适配器。"
			} else {
				hint = "只会把勾选的适配器写到这台机器。其他适配器这次不动。"
			}
		}
	default:
		choices = BuildResolveChoices(report.Adapters)
		if len(choices) == 0 {
			hint = "这台机器没有需要裁决的冲突。"
		} else {
			hint = "选择要裁决的适配器。没勾选的这次不动。"
		}
	}
	if choices == nil {
		choices = []AdapterChoice{}
	}
	writeJSON(w, http.StatusOK, struct {
		OK        bool            `json:"ok"`
		Direction string          `json:"direction"`
		AgentID   string          `json:"agentId"`
		Hint      string          `json:"hint"`
		Adapters  []AdapterChoice `json:"adapters"`
	}{true, direction, agentID, hint, choices})
}

func collectChoiceHint(report commands.StatusReport, choices []AdapterChoice) string {
	if len(choices) == 0 {
		for _, message := range report.Errors {
			if strings.Contains(message, "未找到 homer 配置") {
				return "这台机器还没有配置，无法收取。"
			}
		}
		return "这台机器没有可收取的适配器。"
	}
	return "只会把勾选的适配器写入中心。没勾选的适配器保持中心现有内容。"
}
