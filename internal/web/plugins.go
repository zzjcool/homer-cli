package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/keyring"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
	"github.com/zzjcool/homer-cli/internal/pluginregistry"
	"github.com/zzjcool/homer-cli/internal/pluginruntime"
)

const pluginRequestLimit = 1 << 20

// ReportedAdaptersSource is the optional agent-registry capability used to
// annotate the plugin table. It deliberately lives in web as a structural
// interface so web does not import the hub package.
type ReportedAdaptersSource interface {
	AgentReportedAdapters(agentID string) []string
}

type pluginListItem struct {
	pluginregistry.Plugin
	MachineCount *int `json:"machineCount,omitempty"`
}

type pluginsListResponse struct {
	SchemaVersion int              `json:"schemaVersion"`
	Installed     []pluginListItem `json:"installed"`
	Available     []pluginListItem `json:"available"`
}

type pluginInstallRequest struct {
	ID       string          `json:"id"`
	Manifest json.RawMessage `json:"manifest"`
}

func (s *Server) handlePluginsAPI(w http.ResponseWriter, r *http.Request, path string) {
	switch path {
	case "/api/plugins":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		s.handlePluginsList(w)
	case "/api/plugins/install":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		s.handlePluginInstall(w, r)
	case "/api/plugins/uninstall":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		s.handlePluginUninstall(w, r)
	default:
		writeError(w, http.StatusNotFound, "not-found", "请求的资源不存在", nil)
	}
}

func (s *Server) handlePluginsList(w http.ResponseWriter) {
	response := pluginsListResponse{
		SchemaVersion: 1,
		Installed:     []pluginListItem{},
		Available:     []pluginListItem{},
	}
	// Old embedders that do not pass plugin state retain their old behavior:
	// the new endpoint exists, but advertises no installable state.
	if s.opts.Plugins == nil {
		writeJSON(w, http.StatusOK, response)
		return
	}

	counts := s.reportedAdapterCounts()
	countFor := func(plugin pluginregistry.Plugin) *int {
		if counts == nil {
			return nil
		}
		count := counts[plugin.ID]
		return &count
	}
	installedIDs := make(map[string]struct{})
	for _, plugin := range s.opts.Plugins.List() {
		installedIDs[plugin.ID] = struct{}{}
		response.Installed = append(response.Installed, pluginListItem{
			Plugin: plugin, MachineCount: countFor(plugin),
		})
	}
	for _, plugin := range pluginregistry.Builtins() {
		if _, installed := installedIDs[plugin.ID]; installed {
			continue
		}
		response.Available = append(response.Available, pluginListItem{
			Plugin: plugin, MachineCount: countFor(plugin),
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) reportedAdapterCounts() map[string]int {
	source, ok := s.opts.Agents.(ReportedAdaptersSource)
	if !ok || s.opts.Agents == nil {
		return nil
	}
	counts := map[string]int{}
	for _, agent := range s.opts.Agents.ListAgents() {
		if agent.Stale {
			continue
		}
		seen := map[string]struct{}{}
		for _, id := range source.AgentReportedAdapters(agent.AgentID) {
			if id != "" {
				seen[id] = struct{}{}
			}
		}
		for id := range seen {
			counts[id]++
		}
	}
	return counts
}

// installedCredentialRules derives the web credential view from the official
// plugin registry instead of maintaining a second hand-written list. A nil
// plugin state is the legacy embedding mode and keeps the pre-plugin view.
func (s *Server) installedCredentialRules() []CredentialRule {
	plugins := pluginregistry.Builtins()
	if s != nil && s.opts.Plugins != nil {
		installed := map[string]struct{}{}
		for _, plugin := range s.opts.Plugins.List() {
			installed[plugin.ID] = struct{}{}
		}
		filtered := make([]pluginregistry.Plugin, 0, len(plugins))
		for _, plugin := range plugins {
			if _, ok := installed[plugin.ID]; ok {
				filtered = append(filtered, plugin)
			}
		}
		plugins = filtered
	}
	return credentialRulesFromPlugins(plugins)
}

func (s *Server) handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	if s.opts.Plugins == nil {
		writePluginsDisabled(w)
		return
	}
	payload, err := decodePluginInstallRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
		return
	}

	var plugin pluginregistry.Plugin
	if len(payload.Manifest) > 0 && string(bytes.TrimSpace(payload.Manifest)) != "null" {
		if strings.TrimSpace(payload.ID) != "" {
			writeError(w, http.StatusBadRequest, "bad-request", "id 与 manifest 不能同时提供", nil)
			return
		}
		var code string
		var details []string
		plugin, code, details, err = parseThirdPartyManifest(payload.Manifest)
		if err != nil {
			status := http.StatusUnprocessableEntity
			if code == "plugin-conflict" {
				status = http.StatusConflict
			}
			writeError(w, status, code, err.Error(), details)
			return
		}
	} else {
		id := strings.TrimSpace(payload.ID)
		if id == "" {
			writeError(w, http.StatusBadRequest, "bad-request", "必须提供 id 或 manifest", nil)
			return
		}
		var ok bool
		plugin, ok = pluginregistry.Builtin(id)
		if !ok {
			writeError(w, http.StatusNotFound, "plugin-not-found", "官方插件不存在；第三方插件请提供 manifest", nil)
			return
		}
	}

	writeMutex.Lock()
	defer writeMutex.Unlock()
	if s.opts.Plugins.IsInstalled(plugin.ID) {
		writeError(w, http.StatusConflict, "plugin-installed", "插件已经安装", []string{plugin.ID})
		return
	}
	if err := s.opts.Plugins.Install(plugin); err != nil {
		s.writePluginStateError(w, err)
		return
	}
	if plugin.Role == pluginregistry.RoleAdapter || plugin.Role == pluginregistry.RoleCarrier {
		if err := s.publishPluginConfiguration(plugin.ID, plugin.Adapter); err != nil {
			rollbackErr := s.opts.Plugins.Uninstall(plugin.ID)
			details := []string{err.Error()}
			if rollbackErr != nil {
				details = append(details, "回滚插件状态失败: "+rollbackErr.Error())
			}
			writeError(w, http.StatusInternalServerError, "generation-publish-failed", "发布插件配置世代失败", details)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "plugin": plugin})
}

func (s *Server) writePluginStateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pluginruntime.ErrAlreadyInstalled):
		writeError(w, http.StatusConflict, "plugin-installed", "插件已经安装", []string{err.Error()})
	case errors.Is(err, pluginruntime.ErrInvalidID), errors.Is(err, pluginruntime.ErrInvalidPlugin):
		writeError(w, http.StatusUnprocessableEntity, "manifest-invalid", "插件 manifest 无效", []string{err.Error()})
	default:
		writeError(w, http.StatusInternalServerError, "plugin-state-failed", "保存插件状态失败", []string{err.Error()})
	}
}

