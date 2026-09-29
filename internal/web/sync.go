package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
)

// Manual sync (planner-frozen MVP step 2): one console action collapses
// "apply this machine's changes to the center" (to-others) or "apply the
// center's changes here" (from-center). Responses speak in human
// sentences — git vocabulary never crosses the wire.

type syncDirection string

const (
	syncToOthers   syncDirection = "to-others"
	syncFromCenter syncDirection = "from-center"
	syncCollect    syncDirection = "collect"
	syncDispatch   syncDirection = "dispatch"
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

// handleSync backs POST /api/sync. The pure-server directions:
//
//	collect  — one machine uploads its content into the storage
//	           (query: agent=<agentId>; the hub runs no local push)
//	dispatch — every online machine applies the storage's current
//	           generation (offline machines are skipped)
//
// The legacy to-others/from-center pair (the hub acting as a machine)
// remains accepted during the transition.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	direction := r.URL.Query().Get("direction")
	switch syncDirection(direction) {
	case syncCollect:
		s.syncCollectFromMachine(w, r, confirmValue(r))
		return
	case syncDispatch:
		s.syncDispatchToMachines(w, r, confirmValue(r))
		return
	case syncToOthers, syncFromCenter:
		// legacy machine-coupled semantics
	default:
		writeError(w, http.StatusBadRequest, "bad-request", "direction 必须是 collect、dispatch、to-others 或 from-center", nil)
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

// syncCollectFromMachine asks one machine to push into the storage. The
// machine's own executor uploads the snapshot (its HubSink); the hub
// side only verifies a generation actually landed.
func (s *Server) syncCollectFromMachine(w http.ResponseWriter, r *http.Request, confirmed bool) {
	agentID := r.URL.Query().Get("agent")
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "缺少 agent 参数（要收取哪台机器）", nil)
		return
	}
	if !confirmed {
		writeJSON(w, http.StatusConflict, syncReport{
			OK: false, Status: "aborted", Direction: string(syncCollect),
			Agents: []agentApplyResult{}, Errors: []string{msgNotConfirmed},
		})
		return
	}
	if s.opts.Agents == nil {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "多机 agent 接口未启用", nil)
		return
	}
	before := 0
	if head, ok := gens.New(s.opts.HomerHome).Read(); ok {
		before = head.Generation
	}
	raw, err := s.opts.Agents.AgentPush(r.Context(), agentID, true)
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	name := agentID
	for _, info := range s.opts.Agents.ListAgents() {
		if info.AgentID == agentID {
			if info.Hostname != "" {
				name = info.Hostname
			}
		}
	}
	result := agentApplyResult{AgentID: agentID, Hostname: name, OK: true}
	var payload struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &payload) != nil || !payload.OK {
		result.OK = false
		result.Status = payload.Status
		result.Error = name + msgAgentNotApplied
	}
	head, ok := gens.New(s.opts.HomerHome).Read()
	published := ok && head.Generation > before
	status := "synced"
	errorsOut := []string{}
	if !result.OK || !published {
		status = "partial"
		if !published {
			errorsOut = append(errorsOut, name+"的内容没有存入中心（generation 未前进）。")
		}
	}
	writeJSON(w, reportHTTPStatus(result.OK && published, status, errorsOut), syncReport{
		OK:        result.OK && published,
		Status:    status,
		Direction: string(syncCollect),
		Agents:    []agentApplyResult{result},
		Errors:    errorsOut,
	})
}

