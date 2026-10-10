package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/keyring"
	"github.com/zzjcool/homer-cli/internal/pluginregistry"
	"github.com/zzjcool/homer-cli/internal/pluginruntime"
)

func TestPluginsListNilStateIsEmpty(t *testing.T) {
	home := pluginTestHome(t)
	server := newPluginTestServer(t, home, nil, nil)
	response := request(t, server.Handler(), http.MethodGet, "/api/plugins")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/plugins = %d %s", response.Code, response.Body)
	}
	var body pluginsListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.SchemaVersion != 1 || len(body.Installed) != 0 || len(body.Available) != 0 {
		t.Fatalf("nil-state response = %+v, want schema 1 with empty tables", body)
	}
}

func TestPluginsListReportsOptionalOnlineMachineCounts(t *testing.T) {
	home := pluginTestHome(t)
	state := pluginruntime.New(home)
	if err := state.Install(pluginByID(t, "pi")); err != nil {
		t.Fatal(err)
	}
	source := &pluginReportedAdapterSource{
		sourceStub: &sourceStub{list: []AgentInfo{
			{AgentID: "box-a"},
			{AgentID: "box-b"},
			{AgentID: "stale", Stale: true},
		}},
		reported: map[string][]string{
			"box-a": {"pi", "pi", "opencode"},
			"box-b": {"pi"},
			"stale": {"pi"},
		},
	}
	server := newPluginTestServer(t, home, state, source)
	response := request(t, server.Handler(), http.MethodGet, "/api/plugins")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/plugins = %d %s", response.Code, response.Body)
	}
	var body pluginsListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Installed) != 1 || body.Installed[0].ID != "pi" || body.Installed[0].MachineCount == nil || *body.Installed[0].MachineCount != 2 {
		t.Fatalf("installed = %+v, want pi on two online machines", body.Installed)
	}
	var opencode *pluginListItem
	for i := range body.Available {
		if body.Available[i].ID == "opencode" {
			opencode = &body.Available[i]
			break
		}
	}
	if opencode == nil || opencode.MachineCount == nil || *opencode.MachineCount != 1 {
		t.Fatalf("available opencode = %+v, want one online machine", opencode)
	}

	withoutCapability := newPluginTestServer(t, home, state, &sourceStub{})
	omitted := request(t, withoutCapability.Handler(), http.MethodGet, "/api/plugins")
	var noCounts pluginsListResponse
	if err := json.Unmarshal(omitted.Body.Bytes(), &noCounts); err != nil {
		t.Fatal(err)
	}
	if noCounts.Installed[0].MachineCount != nil || noCounts.Available[0].MachineCount != nil {
		t.Fatalf("machineCount should be omitted when the optional source is absent: %+v %+v", noCounts.Installed, noCounts.Available)
	}
}

func TestInstalledCredentialRulesFollowInstalledOfficialAdapters(t *testing.T) {
	home := pluginTestHome(t)
	state := pluginruntime.New(home)
	server := newPluginTestServer(t, home, state, nil)
	if got := server.installedCredentialRules(); len(got) != 0 {
		t.Fatalf("credential rules before install = %#v, want none", got)
	}
	if err := state.Install(pluginByID(t, "keyring")); err != nil {
		t.Fatal(err)
	}
	if got := server.installedCredentialRules(); len(got) != 0 {
		t.Fatalf("carrier credential rules = %#v, want none", got)
	}
	for _, id := range []string{"pi", "opencode"} {
		if err := state.Install(pluginByID(t, id)); err != nil {
			t.Fatal(err)
		}
	}
	got := server.installedCredentialRules()
	if len(got) != 3 || got[0].Adapter != "pi" || got[1].Adapter != "pi" || got[2].Adapter != "opencode" {
		t.Fatalf("installed credential rules = %#v, want pi's two files and opencode auth.json", got)
	}
	legacy := newPluginTestServer(t, home, nil, nil)
	if got := legacy.installedCredentialRules(); len(got) != 3 {
		t.Fatalf("legacy nil-state credential rules = %#v, want all built-in rules", got)
	}
}