func (s *Server) handlePluginUninstall(w http.ResponseWriter, r *http.Request) {
	if s.opts.Plugins == nil {
		writePluginsDisabled(w)
		return
	}
	var payload struct {
		ID    string `json:"id"`
		Force bool   `json:"force,omitempty"`
	}
	if err := decodeLimitedJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
		return
	}
	id := strings.TrimSpace(payload.ID)
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "id 不能为空", nil)
		return
	}

	writeMutex.Lock()
	defer writeMutex.Unlock()
	plugin, ok := findInstalledPlugin(s.opts.Plugins.List(), id)
	if !ok {
		writeError(w, http.StatusNotFound, "plugin-not-installed", "插件尚未安装", []string{id})
		return
	}
	if plugin.Role != pluginregistry.RoleAction {
		if !payload.Force {
			writeUninstallGuard(w, "卸载 adapter/carrier 插件需要明确设置 force=true")
			return
		}
		message, err := s.pluginUninstallGuard(plugin)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "uninstall-check-failed", "检查插件卸载条件失败", []string{err.Error()})
			return
		}
		if message != "" {
			writeUninstallGuard(w, message)
			return
		}
	}

	if err := s.opts.Plugins.Uninstall(plugin.ID); err != nil {
		s.writePluginStateError(w, err)
		return
	}
	if plugin.Role == pluginregistry.RoleAdapter || plugin.Role == pluginregistry.RoleCarrier {
		if err := s.removePluginConfiguration(plugin.ID); err != nil {
			rollbackErr := s.opts.Plugins.Install(plugin)
			details := []string{err.Error()}
			if rollbackErr != nil {
				details = append(details, "恢复插件状态失败: "+rollbackErr.Error())
			}
			writeError(w, http.StatusInternalServerError, "generation-publish-failed", "发布插件卸载世代失败", details)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "plugin": plugin})
}

func writeUninstallGuard(w http.ResponseWriter, message string) {
	writeError(w, http.StatusConflict, "uninstall-guard", message, nil)
}