// syncDispatchToMachines asks every online machine to pull the storage's
// current generation. Offline machines are skipped; one failure never
// stops the rest.
func (s *Server) syncDispatchToMachines(w http.ResponseWriter, r *http.Request, confirmed bool) {
	if !confirmed {
		writeJSON(w, http.StatusConflict, syncReport{
			OK: false, Status: "aborted", Direction: string(syncDispatch),
			Agents: []agentApplyResult{}, Errors: []string{msgNotConfirmed},
		})
		return
	}
	if _, ok := gens.New(s.opts.HomerHome).Read(); !ok {
		writeJSON(w, http.StatusConflict, syncReport{
			OK: false, Status: "no-snapshot", Direction: string(syncDispatch),
			Agents: []agentApplyResult{}, Errors: []string{"中心还没有任何快照（先从一台机器收取）"},
		})
		return
	}
	results := fanoutPullOnlineAgents(r.Context(), s.opts.Agents)
	allOK := true
	for _, result := range results {
		if !result.OK && !result.Skipped {
			allOK = false
		}
	}
	status := "synced"
	if !allOK {
		status = "partial"
	}
	writeJSON(w, reportHTTPStatus(allOK, status, nil), syncReport{
		OK:        allOK,
		Status:    status,
		Direction: string(syncDispatch),
		Agents:    results,
	})
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
	deps := &commands.PullDeps{UI: commands.HeadlessUI{}, NoFetch: true}
	if head, ok := gens.New(s.opts.HomerHome).Read(); ok {
		if snapshot, err := readSnapshotFromGeneration(head); err == nil {
			deps.HubSnapshot = snapshot
		}
	}
	report := commands.RunPull(commands.PullOptions{
		HomerHome: s.opts.HomerHome,
		Yes:       true,
	}, deps)
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
	}, &commands.PushDeps{UI: commands.HeadlessUI{}, HubSink: s.publishGeneration})
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

// snapshotPayload is the wire format of the no-git data plane: an agent
// uploads its prepared snapshot; pullers download the hub's current one.
type snapshotPayload struct {
	HomerJSON  string                       `json:"homerJson"`
	Store      map[string]map[string]string `json:"store"`
	Generation int                          `json:"generation"`
}

// generationMutex serializes generation publication only. It must NOT be
// the global writeMutex: an agent push invoked by the hub's dispatcher
// (which holds writeMutex) uploads back to /api/snapshot — sharing the
// lock would deadlock hub -> agent -> hub.
var generationMutex sync.Mutex

// handleSnapshotUpload backs POST /api/snapshot (agent push transport).
// It publishes the payload as a new hub generation — the generation
// counter itself is the compare-and-swap the advisor ruling requires.
// snapshotBodyLimit bounds a snapshot upload: a full machine snapshot is
// plain-text configuration, easily a few hundred KB and plausibly a few
// MB for heavy setups — far beyond the console's 64KB form limit.
const snapshotBodyLimit = 32 << 20

func (s *Server) handleSnapshotUpload(w http.ResponseWriter, r *http.Request) {
	var payload snapshotPayload
	decoder := json.NewDecoder(io.LimitReader(r.Body, snapshotBodyLimit))
	if err := decoder.Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", "请求体不是有效的 JSON（或超过 32MB 上限）", nil)
		return
	}
	generationMutex.Lock()
	defer generationMutex.Unlock()
	layout := gens.New(s.opts.HomerHome)
	generation, err := layout.Publish(payload.Store, []byte(payload.HomerJSON))
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "generation": generation})
}

