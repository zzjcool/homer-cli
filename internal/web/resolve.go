package web

import (
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
		results := fanoutPullOnlineAgents(r.Context(), s.opts.Agents)
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
