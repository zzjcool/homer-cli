package hub

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// ReportedAdaptersSource exposes the adapter IDs a machine last reported in
// its status response. Web consumers can discover this capability without
// depending on the hub package.
type ReportedAdaptersSource interface {
	AgentReportedAdapters(agentID string) []string
}

// MachineReportedAdapters extracts adapter IDs from either the enveloped
// status response (report.adapters), the legacy top-level adapters shape, or
// a pull report (applied.written/deleted[].adapterId — the adapters the
// dispatch actually touched). Top-level adapters take precedence when both
// are present, matching the status-report consumer in internal/web/choices.go.
func MachineReportedAdapters(status json.RawMessage) []string {
	if !json.Valid(status) {
		return nil
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(status, &envelope); err != nil || envelope == nil {
		return nil
	}

	adaptersRaw := envelope["adapters"]
	if missingOrNullJSON(adaptersRaw) {
		if reportRaw := envelope["report"]; !missingOrNullJSON(reportRaw) {
			var report map[string]json.RawMessage
			if err := json.Unmarshal(reportRaw, &report); err != nil {
				return nil
			}
			adaptersRaw = report["adapters"]
		}
	}
	if !missingOrNullJSON(adaptersRaw) {
		if ids := adapterIDsFromList(adaptersRaw); ids != nil {
			return ids
		}
	}
	// Pull 报告形状：applied.written / applied.deleted 携带实际触及的
	// adapter（FileRef.adapterId）。dispatch 后插件页的机器覆盖数因此能
	// 刷新，而不必等下一次 status。
	if appliedRaw := envelope["applied"]; !missingOrNullJSON(appliedRaw) {
		var applied struct {
			Written []struct {
				AdapterID string `json:"adapterId"`
			} `json:"written"`
			Deleted []struct {
				AdapterID string `json:"adapterId"`
			} `json:"deleted"`
		}
		if err := json.Unmarshal(appliedRaw, &applied); err == nil {
			seen := make(map[string]struct{})
			ids := make([]string, 0)
			for _, ref := range applied.Written {
				if ref.AdapterID != "" {
					if _, ok := seen[ref.AdapterID]; !ok {
						seen[ref.AdapterID] = struct{}{}
						ids = append(ids, ref.AdapterID)
					}
				}
			}
			for _, ref := range applied.Deleted {
				if ref.AdapterID != "" {
					if _, ok := seen[ref.AdapterID]; !ok {
						seen[ref.AdapterID] = struct{}{}
						ids = append(ids, ref.AdapterID)
					}
				}
			}
			if len(ids) > 0 {
				sort.Strings(ids)
				return ids
			}
		}
	}
	return nil
}

func adapterIDsFromList(raw json.RawMessage) []string {
	var adapters []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &adapters); err != nil {
		return nil
	}

	seen := make(map[string]struct{}, len(adapters))
	ids := make([]string, 0, len(adapters))
	for _, adapter := range adapters {
		id := strings.TrimSpace(adapter.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func missingOrNullJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// AgentReportedAdapters returns a copy of the last adapter report recorded
// for the machine, or nil when the machine has not reported one.
func (r *Registry) AgentReportedAdapters(agentID string) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	agent, ok := r.agents[agentID]
	if !ok {
		return nil
	}
	return cloneStrings(agent.ReportedAdapters)
}

var _ ReportedAdaptersSource = (*Registry)(nil)