// handleSnapshotDownload backs GET /api/snapshot (agent pull transport).
// It streams the generation HEAD names — a concurrent publish never yields
// a mixed tree.
func (s *Server) handleSnapshotDownload(w http.ResponseWriter, r *http.Request) {
	layout := gens.New(s.opts.HomerHome)
	head, ok := layout.Read()
	if !ok {
		writeError(w, http.StatusConflict, "no-snapshot", "中心还没有任何快照（先从一台机器同步到其他机器）", nil)
		return
	}
	store := map[string]map[string]string{}
	_ = filepath.WalkDir(head.StoreDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(head.StoreDir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		segments := strings.SplitN(rel, "/", 2)
		if len(segments) != 2 {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if store[segments[0]] == nil {
			store[segments[0]] = map[string]string{}
		}
		store[segments[0]][segments[1]] = string(data)
		return nil
	})
	writeJSON(w, http.StatusOK, snapshotPayload{
		Generation: head.Generation,
		HomerJSON:  string(head.Meta),
		Store:      store,
	})
}

// publishGeneration is the hub-side push sink: it publishes the prepared
// snapshots as the hub's new current generation. Local call — the hub
// never HTTP-loops itself.
func (s *Server) publishGeneration(snapshot []core.AdapterSnapshot) (int, error) {
	store := map[string]map[string]string{}
	meta := []byte("{}")
	if data, err := os.ReadFile(filepath.Join(s.opts.HomerHome, "homer.json")); err == nil {
		meta = data
	}
	for _, adapter := range snapshot {
		for _, category := range adapter.Categories {
			for relPath, entry := range category.Files {
				if store[adapter.AdapterID] == nil {
					store[adapter.AdapterID] = map[string]string{}
				}
				store[adapter.AdapterID][category.Category+"/"+relPath] = entry.Content
			}
		}
	}
	return gens.New(s.opts.HomerHome).Publish(store, meta)
}

// readSnapshotFromGeneration converts the hub's current generation back
// into the adapter-snapshot form the pull pipeline consumes.
func readSnapshotFromGeneration(head gens.Head) ([]core.AdapterSnapshot, error) {
	store := map[string]map[string]string{}
	err := filepath.WalkDir(head.StoreDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		rel, relErr := filepath.Rel(head.StoreDir, path)
		if relErr != nil {
			return relErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		segments := strings.SplitN(filepath.ToSlash(rel), "/", 3)
		if len(segments) != 3 {
			return nil
		}
		if store[segments[0]] == nil {
			store[segments[0]] = map[string]string{}
		}
		store[segments[0]][segments[1]+"/"+segments[2]] = string(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	snapshots := []core.AdapterSnapshot{}
	for adapterID, files := range store {
		categories := map[string]*core.CategorySnapshot{}
		for name, content := range files {
			segments := strings.SplitN(name, "/", 2)
			if len(segments) != 2 {
				continue
			}
			if categories[segments[0]] == nil {
				categories[segments[0]] = &core.CategorySnapshot{
					AdapterID: adapterID,
					Category:  segments[0],
					Mode:      core.SyncModeMirror,
					Files:     core.SnapshotFiles{},
				}
			}
			categories[segments[0]].Files[segments[1]] = core.SnapshotEntry{Kind: "file", Content: content}
		}
		snapshot := core.AdapterSnapshot{AdapterID: adapterID, Categories: []core.CategorySnapshot{}}
		for _, category := range categories {
			snapshot.Categories = append(snapshot.Categories, *category)
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

// handleConsole backs GET /api/console — the pure-server view the hero
// renders. The server is NOT a machine: the view speaks about the
// storage's current generation and every connected machine's drift
// relative to the storage it last synced with.
func (s *Server) handleConsole(w http.ResponseWriter, _ *http.Request) {
	type machineView struct {
		AgentID  string      `json:"agentId"`
		Hostname string      `json:"hostname"`
		Stale    bool        `json:"stale"`
		Drift    *AgentDrift `json:"drift,omitempty"`
	}
	layout := gens.New(s.opts.HomerHome)
	head, published := layout.Read()
	machines := []machineView{}
	if s.opts.Agents != nil {
		for _, info := range s.opts.Agents.ListAgents() {
			view := machineView{
				AgentID:  info.AgentID,
				Hostname: info.Hostname,
				Stale:    info.Stale,
			}
			if info.Drift != nil {
				view.Drift = &AgentDrift{
					Push:      info.Drift.Push,
					Pull:      info.Drift.Pull,
					Conflicts: info.Drift.Conflicts,
					Error:     info.Drift.Error,
				}
			}
			machines = append(machines, view)
		}
	}
	generation := 0
	if published {
		generation = head.Generation
	}
	writeJSON(w, http.StatusOK, struct {
		OK         bool          `json:"ok"`
		Published  bool          `json:"published"`
		Generation int           `json:"generation"`
		Machines   []machineView `json:"machines"`
	}{true, published, generation, machines})
}