func TestPluginInstallPublishesGenerationAndPreservesStore(t *testing.T) {
	home := pluginTestHome(t)
	paths := pluginTestPaths(t, home)
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"legacy": {
				Root: "~/.legacy",
				Categories: map[string]core.CategoryConfig{
					"files": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
				},
			},
		},
	}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	originalStore := map[string]map[string]string{
		"legacy":   {"files/settings.json": "legacy-data"},
		"retained": {"files/data.txt": "keep-this"},
	}
	meta, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if generation, err := gens.New(home).Publish(originalStore, meta); err != nil || generation != 1 {
		t.Fatalf("initial Publish = (%d, %v), want generation 1", generation, err)
	}

	state := pluginruntime.New(home)
	server := newPluginTestServer(t, home, state, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/plugins/install", `{"id":"pi"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("install pi = %d %s", response.Code, response.Body)
	}
	if !state.IsInstalled("pi") {
		t.Fatal("pi was not installed")
	}
	head, ok := gens.New(home).Read()
	if !ok || head.Generation != 2 {
		t.Fatalf("generation head = %+v, ok=%v; want generation 2", head, ok)
	}
	stored, err := readGenerationStore(head.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, originalStore) {
		t.Fatalf("store after plugin install = %#v, want %#v", stored, originalStore)
	}
	published, problems := core.ValidateConfig(head.Meta)
	if published == nil {
		t.Fatalf("published homer.json invalid: %v", problems)
	}
	if _, exists := published.Adapters["legacy"]; !exists {
		t.Fatalf("install removed pre-existing adapter config: %+v", published.Adapters)
	}
	pi, exists := published.Adapters["pi"]
	if !exists || pi.Root != pluginByID(t, "pi").Adapter.Root {
		t.Fatalf("published pi adapter = %+v, exists=%v", pi, exists)
	}
	local, err := core.LoadConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := local.Adapters["pi"]; exists {
		t.Fatal("adapter install wrote the hub's local homer.json instead of publishing a generation")
	}
}

