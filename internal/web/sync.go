package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
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
	scope, err := readSyncScope(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
		return
	}
	// Collect never force-publishes a conflict; resolution has its own action.
	scope.Overwrite = false
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
	raw, err := s.opts.Agents.AgentPush(r.Context(), agentID, true, scope)
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
		OK      bool     `json:"ok"`
		Status  string   `json:"status"`
		Errors  []string `json:"errors"`
		Secrets []struct {
			Path string `json:"path"`
		} `json:"secrets"`
	}
	if json.Unmarshal(raw, &payload) == nil && payload.Status == "secrets-rejected" {
		paths := make([]string, 0, len(payload.Secrets))
		for _, item := range payload.Secrets {
			paths = append(paths, item.Path)
		}
		writeJSON(w, http.StatusUnprocessableEntity, syncReport{
			OK: false, Status: "secrets-rejected", Direction: string(syncCollect),
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
		result.Error = agentResultError(name, payload.Status, payload.Errors)
	}
	head, ok := gens.New(s.opts.HomerHome).Read()
	published := ok && head.Generation > before
	status := "synced"
	errorsOut := []string{}
	if !result.OK {
		status = "partial"
		if !published {
			errorsOut = append(errorsOut, name+"的内容没有存入中心（generation 未前进）。")
		}
	} else if !published {
		// A machine reporting no-drift needs no new generation — the
		// storage already IS its content. "synced" (not partial): the
		// machine is in sync with the storage by definition.
		if payload.Status == "no-drift" {
			result.Status = "no-drift"
		} else {
			status = "partial"
			errorsOut = append(errorsOut, name+"报告已推送，但中心 generation 未前进。")
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
	scope, err := readSyncScope(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
		return
	}
	scope.Overwrite = false
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
	if scope.Explicit {
		if err := s.requireCenterAdapters(scope.Adapters); err != nil {
			writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
			return
		}
	}
	// A fan-out has no per-machine password step, so an adapter with a key
	// bound to it cannot be delivered whole. Refuse instead of half applying.
	if s.refuseBoundKeyGap(w, scope) {
		return
	}
	results := fanoutPullOnlineAgents(r.Context(), s.opts.Agents, scope)
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
	// The push above already reached the center; the fan-out below would write
	// every machine from it. Stop before that if a bound key cannot travel.
	if s.refuseBoundKeyGap(w, SyncScope{}) {
		return
	}
	results := fanoutPullOnlineAgents(r.Context(), s.opts.Agents, SyncScope{})
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
const maxConcurrentAgentPulls = 8

func fanoutPullOnlineAgents(ctx context.Context, agents AgentsSource, scope SyncScope) []agentApplyResult {
	if agents == nil {
		return []agentApplyResult{}
	}
	infos := agents.ListAgents()
	results := make([]agentApplyResult, len(infos))
	jobs := make(chan int)
	workers := min(maxConcurrentAgentPulls, len(infos))
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for index := range jobs {
				info := infos[index]
				name := info.Hostname
				if name == "" {
					name = info.AgentID
				}
				result := agentApplyResult{AgentID: info.AgentID, Hostname: name, OK: true}
				if info.Stale {
					result.Skipped = true
					results[index] = result
					continue
				}
				raw, err := agents.AgentPull(ctx, info.AgentID, true, scope)
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
				results[index] = result
			}
		}()
	}
	for index := range infos {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	return results
}

// secretConfirmLines is the console warning for a scanner hit. Paths only;
// the matched text stays on the agent.
func secretConfirmLines(paths []string) []string {
	lines := []string{"这些文件里有像密钥的内容。可以去掉后再收取，也可以确认后仍然写入。"}
	seen := map[string]struct{}{}
	ordered := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	return append(lines, ordered...)
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

func agentResultError(name, status string, messages []string) string {
	switch status {
	case "conflicts", "conflicts-remain":
		return msgBothChanged
	case "secrets-rejected":
		return msgSecretsStopped
	}
	if len(messages) > 0 && !strings.Contains(messages[0], "homer merge") && !strings.Contains(messages[0], "git ") {
		return messages[0]
	}
	return name + msgAgentNotApplied
}

// snapshotPayload is the wire format of the no-git data plane: an agent
// uploads its prepared snapshot; pullers download the hub's current one.
type snapshotPayload struct {
	HomerJSON  string                       `json:"homerJson"`
	Store      map[string]map[string]string `json:"store"`
	Generation int                          `json:"generation"`
	// Adapters, when set, merges just those adapters into the previous
	// generation. Omitted, the upload replaces the generation entirely.
	Adapters []string `json:"adapters,omitempty"`
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
	store := payload.Store
	if store == nil {
		store = map[string]map[string]string{}
	}
	if payload.Adapters != nil {
		ids, err := syncx.ParseAdapterIDs(payload.Adapters)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
			return
		}
		var base map[string]map[string]string
		if head, ok := layout.Read(); ok {
			base, err = readGenerationStore(head.StoreDir)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "storage-read", "读取存储内容失败: "+err.Error(), nil)
				return
			}
		}
		store = syncx.MergeScopedStore(base, store, ids)
	}
	generation, err := layout.Publish(store, []byte(payload.HomerJSON))
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
	contentHash := sha256.New()
	writeHashField := func(value []byte) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = contentHash.Write(length[:])
		_, _ = contentHash.Write(value)
	}
	writeHashField(head.Meta)
	_ = filepath.WalkDir(head.StoreDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(head.StoreDir, path)
		if relErr != nil || len(strings.SplitN(filepath.ToSlash(rel), "/", 2)) != 2 {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		writeHashField([]byte(filepath.ToSlash(rel)))
		writeHashField(data)
		return nil
	})
	contentDigest := hex.EncodeToString(contentHash.Sum(nil))[:16]
	etag := fmt.Sprintf("\"g%d-%s\"", head.Generation, contentDigest)
	w.Header().Set("ETag", etag)
	if ifNoneMatch(r.Header.Values("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
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

func ifNoneMatch(values []string, etag string) bool {
	for _, value := range values {
		for _, candidate := range strings.Split(value, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
				return true
			}
		}
	}
	return false
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

type storageAdapter struct {
	ID         string            `json:"id"`
	Categories []OutlineCategory `json:"categories"`
}

// handleStorageListing answers "what does the server hold right now":
// the current generation's adapters/categories/files with sizes, for
// the console's storage drawer. Manifest categories are plugin lists.
func (s *Server) handleStorageListing(w http.ResponseWriter, _ *http.Request) {
	generation, adapters, err := s.readStorageOutline()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage-read", "读取存储内容失败: "+err.Error(), nil)
		return
	}
	if adapters == nil {
		adapters = []storageAdapter{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"generation": generation,
		"adapters":   adapters,
		"secrets":    credentialRulesView(s.opts.HomerHome),
	})
}

func (s *Server) readStorageOutline() (int, []storageAdapter, error) {
	config, _ := core.LoadConfig(s.paths())
	head, ok := gens.New(s.opts.HomerHome).Read()
	if !ok {
		return 0, []storageAdapter{}, nil
	}
	type workingCategory struct {
		entry        OutlineCategory
		manifestBody string
		manifest     bool
	}
	adapters := map[string]*storageAdapter{}
	categories := map[string]map[string]*workingCategory{}
	// store layout: <adapter>/<category>/<relpath>
	root := head.StoreDir
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		parts := strings.SplitN(filepath.ToSlash(rel), "/", 3)
		if len(parts) != 3 {
			return nil
		}
		adapterID, category, fileRel := parts[0], parts[1], parts[2]
		// Generations published before AppleDouble ignore still contain
		// macOS sidecar files (._advisor.md). They are not text; showing
		// them in the drawer is mojibake next to the real file.
		if isAppleDoublePath(category) || isAppleDoublePath(fileRel) {
			return nil
		}
		if adapters[adapterID] == nil {
			adapters[adapterID] = &storageAdapter{ID: adapterID}
			categories[adapterID] = map[string]*workingCategory{}
		}
		if categories[adapterID][category] == nil {
			categories[adapterID][category] = &workingCategory{entry: OutlineCategory{Name: category, Files: []OutlineFile{}}}
		}
		cat := categories[adapterID][category]
		if strings.HasSuffix(filepath.Base(fileRel), ".manifest.txt") {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			cat.manifest = true
			cat.manifestBody = string(data)
			return nil
		}
		info, statErr := entry.Info()
		size := 0
		if statErr == nil {
			size = int(info.Size())
		}
		file := OutlineFile{Path: category + "/" + fileRel, Size: size}
		if config != nil {
			if adapter, ok := config.Adapters[adapterID]; ok {
				if categoryConfig, ok := adapter.Categories[category]; ok {
					file.Destination = categoryDestination(adapter.Root, categoryConfig, fileRel)
				}
			}
		}
		cat.entry.Files = append(cat.entry.Files, file)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	ids := make([]string, 0, len(adapters))
	for id := range adapters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]storageAdapter, 0, len(ids))
	for _, id := range ids {
		adapter := adapters[id]
		catNames := make([]string, 0, len(categories[id]))
		for name := range categories[id] {
			catNames = append(catNames, name)
		}
		sort.Strings(catNames)
		adapter.Categories = make([]OutlineCategory, 0, len(catNames))
		for _, name := range catNames {
			cat := categories[id][name]
			if cat.manifest {
				presentPluginList(&cat.entry, cat.manifestBody)
			}
			if len(cat.entry.Files) == 0 {
				continue
			}
			sort.Slice(cat.entry.Files, func(i, j int) bool { return cat.entry.Files[i].Path < cat.entry.Files[j].Path })
			adapter.Categories = append(adapter.Categories, cat.entry)
		}
		if len(adapter.Categories) == 0 {
			continue
		}
		out = append(out, *adapter)
	}
	return head.Generation, out, nil
}

