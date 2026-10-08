package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/keyring"
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
	// Credentials are the adapter's credential files that never sync in
	// plaintext. A collect shows them so the user can encrypt them first.
	Credentials []CredentialRule `json:"credentials,omitempty"`
	// SecretHits are files the machine's scanner would refuse on collect.
	SecretHits []SecretHit `json:"secretHits,omitempty"`
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
// Keys names the JSON fields inside a merge file that make up a drift
// count (two pending keys in one settings.json are still two items).
type OutlineFile struct {
	Path   string   `json:"path"`
	Size   int      `json:"size,omitempty"`
	Status string   `json:"status,omitempty"`
	Keys   []string `json:"keys,omitempty"`
	// Destination is the tool path (~/...) a review action encrypts.
	Destination string `json:"destination,omitempty"`
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

// stampChoiceDrift copies the machine's per-category and per-file drift
// onto a storage outline. Storage lists every file and knows nothing
// about which of them this machine still needs.
func stampChoiceDrift(choices []AdapterChoice, machine []commands.StatusAdapterReport) {
	byID := map[string][]commands.StatusCategoryReport{}
	for _, adapter := range machine {
		if _, ok := byID[adapter.ID]; ok {
			continue
		}
		byID[adapter.ID] = adapter.Categories
	}
	for i := range choices {
		status, ok := byID[choices[i].ID]
		if !ok {
			continue
		}
		choices[i].Categories = mergeDriftIntoOutline(choices[i].Categories, status)
	}
}

func mergeDriftIntoOutline(outline []OutlineCategory, status []commands.StatusCategoryReport) []OutlineCategory {
	if len(status) == 0 {
		return outline
	}
	out := append([]OutlineCategory(nil), outline...)
	index := map[string]int{}
	for i := range out {
		index[out[i].Name] = i
	}
	for _, src := range status {
		i, ok := index[src.Name]
		if !ok {
			if src.Push == 0 && src.Pull == 0 && src.Conflicts == 0 {
				continue
			}
			item := OutlineCategory{Name: src.Name, Kind: src.Kind, Files: []OutlineFile{}}
			if src.Kind == "manifest" {
				item.Label = "插件"
			}
			out = append(out, item)
			i = len(out) - 1
			index[src.Name] = i
		}
		cat := &out[i]
		cat.Push = src.Push
		cat.Pull = src.Pull
		cat.Conflicts = src.Conflicts
		if src.Kind == "manifest" {
			cat.Kind = "manifest"
			cat.Label = "插件"
		}
		cat.Files = applyDriftToFiles(cat.Name, cat.Kind, cat.Files, src.Files)
	}
	return out
}

func applyDriftToFiles(category, kind string, files []OutlineFile, reports []commands.StatusFileReport) []OutlineFile {
	hits := map[string]*driftHit{}
	for _, report := range reports {
		if !notableDriftStatus(report.Status) {
			continue
		}
		rel, key := splitDriftPath(report.Path)
		hit := hits[rel]
		if hit == nil {
			hit = &driftHit{}
			hits[rel] = hit
		}
		hit.status = strongerDriftStatus(hit.status, report.Status)
		if key != "" {
			hit.keys = appendKey(hit.keys, key)
		}
	}
	matched := map[string]struct{}{}
	for i := range files {
		rel := outlineRel(category, kind, files[i].Path)
		matched[rel] = struct{}{}
		hit := hits[rel]
		if hit == nil {
			continue
		}
		files[i].Status = hit.status
		files[i].Keys = append([]string(nil), hit.keys...)
	}
	extras := make([]string, 0)
	for rel := range hits {
		if _, ok := matched[rel]; ok {
			continue
		}
		extras = append(extras, rel)
	}
	sort.Strings(extras)
	for _, rel := range extras {
		hit := hits[rel]
		files = append(files, OutlineFile{
			Path:   outlineStorePath(category, kind, rel),
			Status: hit.status,
			Keys:   append([]string(nil), hit.keys...),
		})
	}
	return files
}

type driftHit struct {
	status string
	keys   []string
}

func notableDriftStatus(status string) bool {
	switch status {
	case "push", "pull", "conflict", "delete", "push-delete", "pull-delete", "changed":
		return true
	default:
		return false
	}
}

func strongerDriftStatus(current, next string) string {
	if driftStatusRank(next) > driftStatusRank(current) {
		return next
	}
	return current
}

func driftStatusRank(status string) int {
	switch status {
	case "conflict":
		return 4
	case "pull", "pull-delete":
		return 3
	case "push", "push-delete", "changed":
		return 2
	default:
		return 0
	}
}

// splitDriftPath separates a merge key (settings.json:theme) from a file
// path. Package ids such as npm:pi-lens also contain a colon; those stay
// whole because the part before the colon is not a file name.
func splitDriftPath(path string) (rel, key string) {
	colon := strings.Index(path, ":")
	if colon <= 0 {
		return path, ""
	}
	head := path[:colon]
	if !strings.ContainsAny(head, "./") {
		return path, ""
	}
	return head, path[colon+1:]
}

func appendKey(keys []string, key string) []string {
	for _, existing := range keys {
		if existing == key {
			return keys
		}
	}
	keys = append(keys, key)
	sort.Strings(keys)
	return keys
}

func outlineRel(category, kind, path string) string {
	if kind == "manifest" {
		return path
	}
	prefix := category + "/"
	if strings.HasPrefix(path, prefix) {
		return strings.TrimPrefix(path, prefix)
	}
	return path
}

func outlineStorePath(category, kind, rel string) string {
	if kind == "manifest" || category == "" || strings.HasPrefix(rel, category+"/") {
		return rel
	}
	return category + "/" + rel
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

type syncChoicesResponse struct {
	OK        bool            `json:"ok"`
	Direction string          `json:"direction"`
	AgentID   string          `json:"agentId"`
	Hint      string          `json:"hint"`
	Adapters  []AdapterChoice `json:"adapters"`
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
	if direction == "collect" {
		s.handleCollectChoices(w, r, agentID, r.URL.Query().Get("stream") == "1")
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
	response, err := s.buildSyncChoicesResponse(direction, agentID, report)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage-read", "读取存储内容失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) buildSyncChoicesResponse(direction, agentID string, report commands.StatusReport) (syncChoicesResponse, error) {
	choices := []AdapterChoice{}
	hint := ""
	switch direction {
	case "dispatch":
		ids, published, err := s.centerAdapterIDs()
		if err != nil {
			return syncChoicesResponse{}, err
		}
		if !published {
			hint = "中心还没有任何内容。先从一台机器收取。"
		} else {
			choices = BuildDispatchChoices(ids, report.Adapters)
			if _, adapters, outlineErr := s.readStorageOutline(); outlineErr == nil {
				attachStorageOutline(choices, adapters)
			}
			// The storage tree is the full inventory and has no drift.
			// Stamp the machine's status onto it so "↓2 待下发" names the
			// two files instead of sitting only on the adapter row.
			stampChoiceDrift(choices, report.Adapters)
			if len(choices) == 0 {
				hint = "中心还没有可下发的适配器。"
			} else {
				hint = "只会把勾选的适配器写到这台机器。其他适配器这次不动。带密钥的适配器要先填口令，口令能解开才会下发。"
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
	return syncChoicesResponse{OK: true, Direction: direction, AgentID: agentID, Hint: hint, Adapters: choices}, nil
}

func (s *Server) handleCollectChoices(w http.ResponseWriter, r *http.Request, agentID string, streaming bool) {
	inspector, supportsInspect := s.opts.Agents.(InspectSource)
	if streaming {
		s.handleCollectChoicesStream(w, r, agentID, inspector, supportsInspect)
		return
	}

	var report commands.StatusReport
	var present map[string]bool
	var secrets []InspectSecret
	if supportsInspect {
		result, err := inspector.AgentInspect(r.Context(), agentID, collectInspectParams(), nil)
		if err != nil {
			writeErrorValue(w, err)
			return
		}
		report, present, secrets = result.Status, result.Present, result.Secrets
	} else {
		raw, err := s.opts.Agents.AgentStatus(r.Context(), agentID)
		if err != nil {
			writeErrorValue(w, err)
			return
		}
		var parseErr error
		report, parseErr = parseStatusReport(raw)
		if parseErr != nil {
			writeError(w, http.StatusBadGateway, "agent-unreachable", parseErr.Error(), nil)
			return
		}
	}
	response := s.buildCollectChoicesResponse(agentID, report, present, secrets)
	writeJSON(w, http.StatusOK, response)
}

func collectInspectParams() InspectParams {
	paths := make([]string, 0)
	for _, rule := range credentialRules() {
		paths = append(paths, rule.Destination)
	}
	return InspectParams{Credentials: paths, WantKeys: true}
}

func (s *Server) buildCollectChoicesResponse(agentID string, report commands.StatusReport, present map[string]bool, secrets []InspectSecret) syncChoicesResponse {
	choices := BuildCollectChoices(report.Adapters)
	s.enrichCollectChoices(choices, present, secrets)
	if choices == nil {
		choices = []AdapterChoice{}
	}
	return syncChoicesResponse{
		OK:        true,
		Direction: "collect",
		AgentID:   agentID,
		Hint:      collectChoiceHint(report, choices),
		Adapters:  choices,
	}
}

func (s *Server) enrichCollectChoices(choices []AdapterChoice, present map[string]bool, secrets []InspectSecret) {
	applyCredentialProbe(choices, present)
	config, _ := core.LoadConfig(s.paths())
	hits := inspectSecretHits(config, secrets)
	for i := range choices {
		choices[i].SecretHits = append([]SecretHit(nil), hits[choices[i].ID]...)
	}
}

func inspectSecretHits(config *core.HomerConfig, secrets []InspectSecret) map[string][]SecretHit {
	hits := make(map[string][]SecretHit)
	seen := make(map[string]struct{}, len(secrets))
	for _, secret := range secrets {
		if _, ok := seen[secret.Path]; ok {
			continue
		}
		seen[secret.Path] = struct{}{}
		adapterID, destination := secretDestination(config, secret.Path)
		hits[adapterID] = append(hits[adapterID], SecretHit{
			Path: secret.Path, Destination: destination, Reason: secret.Description, Line: secret.Line,
		})
	}
	return hits
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
	return "只会把勾选的适配器写入中心。没勾选的适配器保持中心现有内容。绑在适配器上的密钥会跟着走。"
}

func (s *Server) handleCollectChoicesStream(w http.ResponseWriter, r *http.Request, agentID string, inspector InspectSource, supportsInspect bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "stream-unavailable", "服务器不支持流式响应", nil)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	var mu sync.Mutex
	var writeErr error
	writeLine := func(value any) bool {
		data, err := json.Marshal(value)
		if err != nil {
			writeErr = err
			cancel()
			return false
		}
		if ctx.Err() != nil || writeErr != nil {
			return false
		}
		data = append(data, '\n')
		if _, err := w.Write(data); err != nil {
			writeErr = err
			cancel()
			return false
		}
		flusher.Flush()
		return ctx.Err() == nil
	}
	writeErrorEvent := func(err error) {
		_, code := errorStatusCode(err)
		message := errorMessage(code)
		if code == "internal" && err != nil {
			message = err.Error()
		}
		writeLine(map[string]any{"type": "error", "code": code, "message": message})
	}
	if !supportsInspect {
		raw, err := s.opts.Agents.AgentStatus(ctx, agentID)
		if err != nil {
			writeErrorEvent(err)
			return
		}
		report, err := parseStatusReport(raw)
		if err != nil {
			writeErrorEvent(&AgentError{Code: "agent-unreachable", Status: http.StatusBadGateway, Err: err})
			return
		}
		response := s.buildCollectChoicesResponse(agentID, report, nil, nil)
		for i, adapter := range response.Adapters {
			if !writeLine(map[string]any{"type": "adapter", "adapter": adapter, "done": i + 1, "total": len(response.Adapters)}) {
				return
			}
		}
		writeLine(map[string]any{"type": "keys", "keys": []any{}})
		writeLine(map[string]any{"type": "done", "hint": response.Hint, "adapters": response.Adapters})
		return
	}

	var currentChoices []AdapterChoice
	var present map[string]bool
	var secrets []InspectSecret
	var lastKeys *keyring.Result
	writePrecheck := func() bool {
		credentials := make(map[string][]CredentialRule, len(currentChoices))
		secretHits := make(map[string][]SecretHit, len(currentChoices))
		for _, choice := range currentChoices {
			credentials[choice.ID] = choice.Credentials
			secretHits[choice.ID] = choice.SecretHits
		}
		return writeLine(map[string]any{"type": "precheck", "credentials": credentials, "secretHits": secretHits})
	}
	writeKeys := func(result *keyring.Result) bool {
		keys := []keyring.Summary{}
		if result != nil && result.Keys != nil {
			keys = result.Keys
		}
		return writeLine(map[string]any{"type": "keys", "keys": keys})
	}

	result, err := inspector.AgentInspect(ctx, agentID, collectInspectParams(), func(event InspectEvent) {
		mu.Lock()
		defer mu.Unlock()
		if ctx.Err() != nil || writeErr != nil {
			return
		}
		switch event.Stage {
		case "adapter":
			if event.Adapter == nil {
				return
			}
			rows := BuildCollectChoices([]commands.StatusAdapterReport{*event.Adapter})
			if len(rows) == 0 {
				return
			}
			s.enrichCollectChoices(rows, present, secrets)
			updated := false
			for i := range currentChoices {
				if currentChoices[i].ID == rows[0].ID {
					currentChoices[i] = rows[0]
					updated = true
					break
				}
			}
			if !updated {
				currentChoices = append(currentChoices, rows[0])
			}
			_ = writeLine(map[string]any{
				"type": "adapter", "adapter": rows[0], "done": event.Done, "total": event.Total,
			})
		case "credentials":
			if event.Present != nil {
				if present == nil {
					present = make(map[string]bool)
				}
				for path, exists := range event.Present {
					present[path] = exists
				}
			}
			s.enrichCollectChoices(currentChoices, present, secrets)
			_ = writePrecheck()
		case "secrets":
			secrets = appendInspectSecrets(secrets, event.Secrets)
			s.enrichCollectChoices(currentChoices, present, secrets)
			_ = writePrecheck()
		case "keys":
			if event.Keys != nil {
				lastKeys = event.Keys
				_ = writeKeys(lastKeys)
			}
		}
	})
	if err != nil {
		mu.Lock()
		defer mu.Unlock()
		if ctx.Err() == nil && writeErr == nil {
			writeErrorEvent(err)
		}
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if ctx.Err() != nil || writeErr != nil {
		return
	}
	if result.Present != nil {
		present = result.Present
	}
	if result.Secrets != nil {
		secrets = result.Secrets
	}
	if result.Keys != nil {
		lastKeys = result.Keys
	}
	response := s.buildCollectChoicesResponse(agentID, result.Status, present, secrets)
	if result.Present != nil || result.Secrets != nil {
		currentChoices = append([]AdapterChoice(nil), response.Adapters...)
		_ = writePrecheck()
	}
	_ = writeKeys(lastKeys)
	writeLine(map[string]any{"type": "done", "hint": response.Hint, "adapters": response.Adapters})
}

func appendInspectSecrets(existing, next []InspectSecret) []InspectSecret {
	seen := make(map[string]struct{}, len(existing)+len(next))
	for _, item := range existing {
		seen[item.Path] = struct{}{}
	}
	for _, item := range next {
		if _, ok := seen[item.Path]; ok {
			continue
		}
		seen[item.Path] = struct{}{}
		existing = append(existing, item)
	}
	return existing
}