func TestPluginInstallUsesHubConfigWhenNoGenerationExists(t *testing.T) {
	home := pluginTestHome(t)
	paths := pluginTestPaths(t, home)
	if err := core.SaveConfig(paths, core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"legacy": {
				Root: "~/.legacy",
				Categories: map[string]core.CategoryConfig{
					"files": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	server := newPluginTestServer(t, home, pluginruntime.New(home), nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/plugins/install", `{"id":"vscode"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("install vscode = %d %s", response.Code, response.Body)
	}
	head, ok := gens.New(home).Read()
	if !ok || head.Generation != 1 {
		t.Fatalf("generation head = %+v, ok=%v; want generation 1", head, ok)
	}
	config, problems := core.ValidateConfig(head.Meta)
	if config == nil {
		t.Fatalf("published homer.json invalid: %v", problems)
	}
	if _, exists := config.Adapters["legacy"]; !exists {
		t.Fatalf("install lost current hub adapter config: %+v", config.Adapters)
	}
	if _, exists := config.Adapters["vscode"]; !exists {
		t.Fatalf("install did not add vscode to hub config: %+v", config.Adapters)
	}
}

func TestPluginInstallStartsGenerationFromEmptyConfigWhenNeeded(t *testing.T) {
	home := pluginTestHome(t)
	state := pluginruntime.New(home)
	server := newPluginTestServer(t, home, state, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/plugins/install", `{"id":"keyring"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("install keyring = %d %s", response.Code, response.Body)
	}
	head, ok := gens.New(home).Read()
	if !ok || head.Generation != 1 {
		t.Fatalf("generation head = %+v, ok=%v; want generation 1", head, ok)
	}
	config, problems := core.ValidateConfig(head.Meta)
	if config == nil {
		t.Fatalf("published homer.json invalid: %v", problems)
	}
	if _, exists := config.Adapters["keyring"]; !exists {
		t.Fatalf("empty start config lacks keyring adapter: %+v", config.Adapters)
	}
	store, err := readGenerationStore(head.StoreDir)
	if err != nil || len(store) != 0 {
		t.Fatalf("initial store = %#v, error=%v; want empty", store, err)
	}
}

func TestThirdPartyManifestInstallPersistsAcrossRuntimeRestart(t *testing.T) {
	home := pluginTestHome(t)
	state := pluginruntime.New(home)
	server := newPluginTestServer(t, home, state, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/plugins/install", `{"manifest":{"schemaVersion":1,"id":"custom-editor","role":"adapter","name":"Custom editor","description":"Editor settings","root":"~/.custom-editor","categories":{"packages":{"mode":"mirror","kind":"manifest","listCmd":"plugins list","applyCmd":"plugins add","idPattern":"^[a-z][a-z0-9-]*$"}}}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("install custom plugin = %d %s", response.Code, response.Body)
	}
	if !state.IsInstalled("custom-editor") {
		t.Fatal("custom plugin is not installed")
	}
	duplicate := request(t, server.Handler(), http.MethodPost, "/api/plugins/install", `{"manifest":{"schemaVersion":1,"id":"custom-editor","role":"adapter","root":"~/.custom-editor","categories":{"files":{"paths":["settings.json"],"mode":"mirror"}}}}`)
	if duplicate.Code != http.StatusConflict || errorCode(t, duplicate) != "plugin-installed" {
		t.Fatalf("duplicate custom install = %d %s, want 409 plugin-installed", duplicate.Code, duplicate.Body)
	}
	head, ok := gens.New(home).Read()
	if !ok || head.Generation != 1 {
		t.Fatalf("custom adapter did not publish generation: %+v, ok=%v", head, ok)
	}
	published, problems := core.ValidateConfig(head.Meta)
	if published == nil || published.Adapters["custom-editor"].Categories["packages"].IDPattern != "^[a-z][a-z0-9-]*$" {
		t.Fatalf("custom manifest selector was not published: config=%+v problems=%v", published, problems)
	}

	restarted := pluginruntime.New(home)
	if !restarted.IsInstalled("custom-editor") {
		t.Fatal("custom plugin did not survive runtime restart")
	}
	if got := restarted.List()[0].Adapter.Categories["packages"].IDPattern; got != "^[a-z][a-z0-9-]*$" {
		t.Fatalf("custom manifest selector after restart = %q", got)
	}
	restartedServer := newPluginTestServer(t, home, restarted, nil)
	listed := request(t, restartedServer.Handler(), http.MethodGet, "/api/plugins")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"id":"custom-editor"`) || !strings.Contains(listed.Body.String(), `"root":"~/.custom-editor"`) {
		t.Fatalf("GET after restart = %d %s", listed.Code, listed.Body)
	}
	var saved struct {
		SchemaVersion int                     `json:"schemaVersion"`
		Installed     []string                `json:"installed"`
		Custom        []pluginregistry.Plugin `json:"custom"`
	}
	data, err := os.ReadFile(filepath.Join(home, "plugins.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.SchemaVersion != 1 || len(saved.Installed) != 1 || saved.Installed[0] != "custom-editor" || len(saved.Custom) != 1 || saved.Custom[0].ID != "custom-editor" {
		t.Fatalf("persisted third-party state = %+v", saved)
	}
}

func TestThirdPartyManifestValidationErrors(t *testing.T) {
	cases := []struct {
		name, manifest, code string
		status               int
	}{
		{
			name: "unsupported schema version", manifest: `{"schemaVersion":2,"id":"custom-editor","role":"adapter","root":"~/.editor","categories":{"files":{"paths":["settings.json"],"mode":"mirror"}}}`,
			code: "schema-version", status: http.StatusUnprocessableEntity,
		},
		{
			name: "unsupported role", manifest: `{"schemaVersion":1,"id":"custom-editor","role":"carrier","root":"~/.editor","categories":{"files":{"paths":["settings.json"],"mode":"mirror"}}}`,
			code: "third-party-role", status: http.StatusUnprocessableEntity,
		},
		{
			name: "official id collision", manifest: `{"schemaVersion":1,"id":"pi","role":"adapter","root":"~/.editor","categories":{"files":{"paths":["settings.json"],"mode":"mirror"}}}`,
			code: "plugin-conflict", status: http.StatusConflict,
		},
		{
			name: "invalid category", manifest: `{"schemaVersion":1,"id":"custom-editor","role":"adapter","root":"~/.editor","categories":{"files":{"paths":["settings.json"],"mode":"replace"}}}`,
			code: "manifest-invalid", status: http.StatusUnprocessableEntity,
		},
		{
			name: "invalid id", manifest: `{"schemaVersion":1,"id":"Custom_Editor","role":"adapter","root":"~/.editor","categories":{"files":{"paths":["settings.json"],"mode":"mirror"}}}`,
			code: "manifest-invalid", status: http.StatusUnprocessableEntity,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			home := pluginTestHome(t)
			server := newPluginTestServer(t, home, pluginruntime.New(home), nil)
			response := request(t, server.Handler(), http.MethodPost, "/api/plugins/install", `{"manifest":`+testCase.manifest+`}`)
			if response.Code != testCase.status || errorCode(t, response) != testCase.code {
				t.Fatalf("install invalid manifest = %d %s, want %d code %q", response.Code, response.Body, testCase.status, testCase.code)
			}
		})
	}
}

func TestPluginActionInstallUninstallAndUnsupportedRoute(t *testing.T) {
	home := pluginTestHome(t)
	state := pluginruntime.New(home)
	server := newPluginTestServer(t, home, state, nil)
	installed := request(t, server.Handler(), http.MethodPost, "/api/plugins/install", `{"id":"ssh-key"}`)
	if installed.Code != http.StatusOK || !state.IsInstalled("ssh-key") {
		t.Fatalf("install action = %d %s", installed.Code, installed.Body)
	}
	listed := request(t, server.Handler(), http.MethodGet, "/api/plugins")
	var list pluginsListResponse
	if listed.Code != http.StatusOK || json.Unmarshal(listed.Body.Bytes(), &list) != nil || len(list.Installed) != 1 || list.Installed[0].ID != "ssh-key" {
		t.Fatalf("GET after action install = %d %s", listed.Code, listed.Body)
	}
	if _, exists := gens.New(home).Read(); exists {
		t.Fatal("installing action plugin unexpectedly published a generation")
	}
	uninstalled := request(t, server.Handler(), http.MethodPost, "/api/plugins/uninstall", `{"id":"ssh-key"}`)
	if uninstalled.Code != http.StatusOK || state.IsInstalled("ssh-key") {
		t.Fatalf("uninstall action = %d %s", uninstalled.Code, uninstalled.Body)
	}
	if _, exists := gens.New(home).Read(); exists {
		t.Fatal("uninstalling action plugin unexpectedly published a generation")
	}
	method := request(t, server.Handler(), http.MethodPut, "/api/plugins")
	if method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/plugins = %d %s, want 405", method.Code, method.Body)
	}
	missing := request(t, server.Handler(), http.MethodGet, "/api/plugins/unknown")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("GET unknown plugin path = %d %s, want 404", missing.Code, missing.Body)
	}
}

func TestPluginUninstallRequiresForceAndProtectsAdapterStore(t *testing.T) {
	home := pluginTestHome(t)
	state := pluginruntime.New(home)
	if err := state.Install(pluginByID(t, "pi")); err != nil {
		t.Fatal(err)
	}
	store := map[string]map[string]string{"pi": {"settings/settings.json": "stored"}}
	if _, err := gens.New(home).Publish(store, []byte("{\"version\":1,\"adapters\":{}}\n")); err != nil {
		t.Fatal(err)
	}
	server := newPluginTestServer(t, home, state, nil)
	withoutForce := request(t, server.Handler(), http.MethodPost, "/api/plugins/uninstall", `{"id":"pi"}`)
	if withoutForce.Code != http.StatusConflict || errorCode(t, withoutForce) != "uninstall-guard" || !state.IsInstalled("pi") {
		t.Fatalf("uninstall without force = %d %s", withoutForce.Code, withoutForce.Body)
	}
	withForce := request(t, server.Handler(), http.MethodPost, "/api/plugins/uninstall", `{"id":"pi","force":true}`)
	if withForce.Code != http.StatusConflict || errorCode(t, withForce) != "uninstall-guard" || !state.IsInstalled("pi") {
		t.Fatalf("uninstall with center data = %d %s", withForce.Code, withForce.Body)
	}
	if !strings.Contains(withForce.Body.String(), "清理中心数据") {
		t.Fatalf("adapter store guard did not explain data cleanup: %s", withForce.Body)
	}
}

func TestKeyringUninstallIsGuardedByBindingsToInstalledAdapters(t *testing.T) {
	home := pluginTestHome(t)
	state := pluginruntime.New(home)
	for _, id := range []string{"pi", "keyring"} {
		if err := state.Install(pluginByID(t, id)); err != nil {
			t.Fatal(err)
		}
	}
	password := "long-password"
	destination := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(destination, []byte("secret token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := keyring.Apply(home, keyring.Command{Action: "create", ID: "bound-key", Name: "Bound key", Password: password, WorkFactor: 14})
	if !created.OK {
		t.Fatalf("create test key: %+v", created)
	}
	bound := keyring.Apply(home, keyring.Command{Action: "encrypt", ID: "bound-key", Path: destination, Adapter: "pi", Password: password, WorkFactor: 14})
	if !bound.OK {
		t.Fatalf("bind test key to pi: %+v", bound)
	}
	server := newPluginTestServer(t, home, state, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/plugins/uninstall", `{"id":"keyring","force":true}`)
	if response.Code != http.StatusConflict || errorCode(t, response) != "uninstall-guard" || !state.IsInstalled("keyring") {
		t.Fatalf("uninstall keyring with bound key = %d %s", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), "pi") || !strings.Contains(response.Body.String(), "解绑") {
		t.Fatalf("keyring guard should name the bound adapter and explain unbinding: %s", response.Body)
	}
}

func TestKeyWritesRequireKeyringPluginAndRestoreHubAdapter(t *testing.T) {
	home := pluginTestHome(t)
	paths := pluginTestPaths(t, home)
	if err := core.SaveConfig(paths, core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{}}); err != nil {
		t.Fatal(err)
	}
	state := pluginruntime.New(home)
	server := newPluginTestServer(t, home, state, nil)
	body := `{"id":"missing-key","path":"/not-used","password":"long-password"}`
	refused := request(t, server.Handler(), http.MethodPost, "/api/keys/encrypt", body)
	if refused.Code != http.StatusServiceUnavailable || errorCode(t, refused) != "plugin-required" || !strings.Contains(refused.Body.String(), "密钥环插件未安装，请先在插件页安装") {
		t.Fatalf("key write without keyring plugin = %d %s", refused.Code, refused.Body)
	}
	config, err := core.LoadConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := config.Adapters["keyring"]; exists {
		t.Fatal("a refused write unexpectedly injected the keyring adapter")
	}

	if err := state.Install(pluginByID(t, "keyring")); err != nil {
		t.Fatal(err)
	}
	attempt := request(t, server.Handler(), http.MethodPost, "/api/keys/encrypt", body)
	if attempt.Code == http.StatusServiceUnavailable {
		t.Fatalf("installed keyring write was still refused: %d %s", attempt.Code, attempt.Body)
	}
	config, err = core.LoadConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := config.Adapters["keyring"]; !exists {
		t.Fatal("keyring adapter was not restored to hub homer.json before key operation")
	}
}

func TestRemoteKeyWritesRequireKeyringPlugin(t *testing.T) {
	home := pluginTestHome(t)
	state := pluginruntime.New(home)
	actor := &recordingKeySource{raw: []byte(`{"ok":true,"status":"encrypted"}`)}
	server := newPluginTestServer(t, home, state, actor)
	response := request(t, server.Handler(), http.MethodPost, "/api/agents/box/keys", `{"action":"encrypt","id":"secret"}`)
	if response.Code != http.StatusServiceUnavailable || errorCode(t, response) != "plugin-required" || actor.keyCalls != 0 {
		t.Fatalf("remote key write without plugin = %d calls=%d %s", response.Code, actor.keyCalls, response.Body)
	}
}

func TestNilPluginStateKeepsLegacyKeyWritesAvailable(t *testing.T) {
	home := pluginTestHome(t)
	paths := pluginTestPaths(t, home)
	if err := core.SaveConfig(paths, core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{}}); err != nil {
		t.Fatal(err)
	}
	server := newPluginTestServer(t, home, nil, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/keys/encrypt", `{"id":"missing","path":"/not-used","password":"long-password"}`)
	if response.Code == http.StatusServiceUnavailable || errorCode(t, response) == "plugin-required" {
		t.Fatalf("nil plugin state broke legacy key API: %d %s", response.Code, response.Body)
	}
}

func pluginTestHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	return filepath.Join(root, ".homer")
}

func pluginTestPaths(t *testing.T, home string) core.HomerPaths {
	t.Helper()
	return core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return home
		}
		return os.Getenv(name)
	})
}

func newPluginTestServer(t *testing.T, home string, state *pluginruntime.State, agents AgentsSource) *Server {
	t.Helper()
	server, err := NewServer(ServeOptions{
		Addr:      "127.0.0.1:0",
		HomerHome: home,
		Token:     "test-token",
		Agents:    agents,
		Plugins:   state,
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func pluginByID(t *testing.T, id string) pluginregistry.Plugin {
	t.Helper()
	plugin, ok := pluginregistry.Builtin(id)
	if !ok {
		t.Fatalf("pluginregistry.Builtin(%q) not found", id)
	}
	return plugin
}

func errorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response %q: %v", response.Body.String(), err)
	}
	return body.Error.Code
}

type pluginReportedAdapterSource struct {
	*sourceStub
	reported map[string][]string
}

func (s *pluginReportedAdapterSource) AgentReportedAdapters(agentID string) []string {
	return append([]string(nil), s.reported[agentID]...)
}
