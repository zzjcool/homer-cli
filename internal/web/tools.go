package web

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/zzjcool/homer-cli/internal/toolctl"
)

// AgentTool is one program an adapter drives (pi, herdr, opencode...) as
// installed on one machine, with the console's verdict on its version.
type AgentTool struct {
	toolctl.Status
	// Latest is the version this one is measured against: the newest release
	// any machine in the fleet runs, or the adapter's declared floor when that
	// is newer. Empty when nothing is known.
	Latest string `json:"latest,omitempty"`
	// Outdated is true when this machine runs something older than Latest.
	// A machine is only flagged on evidence: an unreadable version is not.
	Outdated bool `json:"outdated,omitempty"`
}

// annotateTools fills Latest and Outdated for every machine's programs. The
// reference is fleet wide, so a stale or offline machine still counts as
// evidence of a newer release.
func annotateTools(agents []AgentInfo) {
	fleet := make([][]toolctl.Status, 0, len(agents))
	for _, agent := range agents {
		report := make([]toolctl.Status, 0, len(agent.Tools))
		for _, tool := range agent.Tools {
			report = append(report, tool.Status)
		}
		fleet = append(fleet, report)
	}
	latest := toolctl.Reference(fleet)
	for i := range agents {
		for j := range agents[i].Tools {
			tool := &agents[i].Tools[j]
			reference := latest[tool.ID]
			tool.Latest = reference
			tool.Outdated = toolctl.Behind(tool.Version, reference)
		}
	}
}

// toolIDPattern is the shape of a registered tool ID. The hub only checks the
// shape; which IDs exist is the machine's decision, made against its own
// table, so a machine on a newer release can still be asked about a program
// this hub has never heard of.
var toolIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

func (s *Server) handleAgentToolUpgrade(w http.ResponseWriter, r *http.Request, agentID string) {
	var payload struct {
		Tool string `json:"tool"`
	}
	if err := readJSONBody(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
		return
	}
	tool := strings.TrimSpace(payload.Tool)
	if !toolIDPattern.MatchString(tool) {
		writeError(w, http.StatusBadRequest, "bad-request", "请指定要升级的应用", nil)
		return
	}
	upgrader, ok := s.opts.Agents.(interface {
		AgentToolUpgrade(ctx context.Context, agentID, tool string) (json.RawMessage, error)
	})
	if !ok {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "这台 hub 不能升级机器上的应用", nil)
		return
	}
	raw, err := upgrader.AgentToolUpgrade(r.Context(), agentID, tool)
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	writeToolUpgradeReport(w, raw)
}

// writeToolUpgradeReport relays a machine's upgrade report. A report that
// says the upgrade did not work becomes a 422 whose error carries the same
// sentence and the tail of the command's output, with the whole report beside
// it so the console can offer the official installer.
func writeToolUpgradeReport(w http.ResponseWriter, raw json.RawMessage) {
	var report struct {
		OK     bool   `json:"ok"`
		Note   string `json:"note"`
		Output string `json:"output"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &report) != nil {
		writeError(w, http.StatusInternalServerError, "internal", "机器没有返回升级结果", nil)
		return
	}
	if report.OK {
		writeJSONBytes(w, http.StatusOK, raw)
		return
	}
	message := strings.TrimSpace(report.Note)
	if message == "" {
		message = "升级没有完成"
	}
	details := []string{}
	for _, line := range strings.Split(report.Output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			details = append(details, line)
		}
	}
	writeJSONBytes(w, http.StatusUnprocessableEntity, mustMarshalJSON(struct {
		Error  errorBody       `json:"error"`
		Report json.RawMessage `json:"report"`
	}{
		Error:  errorBody{Code: "tool-upgrade-failed", Message: message, Details: details},
		Report: raw,
	}))
}