func (s *Server) pluginUninstallGuard(plugin pluginregistry.Plugin) (string, error) {
	if plugin.ID == "keyring" && plugin.Role == pluginregistry.RoleCarrier {
		installedAdapters := map[string]struct{}{}
		for _, installed := range s.opts.Plugins.List() {
			if installed.Role == pluginregistry.RoleAdapter {
				installedAdapters[installed.ID] = struct{}{}
			}
		}
		listed := keyring.Apply(s.opts.HomerHome, keyring.Command{Action: "list"})
		if !listed.OK {
			return "", fmt.Errorf("读取密钥环失败: %s", strings.Join(listed.Errors, "; "))
		}
		bound := map[string]struct{}{}
		for _, key := range listed.Keys {
			for _, file := range key.Files {
				if file.Adapter == "" {
					continue
				}
				if _, installed := installedAdapters[file.Adapter]; installed {
					bound[file.Adapter] = struct{}{}
				}
			}
		}
		if len(bound) > 0 {
			adapters := make([]string, 0, len(bound))
			for id := range bound {
				adapters = append(adapters, id)
			}
			sort.Strings(adapters)
			return "仍有密钥绑定在已安装的 adapter（" + strings.Join(adapters, "、") + "）上，请先解绑密钥后再卸载密钥环插件", nil
		}
	}
	if plugin.Role == pluginregistry.RoleAdapter {
		head, exists := gens.New(s.opts.HomerHome).Read()
		if exists {
			store, err := readGenerationStore(head.StoreDir)
			if err != nil {
				return "", err
			}
			if len(store[plugin.ID]) > 0 {
				return "中心 store 仍有 " + plugin.ID + " 的数据，请先清理中心数据后再卸载（当前版本不提供清理）", nil
			}
		}
	}
	return "", nil
}

func findInstalledPlugin(plugins []pluginregistry.Plugin, id string) (pluginregistry.Plugin, bool) {
	for _, plugin := range plugins {
		if plugin.ID == id {
			return plugin, true
		}
	}
	return pluginregistry.Plugin{}, false
}

func (s *Server) publishPluginConfiguration(id string, adapter *core.AdapterConfig) error {
	if adapter == nil {
		return fmt.Errorf("插件 %q 缺少 adapter 配置", id)
	}
	return s.publishPluginGeneration(id, adapter)
}

func (s *Server) removePluginConfiguration(id string) error {
	return s.publishPluginGeneration(id, nil)
}

func (s *Server) publishPluginGeneration(id string, adapter *core.AdapterConfig) error {
	// Snapshot uploads have their own lock because they can arrive while a
	// hub-side request is running. Serialize this generation read/publish pair
	// with that transport as well as the web write mutex held by the caller.
	generationMutex.Lock()
	defer generationMutex.Unlock()
	layout := gens.New(s.opts.HomerHome)
	store := map[string]map[string]string{}
	var meta []byte
	if head, ok := layout.Read(); ok {
		meta = head.Meta
		var err error
		store, err = readGenerationStore(head.StoreDir)
		if err != nil {
			return fmt.Errorf("读取当前 generation store: %w", err)
		}
	} else {
		data, err := os.ReadFile(s.paths().ConfigFile)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("读取 hub homer.json: %w", err)
			}
			data = []byte("{\"version\":1,\"adapters\":{}}\n")
		}
		meta = data
	}
	if validated, problems := core.ValidateConfig(meta); validated == nil {
		return fmt.Errorf("当前 homer.json 无效: %s", strings.Join(problems, "; "))
	}
	value, err := orderedjson.Parse(meta)
	if err != nil {
		return fmt.Errorf("解析当前 homer.json: %w", err)
	}
	root, ok := value.(*orderedjson.Object)
	if !ok || root == nil {
		return errors.New("当前 homer.json 顶层必须是对象")
	}
	adapters, ok := root.M["adapters"].(*orderedjson.Object)
	if !ok || adapters == nil {
		return errors.New("当前 homer.json adapters 必须是对象")
	}
	if adapter == nil {
		if _, exists := adapters.M[id]; !exists {
			return nil
		}
		delete(adapters.M, id)
		adapters.Keys = removeJSONKey(adapters.Keys, id)
	} else {
		configValue := core.ConfigValue(core.HomerConfig{
			Version: 1,
			Adapters: map[string]core.AdapterConfig{
				id: *adapter,
			},
		})
		configObject, ok := configValue.(*orderedjson.Object)
		if !ok || configObject == nil {
			return fmt.Errorf("编码 adapter %q 失败", id)
		}
		adapterObjects, ok := configObject.M["adapters"].(*orderedjson.Object)
		if !ok || adapterObjects == nil {
			return fmt.Errorf("编码 adapter %q 失败", id)
		}
		adapterValue, exists := adapterObjects.M[id]
		if !exists {
			return fmt.Errorf("编码 adapter %q 失败", id)
		}
		if _, exists := adapters.M[id]; !exists {
			adapters.Keys = append(adapters.Keys, id)
		}
		adapters.M[id] = adapterValue
	}
	newMeta := orderedjson.SerializeFile(value)
	if validated, problems := core.ValidateConfig(newMeta); validated == nil {
		return fmt.Errorf("插件配置无效: %s", strings.Join(problems, "; "))
	}
	if _, err := layout.Publish(store, newMeta); err != nil {
		return fmt.Errorf("发布 generation: %w", err)
	}
	return nil
}