// categoryDestination is the tool path the console encrypts. A directory
// category stores paths relative to that directory; a file list stores
// basenames; the pi catch-all walks the adapter root.
func categoryDestination(root string, category core.CategoryConfig, fileRel string) string {
	rel := fileRel
	for _, path := range category.Paths {
		if !strings.HasSuffix(path, "/") {
			continue
		}
		dir := strings.TrimSuffix(path, "/")
		if dir == "." || dir == "" {
			rel = fileRel
		} else {
			rel = dir + "/" + fileRel
		}
		break
	}
	root = strings.TrimRight(strings.ReplaceAll(root, "\\", "/"), "/")
	if root == "" {
		return rel
	}
	return root + "/" + rel
}

func presentPluginList(cat *OutlineCategory, content string) {
	cat.Kind = "manifest"
	cat.Label = "插件"
	names := pluginNames(content)
	cat.Files = make([]OutlineFile, 0, len(names))
	for _, name := range names {
		cat.Files = append(cat.Files, OutlineFile{Path: name})
	}
}

func pluginNames(content string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		out = append(out, line)
	}
	sort.Strings(out)
	return out
}

func attachStorageOutline(choices []AdapterChoice, adapters []storageAdapter) {
	byID := map[string][]OutlineCategory{}
	for _, adapter := range adapters {
		byID[adapter.ID] = adapter.Categories
	}
	for i := range choices {
		cats, ok := byID[choices[i].ID]
		if ok && len(cats) > 0 {
			choices[i].Categories = cats
		}
	}
}

