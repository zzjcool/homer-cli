package web

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/gens"
)

func choiceReport(items ...commands.StatusAdapterReport) []commands.StatusAdapterReport {
	return items
}

func TestBuildCollectChoices(t *testing.T) {
	got := BuildCollectChoices(choiceReport(
		commands.StatusAdapterReport{ID: "vscode", Push: 2, Pull: 1},
		commands.StatusAdapterReport{ID: "pi", Push: 0, Pull: 0},
		commands.StatusAdapterReport{ID: "pad", Push: 1, Conflicts: 3},
		commands.StatusAdapterReport{ID: "vscode", Push: 9},
		commands.StatusAdapterReport{ID: "Not An ID"},
	))
	want := []AdapterChoice{
		{ID: "pad", Push: 1, Conflicts: 3, Enabled: false, Detail: "↑1 未收取 · 3 项冲突", Reason: reasonConflict, OnMachine: true},
		{ID: "pi", Enabled: true, OnMachine: true},
		{ID: "vscode", Push: 2, Pull: 1, Checked: true, Enabled: true, Detail: "↑2 未收取 · ↓1 待下发", OnMachine: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("choices = %#v\nwant %#v", got, want)
	}
	if len(BuildCollectChoices(nil)) != 0 {
		t.Fatal("empty machine must produce no choices")
	}
}

func TestCollectChoicesCarryPluginOutline(t *testing.T) {
	got := BuildCollectChoices(choiceReport(commands.StatusAdapterReport{
		ID: "pi",
		Categories: []commands.StatusCategoryReport{{
			Name: "packages",
			Kind: "manifest",
			Files: []commands.StatusFileReport{
				{Path: "npm:pi-lens", Status: "installed"},
				{Path: "packages.manifest.txt", Status: "push"},
			},
		}},
	}))
	if len(got) != 1 || len(got[0].Categories) != 1 {
		t.Fatalf("choices = %#v", got)
	}
	cat := got[0].Categories[0]
	if cat.Label != "插件" || cat.Kind != "manifest" {
		t.Fatalf("category = %#v", cat)
	}
	if len(cat.Files) != 1 || cat.Files[0].Path != "npm:pi-lens" {
		t.Fatalf("files = %#v", cat.Files)
	}
}

func TestStampChoiceDriftMarksPendingFiles(t *testing.T) {
	choices := []AdapterChoice{{
		ID:   "pi",
		Pull: 3,
		Categories: []OutlineCategory{
			{Name: "agents", Files: []OutlineFile{
				{Path: "agents/keep.md"},
				{Path: "agents/sub/send.md"},
				{Path: "agents/other.md"},
			}},
			{Name: "settings", Files: []OutlineFile{
				{Path: "settings/settings.json"},
			}},
			{Name: "packages", Kind: "manifest", Label: "插件", Files: []OutlineFile{
				{Path: "npm:pi-lens"},
				{Path: "npm:other"},
			}},
		},
	}}
	stampChoiceDrift(choices, []commands.StatusAdapterReport{{
		ID:   "pi",
		Pull: 3,
		Categories: []commands.StatusCategoryReport{
			{Name: "agents", Pull: 1, Files: []commands.StatusFileReport{
				{Path: "keep.md", Status: "noop"},
				{Path: "sub/send.md", Status: "pull"},
				{Path: "other.md", Status: "noop"},
				{Path: "gone.md", Status: "pull-delete"},
			}},
			{Name: "settings", Pull: 2, Files: []commands.StatusFileReport{
				{Path: "settings.json:theme", Status: "pull"},
				{Path: "settings.json:font", Status: "pull"},
			}},
			{Name: "packages", Kind: "manifest", Pull: 0, Files: []commands.StatusFileReport{
				{Path: "npm:pi-lens", Status: "installed"},
				{Path: "npm:other", Status: "pull"},
			}},
		},
	}})
	byName := map[string]OutlineCategory{}
	for _, cat := range choices[0].Categories {
		byName[cat.Name] = cat
	}
	agents := byName["agents"]
	if agents.Pull != 1 {
		t.Fatalf("agents = %#v", agents)
	}
	byPath := map[string]OutlineFile{}
	for _, file := range agents.Files {
		byPath[file.Path] = file
	}
	if byPath["agents/keep.md"].Status != "" || byPath["agents/sub/send.md"].Status != "pull" || byPath["agents/gone.md"].Status != "pull-delete" {
		t.Fatalf("agent files = %#v", agents.Files)
	}
	settings := byName["settings"].Files
	if byName["settings"].Pull != 2 || len(settings) != 1 || settings[0].Status != "pull" || !reflect.DeepEqual(settings[0].Keys, []string{"font", "theme"}) {
		t.Fatalf("settings = %#v", byName["settings"])
	}
	plugins := map[string]string{}
	for _, file := range byName["packages"].Files {
		plugins[file.Path] = file.Status
	}
	if plugins["npm:pi-lens"] != "" || plugins["npm:other"] != "pull" {
		t.Fatalf("plugins = %#v", byName["packages"].Files)
	}
}

func TestBuildDispatchChoices(t *testing.T) {
	machine := choiceReport(
		commands.StatusAdapterReport{ID: "vscode", Pull: 2},
		commands.StatusAdapterReport{ID: "pi", Conflicts: 1, Pull: 4},
		commands.StatusAdapterReport{ID: "only-local", Push: 3},
	)
	got := BuildDispatchChoices([]string{"pi", "vscode", "pad", "vscode", "../x"}, machine)
	if len(got) != 3 {
		t.Fatalf("choices = %#v", got)
	}
	byID := map[string]AdapterChoice{}
	for _, choice := range got {
		byID[choice.ID] = choice
	}
	if !byID["vscode"].Checked || !byID["vscode"].Enabled || !byID["vscode"].InCenter {
		t.Fatalf("vscode = %#v", byID["vscode"])
	}
	if byID["pi"].Enabled || byID["pi"].Checked || byID["pi"].Reason != reasonConflict {
		t.Fatalf("pi = %#v", byID["pi"])
	}
	if byID["pad"].Checked || !byID["pad"].Enabled || byID["pad"].OnMachine || byID["pad"].Reason != reasonNotOnMachine {
		t.Fatalf("pad = %#v", byID["pad"])
	}
	if _, ok := byID["only-local"]; ok {
		t.Fatal("machine-only adapters cannot be dispatched")
	}
	if len(BuildDispatchChoices(nil, machine)) != 0 {
		t.Fatal("empty center must produce no dispatch choices")
	}
}

// A key-only drift (a rotated password, a newly encrypted file) leaves the
// bound adapter with no pull of its own. The console hides the keyring row,
// so the key can only travel with its bound adapter; that adapter must then
// be pre-checked or the key silently strands in the center with no visible
// way to send it.
func TestBuildDispatchChoicesPreChecksBoundAdaptersOnKeyDrift(t *testing.T) {
	machine := choiceReport(
		commands.StatusAdapterReport{ID: "pi", Pull: 0},
		commands.StatusAdapterReport{ID: "herdr", Pull: 0},
		commands.StatusAdapterReport{ID: "keyring", Pull: 4},
	)
	got := BuildDispatchChoicesWithKeys([]string{"pi", "herdr", "keyring"}, machine, nil, map[string]bool{"pi": true})
	byID := map[string]AdapterChoice{}
	for _, choice := range got {
		byID[choice.ID] = choice
	}
	if !byID["pi"].Checked || !byID["pi"].Enabled {
		t.Fatalf("pi must be pre-checked so its key can travel: %#v", byID["pi"])
	}
	if byID["herdr"].Checked {
		t.Fatalf("herdr has no key bound and no drift: %#v", byID["herdr"])
	}
	// No pending key drift: nothing changes even when an adapter has a key.
	fresh := choiceReport(
		commands.StatusAdapterReport{ID: "pi", Pull: 0},
		commands.StatusAdapterReport{ID: "keyring", Pull: 0},
	)
	got = BuildDispatchChoicesWithKeys([]string{"pi", "keyring"}, fresh, nil, map[string]bool{"pi": true})
	for _, choice := range got {
		if choice.ID == "pi" && choice.Checked {
			t.Fatalf("pi must stay unchecked without key drift: %#v", choice)
		}
	}
	// A conflicted adapter stays uncheckable; the recorded decision path
	// remains the only route for it.
	conflicted := choiceReport(
		commands.StatusAdapterReport{ID: "pi", Conflicts: 2},
		commands.StatusAdapterReport{ID: "keyring", Pull: 4},
	)
	got = BuildDispatchChoicesWithKeys([]string{"pi", "keyring"}, conflicted, nil, map[string]bool{"pi": true})
	for _, choice := range got {
		if choice.ID == "pi" && choice.Checked {
			t.Fatalf("conflicted pi must not be pre-checked: %#v", choice)
		}
	}
}

func TestBuildResolveChoicesOnlyConflicts(t *testing.T) {
	got := BuildResolveChoices(choiceReport(
		commands.StatusAdapterReport{ID: "pad", Conflicts: 2, Push: 1},
		commands.StatusAdapterReport{ID: "vscode", Push: 4},
	))
	if len(got) != 1 || got[0].ID != "pad" || !got[0].Checked || !got[0].Enabled || got[0].Reason != "" {
		t.Fatalf("resolve choices = %#v", got)
	}
}

func TestSyncChoicesEndpoint(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	if _, err := gens.New(home).Publish(map[string]map[string]string{
		"vscode": {"settings/settings.json": "code"},
		"pi":     {"settings/settings.json": "pi"},
	}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	source := &sourceStub{statusRaw: []byte(`{"adapters":[{"id":"vscode","pull":2},{"id":"pi","conflicts":1,"pull":1}],"errors":[]}`)}
	server := newWebServer(t, fixture, "test-token", source, nil)

	collect := request(t, server.Handler(), http.MethodGet, "/api/sync/choices?direction=collect&agent=box")
	if collect.Code != http.StatusOK || !strings.Contains(collect.Body.String(), `"id":"vscode"`) {
		t.Fatalf("collect choices = %d %s", collect.Code, collect.Body)
	}
	if !strings.Contains(collect.Body.String(), "没勾选的适配器保持中心现有内容") {
		t.Fatalf("hint missing: %s", collect.Body)
	}

	dispatch := request(t, server.Handler(), http.MethodGet, "/api/sync/choices?direction=dispatch&agent=box")
	if dispatch.Code != http.StatusOK {
		t.Fatalf("dispatch choices = %d %s", dispatch.Code, dispatch.Body)
	}
	if !strings.Contains(dispatch.Body.String(), `"id":"vscode"`) || strings.Contains(dispatch.Body.String(), "only-local") {
		t.Fatalf("dispatch body = %s", dispatch.Body)
	}
	if !strings.Contains(dispatch.Body.String(), "密钥会自动跟着它绑定的适配器一起下发") {
		t.Fatalf("dispatch hint = %s", dispatch.Body)
	}

	fresh := &sourceStub{statusRaw: []byte(`{"adapters":[],"errors":["未找到 homer 配置: /tmp/homer.json"]}`)}
	freshServer := newWebServer(t, fixture, "test-token", fresh, nil)
	empty := request(t, freshServer.Handler(), http.MethodGet, "/api/sync/choices?direction=collect&agent=box")
	if !strings.Contains(empty.Body.String(), "还没有配置，无法收取") {
		t.Fatalf("fresh hint = %s", empty.Body)
	}

	marked := &sourceStub{statusRaw: []byte(`{"adapters":[{"id":"pi","pull":1,"categories":[{"name":"agents","pull":1,"files":[{"path":"keep.md","status":"noop"},{"path":"send.md","status":"pull"}]}]}],"errors":[]}`)}
	if _, err := gens.New(home).Publish(map[string]map[string]string{
		"pi": {"agents/keep.md": "a", "agents/send.md": "b"},
	}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	markedServer := newWebServer(t, fixture, "test-token", marked, nil)
	pending := request(t, markedServer.Handler(), http.MethodGet, "/api/sync/choices?direction=dispatch&agent=box")
	if pending.Code != http.StatusOK {
		t.Fatalf("marked dispatch = %d %s", pending.Code, pending.Body)
	}
	var payload struct {
		Adapters []AdapterChoice `json:"adapters"`
	}
	if err := json.Unmarshal(pending.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	var agents *OutlineCategory
	for i := range payload.Adapters {
		if payload.Adapters[i].ID != "pi" {
			continue
		}
		for j := range payload.Adapters[i].Categories {
			if payload.Adapters[i].Categories[j].Name == "agents" {
				agents = &payload.Adapters[i].Categories[j]
			}
		}
	}
	if agents == nil || agents.Pull != 1 {
		t.Fatalf("agents missing from %s", pending.Body)
	}
	got := map[string]string{}
	for _, file := range agents.Files {
		got[file.Path] = file.Status
	}
	if got["agents/send.md"] != "pull" || got["agents/keep.md"] != "" {
		t.Fatalf("files = %#v", agents.Files)
	}

	bad := request(t, server.Handler(), http.MethodGet, "/api/sync/choices?direction=sideways&agent=box")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad direction = %d", bad.Code)
	}
	missingAgent := request(t, server.Handler(), http.MethodGet, "/api/sync/choices?direction=collect")
	if missingAgent.Code != http.StatusBadRequest {
		t.Fatalf("missing agent = %d", missingAgent.Code)
	}
}

func TestSnapshotScopedMergeKeepsOtherAdapters(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	handler := server.Handler()
	first := request(t, handler, http.MethodPost, "/api/snapshot",
		`{"homerJson":"{}","store":{"pi":{"settings/settings.json":"pi-old"},"vscode":{"settings/settings.json":"code-old"}}}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first upload = %d %s", first.Code, first.Body)
	}
	second := request(t, handler, http.MethodPost, "/api/snapshot",
		`{"homerJson":"{}","adapters":["vscode"],"store":{"vscode":{"settings/settings.json":"code-new"},"pad":{"config/config.json":"leak"}}}`)
	if second.Code != http.StatusOK {
		t.Fatalf("scoped upload = %d %s", second.Code, second.Body)
	}
	store := downloadStore(t, handler)
	if store["pi"]["settings/settings.json"] != "pi-old" {
		t.Fatalf("pi was replaced: %#v", store)
	}
	if store["vscode"]["settings/settings.json"] != "code-new" {
		t.Fatalf("vscode = %#v", store["vscode"])
	}
	if _, ok := store["pad"]; ok {
		t.Fatal("adapter outside the scope leaked into the center")
	}

	removed := request(t, handler, http.MethodPost, "/api/snapshot",
		`{"homerJson":"{}","adapters":["pi"],"store":{}}`)
	if removed.Code != http.StatusOK {
		t.Fatalf("remove upload = %d %s", removed.Code, removed.Body)
	}
	store = downloadStore(t, handler)
	if _, ok := store["pi"]; ok {
		t.Fatalf("pi should have been removed: %#v", store)
	}
	if store["vscode"]["settings/settings.json"] != "code-new" {
		t.Fatalf("vscode changed while removing pi: %#v", store)
	}

	before := store
	rejected := request(t, handler, http.MethodPost, "/api/snapshot",
		`{"homerJson":"{}","adapters":["../x"],"store":{"vscode":{"settings/settings.json":"nope"}}}`)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("invalid scope = %d %s", rejected.Code, rejected.Body)
	}
	empty := request(t, handler, http.MethodPost, "/api/snapshot", `{"homerJson":"{}","adapters":[]}`)
	if empty.Code != http.StatusBadRequest || !strings.Contains(empty.Body.String(), "请选择至少一个适配器") {
		t.Fatalf("empty scope = %d %s", empty.Code, empty.Body)
	}
	if !reflect.DeepEqual(downloadStore(t, handler), before) {
		t.Fatal("rejected uploads changed the center")
	}

	replaced := request(t, handler, http.MethodPost, "/api/snapshot",
		`{"homerJson":"{}","store":{"pad":{"config/config.json":"only-pad"}}}`)
	if replaced.Code != http.StatusOK {
		t.Fatalf("full upload = %d %s", replaced.Code, replaced.Body)
	}
	store = downloadStore(t, handler)
	if _, ok := store["vscode"]; ok {
		t.Fatalf("unscoped upload must replace the generation: %#v", store)
	}
	if store["pad"]["config/config.json"] != "only-pad" {
		t.Fatalf("pad = %#v", store["pad"])
	}
}

func TestCollectAndDispatchForwardSelection(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	if _, err := gens.New(home).Publish(map[string]map[string]string{
		"vscode": {"settings/settings.json": "code"},
		"pi":     {"settings/settings.json": "pi"},
	}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	source := &sourceStub{
		list:    []AgentInfo{{AgentID: "box", Hostname: "box"}},
		pushRaw: []byte(`{"ok":true,"status":"pushed"}`),
		pullRaw: []byte(`{"ok":true,"status":"applied"}`),
	}
	source.onPush = func() {
		if _, err := gens.New(home).Publish(map[string]map[string]string{
			"pi":     {"settings/settings.json": "pi"},
			"vscode": {"settings/settings.json": "from-machine"},
		}, []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	server := newWebServer(t, fixture, "test-token", source, nil)
	handler := server.Handler()

	collect := request(t, handler, http.MethodPost, "/api/sync?direction=collect&agent=box&confirm=true", `{"adapters":["vscode","pad"]}`)
	if collect.Code != http.StatusOK {
		t.Fatalf("collect = %d %s", collect.Code, collect.Body)
	}
	if len(source.pushScopes) != 1 || !source.pushScopes[0].Explicit || !reflect.DeepEqual(source.pushScopes[0].Adapters, []string{"vscode", "pad"}) || source.pushScopes[0].Overwrite {
		t.Fatalf("push scope = %#v", source.pushScopes)
	}

	dispatch := request(t, handler, http.MethodPost, "/api/agents/box/pull?confirm=true", `{"adapters":["vscode"]}`)
	if dispatch.Code != http.StatusOK {
		t.Fatalf("dispatch = %d %s", dispatch.Code, dispatch.Body)
	}
	if len(source.pullScopes) != 1 || !reflect.DeepEqual(source.pullScopes[0].Adapters, []string{"vscode"}) || source.pullScopes[0].Overwrite {
		t.Fatalf("pull scope = %#v", source.pullScopes)
	}

	before := len(source.pullScopes)
	missing := request(t, handler, http.MethodPost, "/api/sync?direction=dispatch&confirm=true", `{"adapters":["nope"]}`)
	if missing.Code != http.StatusBadRequest || !strings.Contains(missing.Body.String(), "中心没有适配器 nope") {
		t.Fatalf("missing dispatch = %d %s", missing.Code, missing.Body)
	}
	if len(source.pullScopes) != before {
		t.Fatal("missing adapter must not pull any machine")
	}

	invalid := request(t, handler, http.MethodPost, "/api/sync?direction=collect&agent=box&confirm=true", `{"adapters":["../x"]}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid collect = %d %s", invalid.Code, invalid.Body)
	}
	empty := request(t, handler, http.MethodPost, "/api/agents/box/pull?confirm=true", `{"adapters":[]}`)
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty pull = %d %s", empty.Code, empty.Body)
	}
}

func TestResolveForwardsExplicitAdapters(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	if _, err := gens.New(home).Publish(map[string]map[string]string{
		"pad": {"settings/settings.json": "center"},
	}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	source := &sourceStub{
		list:    []AgentInfo{{AgentID: "box", Hostname: "box"}, {AgentID: "other", Hostname: "other"}},
		pushRaw: []byte(`{"ok":true,"status":"pushed"}`),
		pullRaw: []byte(`{"ok":true,"status":"applied"}`),
	}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/resolve?choice=local&agent=box&confirm=true", `{"adapters":["pad"]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", response.Code, response.Body)
	}
	if len(source.pushScopes) != 1 || !source.pushScopes[0].Explicit || !source.pushScopes[0].Overwrite {
		t.Fatalf("push scope = %#v", source.pushScopes)
	}
	if !reflect.DeepEqual(source.pushScopes[0].Adapters, []string{"pad"}) {
		t.Fatalf("adapters = %#v", source.pushScopes[0].Adapters)
	}
	if len(source.pullScopes) != 2 {
		t.Fatalf("fanout pulls = agents %v scopes %#v", source.pulledAgent, source.pullScopes)
	}
	sawOther := false
	for index, scope := range source.pullScopes {
		if !scope.Explicit || !scope.Overwrite || !reflect.DeepEqual(scope.Adapters, []string{"pad"}) {
			t.Fatalf("pull scope = %#v", scope)
		}
		if source.pulledAgent[index] == "other" {
			sawOther = true
		}
	}
	if !sawOther {
		t.Fatal("the other online machine was not updated")
	}
}

func downloadStore(t *testing.T, handler http.Handler) map[string]map[string]string {
	t.Helper()
	response := request(t, handler, http.MethodGet, "/api/snapshot", "")
	if response.Code != http.StatusOK {
		t.Fatalf("download = %d %s", response.Code, response.Body)
	}
	var snapshot struct {
		Store map[string]map[string]string `json:"store"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot.Store
}