func removeJSONKey(keys []string, want string) []string {
	filtered := make([]string, 0, len(keys))
	for _, key := range keys {
		if key != want {
			filtered = append(filtered, key)
		}
	}
	return filtered
}

func decodePluginInstallRequest(r *http.Request) (pluginInstallRequest, error) {
	var payload pluginInstallRequest
	if err := decodeLimitedJSON(r, &payload); err != nil {
		return pluginInstallRequest{}, err
	}
	return payload, nil
}

func decodeLimitedJSON(r *http.Request, target any) error {
	if r.Body == nil {
		return errors.New("请求体为空")
	}
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, pluginRequestLimit+1))
	if err != nil {
		return errors.New("读取请求体失败")
	}
	if len(data) > pluginRequestLimit {
		return errors.New("请求体过大")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("请求体为空")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return errors.New("请求体不是合法 JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("请求体包含多个 JSON 值")
	}
	return nil
}

type thirdPartyManifest struct {
	SchemaVersion json.RawMessage     `json:"schemaVersion"`
	ID            string              `json:"id"`
	Role          pluginregistry.Role `json:"role"`
	Name          string              `json:"name"`
	Description   string              `json:"description"`
	Root          json.RawMessage     `json:"root"`
	Enabled       json.RawMessage     `json:"enabled"`
	Categories    json.RawMessage     `json:"categories"`
	Ignore        json.RawMessage     `json:"ignore"`
	AllowEscape   json.RawMessage     `json:"allowEscape"`
}

func parseThirdPartyManifest(raw json.RawMessage) (pluginregistry.Plugin, string, []string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest thirdPartyManifest
	if err := decoder.Decode(&manifest); err != nil {
		return pluginregistry.Plugin{}, "manifest-invalid", nil, fmt.Errorf("manifest 不是有效的 schemaVersion=1 JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return pluginregistry.Plugin{}, "manifest-invalid", nil, errors.New("manifest 包含多个 JSON 值")
	}
	var version int
	if len(manifest.SchemaVersion) == 0 || json.Unmarshal(manifest.SchemaVersion, &version) != nil || version != 1 {
		return pluginregistry.Plugin{}, "schema-version", nil, errors.New("第三方 manifest 的 schemaVersion 必须是 1")
	}
	if manifest.Role != pluginregistry.RoleAdapter {
		return pluginregistry.Plugin{}, "third-party-role", nil, errors.New("第三方插件目前只支持 role=adapter")
	}
	if !core.ValidAdapterID(manifest.ID) {
		return pluginregistry.Plugin{}, "manifest-invalid", nil, errors.New("manifest id 必须匹配 ^[a-z][a-z0-9-]*$")
	}
	if _, official := pluginregistry.Builtin(manifest.ID); official {
		return pluginregistry.Plugin{}, "plugin-conflict", nil, errors.New("第三方 manifest 的 id 与官方插件冲突")
	}

	adapterShape := map[string]json.RawMessage{}
	for key, value := range map[string]json.RawMessage{
		"root": manifest.Root, "enabled": manifest.Enabled, "categories": manifest.Categories,
		"ignore": manifest.Ignore, "allowEscape": manifest.AllowEscape,
	} {
		if len(value) > 0 {
			adapterShape[key] = value
		}
	}
	adapterJSON, err := json.Marshal(adapterShape)
	if err != nil {
		return pluginregistry.Plugin{}, "manifest-invalid", nil, fmt.Errorf("编码 adapter 配置: %w", err)
	}
	configJSON, err := json.Marshal(struct {
		Version  int                        `json:"version"`
		Adapters map[string]json.RawMessage `json:"adapters"`
	}{Version: 1, Adapters: map[string]json.RawMessage{manifest.ID: adapterJSON}})
	if err != nil {
		return pluginregistry.Plugin{}, "manifest-invalid", nil, fmt.Errorf("编码 manifest 配置: %w", err)
	}
	config, problems := core.ValidateConfig(configJSON)
	if config == nil {
		return pluginregistry.Plugin{}, "manifest-invalid", problems, errors.New("manifest 的 root/categories 配置未通过校验")
	}
	name := manifest.Name
	if name == "" {
		name = manifest.ID
	}
	adapterConfig := config.Adapters[manifest.ID]
	return pluginregistry.Plugin{
		ID:      manifest.ID,
		Role:    pluginregistry.RoleAdapter,
		Name:    name,
		Desc:    manifest.Description,
		Adapter: &adapterConfig,
	}, "", nil, nil
}

func writePluginsDisabled(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "plugins-disabled", "插件管理未启用", nil)
}