// handleStorageFile streams one stored file's content for the drawer's
// file preview.
func (s *Server) handleStorageFile(w http.ResponseWriter, r *http.Request) {
	adapterID := strings.TrimSpace(r.URL.Query().Get("adapter"))
	filePath := strings.TrimSpace(r.URL.Query().Get("path"))
	if adapterID == "" || filePath == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "adapter 与 path 不能为空", nil)
		return
	}
	head, ok := gens.New(s.opts.HomerHome).Read()
	if !ok {
		writeError(w, http.StatusNotFound, "not-found", "服务器还没有存储内容", nil)
		return
	}
	// path arrives as "<category>/<relpath>"; the store layout nests the
	// adapter at the top. Refuse traversal before joining.
	cleaned := filepath.Clean("/" + filePath)
	if strings.Contains(cleaned, "..") {
		writeError(w, http.StatusBadRequest, "bad-request", "非法路径", nil)
		return
	}
	if isAppleDoublePath(filePath) {
		writeJSON(w, http.StatusOK, map[string]any{"binary": true, "content": ""})
		return
	}
	full := filepath.Join(head.StoreDir, adapterID, strings.TrimPrefix(cleaned, string(filepath.Separator)))
	data, err := os.ReadFile(full)
	if err != nil {
		writeError(w, http.StatusNotFound, "not-found", "文件不存在: "+filePath, nil)
		return
	}
	if !storageText(data) {
		writeJSON(w, http.StatusOK, map[string]any{"binary": true, "content": ""})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": string(data)})
}

// storedFileText reads one center file. ok is false when the generation
// or the file is missing; binary is true when the bytes are not text.
func (s *Server) storedFileText(adapterID, filePath string) (content string, ok bool, binary bool) {
	adapterID = strings.TrimSpace(adapterID)
	filePath = strings.TrimSpace(filePath)
	if adapterID == "" || filePath == "" || strings.Contains(filePath, "..") || isAppleDoublePath(filePath) {
		return "", false, isAppleDoublePath(filePath)
	}
	head, exists := gens.New(s.opts.HomerHome).Read()
	if !exists {
		return "", false, false
	}
	cleaned := filepath.Clean("/" + filePath)
	if strings.Contains(cleaned, "..") {
		return "", false, false
	}
	full := filepath.Join(head.StoreDir, adapterID, strings.TrimPrefix(cleaned, string(filepath.Separator)))
	data, err := os.ReadFile(full)
	if err != nil {
		return "", false, false
	}
	if !storageText(data) {
		return "", true, true
	}
	return string(data), true, false
}

// isAppleDoublePath reports macOS sidecar names (._foo) at any depth.
func isAppleDoublePath(rel string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "/")
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, "._") {
			return true
		}
	}
	return false
}

// storageText is content the drawer can show. NUL and invalid UTF-8 are
// the AppleDouble / binary case: rendering those bytes looks like mojibake.
func storageText(data []byte) bool {
	return utf8.Valid(data) && !bytes.Contains(data, []byte{0})
}
