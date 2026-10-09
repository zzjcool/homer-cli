package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
)

func TestScopedCollectPublishesOnlyTheSelection(t *testing.T) {
	paths := writeScopeHome(t)
	pi := scopeAdapterSnap("pi", "pi-keep")
	if err := core.WriteSnapshotToStore(paths, pi); err != nil {
		t.Fatal(err)
	}
	var uploaded []core.AdapterSnapshot
	report := RunPush(PushOptions{
		HomerHome: paths.Home,
		Yes:       true,
		Adapters:  []string{"pad"},
	}, &PushDeps{
		UI: HeadlessUI{},
		Sources: syncx.SyncSources{
			Base:  []core.AdapterSnapshot{scopeAdapterSnap("pad", "old"), pi},
			Local: []core.AdapterSnapshot{scopeAdapterSnap("pad", "from-pad"), pi},
		},
		HubSnapshot: []core.AdapterSnapshot{scopeAdapterSnap("pi", "center-pi"), scopeAdapterSnap("vscode", "center-code")},
		HubSink: func(snapshot []core.AdapterSnapshot) (int, error) {
			uploaded = snapshot
			return 4, nil
		},
	})
	if !report.OK || report.Status != PushStatusPushed {
		t.Fatalf("report = %#v errors=%v", report, report.Errors)
	}
	if len(uploaded) != 1 || uploaded[0].AdapterID != "pad" {
		t.Fatalf("uploaded = %#v", uploaded)
	}
	if uploaded[0].Categories[0].Files["settings.json"].Content != "from-pad" {
		t.Fatalf("pad content = %#v", uploaded[0])
	}
	kept, err := os.ReadFile(filepath.Join(paths.StoreDir, "pi", "settings", "settings.json"))
	if err != nil || string(kept) != "pi-keep" {
		t.Fatalf("pi store = %q err=%v", kept, err)
	}
}

func TestScopedCollectOverwritesCenterAheadContent(t *testing.T) {
	paths := writeScopeHome(t)
	var uploaded []core.AdapterSnapshot
	report := RunPush(PushOptions{HomerHome: paths.Home, Yes: true, Adapters: []string{"pad"}}, &PushDeps{
		UI:          HeadlessUI{},
		Sources:     syncx.SyncSources{Base: []core.AdapterSnapshot{scopeAdapterSnap("pad", "machine")}, Local: []core.AdapterSnapshot{scopeAdapterSnap("pad", "machine")}},
		HubSnapshot: []core.AdapterSnapshot{scopeAdapterSnap("pad", "center-newer")},
		HubSink: func(snapshot []core.AdapterSnapshot) (int, error) {
			uploaded = snapshot
			return 2, nil
		},
	})
	if !report.OK || report.Status != PushStatusPushed {
		t.Fatalf("report = %#v errors=%v warnings=%v", report, report.Errors, report.Warnings)
	}
	if len(uploaded) != 1 || uploaded[0].Categories[0].Files["settings.json"].Content != "machine" {
		t.Fatalf("uploaded = %#v", uploaded)
	}
}

func TestScopedCollectSkipsIdenticalAndRefusesConflicts(t *testing.T) {
	paths := writeScopeHome(t)
	calls := 0
	deps := &PushDeps{
		UI: HeadlessUI{},
		Sources: syncx.SyncSources{
			Base:  []core.AdapterSnapshot{scopeAdapterSnap("pad", "same")},
			Local: []core.AdapterSnapshot{scopeAdapterSnap("pad", "same")},
		},
		HubSnapshot: []core.AdapterSnapshot{scopeAdapterSnap("pad", "same")},
		HubSink: func([]core.AdapterSnapshot) (int, error) {
			calls++
			return 1, nil
		},
	}
	report := RunPush(PushOptions{HomerHome: paths.Home, Yes: true, Adapters: []string{"pad"}}, deps)
	if report.Status != PushStatusNoDrift || calls != 0 {
		t.Fatalf("identical collect status=%s calls=%d errors=%v", report.Status, calls, report.Errors)
	}

	deps.Sources = syncx.SyncSources{
		Base:  []core.AdapterSnapshot{scopeAdapterSnap("pad", "base")},
		Local: []core.AdapterSnapshot{scopeAdapterSnap("pad", "local")},
	}
	deps.HubSnapshot = []core.AdapterSnapshot{scopeAdapterSnap("pad", "center")}
	report = RunPush(PushOptions{HomerHome: paths.Home, Yes: true, Adapters: []string{"pad"}}, deps)
	if report.Status != PushStatusConflicts || calls != 0 {
		t.Fatalf("conflict collect status=%s calls=%d errors=%v", report.Status, calls, report.Errors)
	}

	report = RunPush(PushOptions{HomerHome: paths.Home, Yes: true, Adapters: []string{"pad"}, Overwrite: true}, deps)
	if !report.OK || report.Status != PushStatusPushed || calls != 1 {
		t.Fatalf("overwrite collect status=%s calls=%d errors=%v", report.Status, calls, report.Errors)
	}
}

func TestScopedCollectIgnoresSecretsOutsideTheSelection(t *testing.T) {
	paths := writeScopeHome(t)
	secret := "sk-ant-abcdefghijklmnopqrst"
	var uploaded []core.AdapterSnapshot
	deps := &PushDeps{
		UI: HeadlessUI{},
		Sources: syncx.SyncSources{
			Base: []core.AdapterSnapshot{scopeAdapterSnap("pad", "old"), scopeAdapterSnap("pi", "old")},
			Local: []core.AdapterSnapshot{
				scopeAdapterSnap("pad", "clean"),
				scopeAdapterSnap("pi", secret),
			},
		},
		HubSnapshot: []core.AdapterSnapshot{},
		HubSink: func(snapshot []core.AdapterSnapshot) (int, error) {
			uploaded = snapshot
			return 1, nil
		},
	}
	report := RunPush(PushOptions{HomerHome: paths.Home, Yes: true, Adapters: []string{"pad"}}, deps)
	if !report.OK || len(uploaded) != 1 || uploaded[0].AdapterID != "pad" {
		t.Fatalf("pad collect = status %s uploaded %#v errors %v", report.Status, uploaded, report.Errors)
	}
	if strings.Contains(uploaded[0].Categories[0].Files["settings.json"].Content, "sk-ant-") {
		t.Fatal("selected upload contains a secret")
	}

	uploaded = nil
	report = RunPush(PushOptions{HomerHome: paths.Home, Yes: true, Adapters: []string{"pi"}}, deps)
	if report.Status != PushStatusSecretsRejected || uploaded != nil {
		t.Fatalf("secret collect status=%s uploaded=%v errors=%v", report.Status, uploaded, report.Errors)
	}
	if len(report.Errors) < 2 || !strings.Contains(report.Errors[1], "pi/") {
		t.Fatalf("secret warning should name the file, errors=%v", report.Errors)
	}

	uploaded = nil
	report = RunPush(PushOptions{HomerHome: paths.Home, Yes: true, Adapters: []string{"pi"}, AllowSecrets: true}, deps)
	if !report.OK || report.Status != PushStatusPushed || len(uploaded) != 1 || uploaded[0].AdapterID != "pi" {
		t.Fatalf("confirmed secret collect status=%s uploaded=%v errors=%v", report.Status, uploaded, report.Errors)
	}
}

func TestScopedCollectRejectsBadSelections(t *testing.T) {
	paths := writeScopeHome(t)
	calls := 0
	deps := &PushDeps{
		UI: HeadlessUI{},
		Sources: syncx.SyncSources{
			Base:  []core.AdapterSnapshot{scopeAdapterSnap("pad", "old")},
			Local: []core.AdapterSnapshot{scopeAdapterSnap("pad", "new")},
		},
		HubSnapshot: []core.AdapterSnapshot{},
		HubSink: func([]core.AdapterSnapshot) (int, error) {
			calls++
			return 1, nil
		},
	}
	cases := []struct {
		name     string
		adapters []string
		yes      bool
		hubErr   error
		want     string
	}{
		{name: "unknown", adapters: []string{"nope"}, yes: true, want: "没有适配器 nope，无法收取"},
		{name: "disabled", adapters: []string{"herdr"}, yes: true, want: "适配器 herdr 未启用，无法收取"},
		{name: "empty", adapters: []string{"  "}, yes: true, want: "请选择至少一个适配器"},
		{name: "invalid", adapters: []string{"../pad"}, yes: true, want: "适配器 ID 无效"},
		{name: "unconfirmed", adapters: []string{"pad"}, yes: false, want: "未确认"},
		{name: "center unread", adapters: []string{"pad"}, yes: true, hubErr: os.ErrClosed, want: "file already closed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			calls = 0
			deps.HubErr = test.hubErr
			report := RunPush(PushOptions{HomerHome: paths.Home, Yes: test.yes, Adapters: test.adapters}, deps)
			if report.OK || calls != 0 {
				t.Fatalf("status=%s ok=%v calls=%d errors=%v", report.Status, report.OK, calls, report.Errors)
			}
			if !strings.Contains(strings.Join(report.Errors, "\n"), test.want) {
				t.Fatalf("errors = %v, want %q", report.Errors, test.want)
			}
		})
	}
}

func TestUnscopedPushIgnoresCenterSnapshot(t *testing.T) {
	paths := writeScopeHome(t)
	var uploaded []string
	base := []core.AdapterSnapshot{scopeAdapterSnap("pad", "base"), scopeAdapterSnap("pi", "base")}
	report := RunPush(PushOptions{HomerHome: paths.Home, Yes: true}, &PushDeps{
		UI: HeadlessUI{},
		Sources: syncx.SyncSources{
			Base:   base,
			Local:  []core.AdapterSnapshot{scopeAdapterSnap("pad", "pad-new"), scopeAdapterSnap("pi", "pi-new")},
			Remote: base,
		},
		HubSnapshot: []core.AdapterSnapshot{scopeAdapterSnap("pad", "conflict")},
		HubSink: func(snapshot []core.AdapterSnapshot) (int, error) {
			for _, item := range snapshot {
				uploaded = append(uploaded, item.AdapterID)
			}
			return 1, nil
		},
	})
	if !report.OK || len(uploaded) != 2 {
		t.Fatalf("unscoped upload = %v status=%s errors=%v", uploaded, report.Status, report.Errors)
	}
}

func TestScopedPullLeavesOtherAdaptersUntouched(t *testing.T) {
	paths, roots := writeScopeTools(t)
	sources := syncx.SyncSources{
		Base:  []core.AdapterSnapshot{scopeAdapterSnap("pi", "keep"), scopeAdapterSnap("pad", "old")},
		Local: []core.AdapterSnapshot{scopeAdapterSnap("pi", "keep"), scopeAdapterSnap("pad", "old")},
		Remote: []core.AdapterSnapshot{
			scopeAdapterSnap("pi", "do-not-apply"),
			scopeAdapterSnap("pad", "from-center"),
		},
	}
	report := RunPull(PullOptions{HomerHome: paths.Home, Yes: true, Adapters: []string{"pad"}}, &PullDeps{
		UI:          HeadlessUI{},
		Sources:     sources,
		HubSnapshot: sources.Remote,
	})
	if !report.OK || report.Status != PullStatusApplied {
		t.Fatalf("pull = %#v errors=%v", report, report.Errors)
	}
	if got := mustReadScope(t, filepath.Join(roots["pad"], "settings.json")); got != "from-center" {
		t.Fatalf("pad = %q", got)
	}
	if got := mustReadScope(t, filepath.Join(roots["pi"], "settings.json")); got != "keep" {
		t.Fatalf("pi = %q", got)
	}
}

func TestScopedPullRejectsMissingUnknownAndDisabled(t *testing.T) {
	paths, roots := writeScopeTools(t)
	remote := []core.AdapterSnapshot{
		scopeAdapterSnap("pad", "from-center"),
		scopeAdapterSnap("pi", "keep"),
		scopeAdapterSnap("herdr", "center-herdr"),
	}
	sources := syncx.SyncSources{
		Base:   []core.AdapterSnapshot{scopeAdapterSnap("pad", "old"), scopeAdapterSnap("pi", "keep")},
		Local:  []core.AdapterSnapshot{scopeAdapterSnap("pad", "old"), scopeAdapterSnap("pi", "keep")},
		Remote: remote,
	}
	cases := []struct {
		name     string
		adapters []string
		want     string
	}{
		{name: "missing", adapters: []string{"vscode"}, want: "中心没有适配器 vscode"},
		{name: "unknown", adapters: []string{"nope"}, want: "中心没有适配器 nope"},
		{name: "disabled", adapters: []string{"herdr"}, want: "适配器 herdr 未启用"},
		{name: "empty", adapters: []string{}, want: "请选择至少一个适配器"},
		{name: "invalid", adapters: []string{"Pad"}, want: "适配器 ID 无效"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			report := RunPull(PullOptions{HomerHome: paths.Home, Yes: true, Adapters: test.adapters}, &PullDeps{
				UI: HeadlessUI{}, Sources: sources, HubSnapshot: remote,
			})
			if report.OK || !strings.Contains(strings.Join(report.Errors, "\n"), test.want) {
				t.Fatalf("errors = %v, want %q", report.Errors, test.want)
			}
			if got := mustReadScope(t, filepath.Join(roots["pad"], "settings.json")); got != "old" {
				t.Fatalf("pad changed to %q", got)
			}
		})
	}
}

func TestScopedPullPreferRemoteResolvesOnlyTheSelection(t *testing.T) {
	paths, roots := writeScopeTools(t)
	sources := syncx.SyncSources{
		Base:  []core.AdapterSnapshot{scopeAdapterSnap("pad", "base"), scopeAdapterSnap("pi", "keep")},
		Local: []core.AdapterSnapshot{scopeAdapterSnap("pad", "local"), scopeAdapterSnap("pi", "keep")},
		Remote: []core.AdapterSnapshot{
			scopeAdapterSnap("pad", "center"),
			scopeAdapterSnap("pi", "do-not-apply"),
		},
	}
	if err := os.WriteFile(filepath.Join(roots["pad"], "settings.json"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocked := RunPull(PullOptions{HomerHome: paths.Home, Yes: true, Adapters: []string{"pad"}}, &PullDeps{
		UI: HeadlessUI{}, Sources: sources, HubSnapshot: sources.Remote,
	})
	if blocked.Status != PullStatusConflictsRemain {
		t.Fatalf("without prefer-remote status=%s errors=%v", blocked.Status, blocked.Errors)
	}
	if got := mustReadScope(t, filepath.Join(roots["pad"], "settings.json")); got != "local" {
		t.Fatalf("conflict pull wrote %q", got)
	}

	applied := RunPull(PullOptions{
		HomerHome: paths.Home, Yes: true, Adapters: []string{"pad"}, PreferRemote: true,
	}, &PullDeps{UI: HeadlessUI{}, Sources: sources, HubSnapshot: sources.Remote})
	if !applied.OK || applied.Status != PullStatusApplied {
		t.Fatalf("prefer remote = %#v errors=%v", applied, applied.Errors)
	}
	if got := mustReadScope(t, filepath.Join(roots["pad"], "settings.json")); got != "center" {
		t.Fatalf("pad = %q", got)
	}
	if got := mustReadScope(t, filepath.Join(roots["pi"], "settings.json")); got != "keep" {
		t.Fatalf("pi = %q", got)
	}
}

func TestPreferRemotePlanFor_SubsetOnly(t *testing.T) {
	plan := syncx.PullPlan{Actions: []syncx.PullAction{
		{Type: syncx.PullActionConflict, AdapterID: "pi", Category: "settings", RelPath: "settings.json", LocalContent: "pi-local", RemoteContent: "pi-center"},
		{Type: syncx.PullActionConflict, AdapterID: "pad", Category: "settings", RelPath: "settings.json", LocalContent: "pad-local", RemoteContent: "pad-center"},
		{Type: syncx.PullActionWrite, AdapterID: "pi", Category: "settings", RelPath: "other.json", Content: "already-written"},
	}}

	got := preferRemotePlanFor(plan, nil, []string{"pi"})
	if len(got.Actions) != len(plan.Actions) {
		t.Fatalf("actions = %#v, want %d actions", got.Actions, len(plan.Actions))
	}
	if action := got.Actions[0]; action.Type != syncx.PullActionWrite || action.Content != "pi-center" {
		t.Fatalf("selected conflict = %#v, want center write", action)
	}
	if action := got.Actions[1]; action.Type != syncx.PullActionConflict || action.Content != "" || action.RemoteContent != "pad-center" {
		t.Fatalf("unselected conflict = %#v, want unchanged conflict", action)
	}
	if action := got.Actions[2]; action.Type != syncx.PullActionWrite || action.Content != "already-written" {
		t.Fatalf("non-conflict action = %#v, want unchanged write", action)
	}
	if plan.Actions[0].Type != syncx.PullActionConflict || plan.Actions[0].Content != "" {
		t.Fatalf("input plan was mutated: %#v", plan.Actions[0])
	}
}

func TestPreferRemotePlanFor_NilMeansAll_EmptyMeansNone(t *testing.T) {
	plan := syncx.PullPlan{Actions: []syncx.PullAction{
		{Type: syncx.PullActionConflict, AdapterID: "pi", Category: "settings", RelPath: "settings.json"},
		{Type: syncx.PullActionConflict, AdapterID: "pad", Category: "settings", RelPath: "settings.json"},
	}}
	remote := []core.AdapterSnapshot{scopeAdapterSnap("pi", "center-pi"), scopeAdapterSnap("pad", "center-pad")}

	var nilIDs []string
	all := preferRemotePlanFor(plan, remote, nilIDs)
	for index, want := range []string{"center-pi", "center-pad"} {
		if action := all.Actions[index]; action.Type != syncx.PullActionWrite || action.Content != want {
			t.Fatalf("nil selection action[%d] = %#v, want write %q", index, action, want)
		}
	}

	emptyIDs := []string{}
	none := preferRemotePlanFor(plan, remote, emptyIDs)
	for index, action := range none.Actions {
		if action.Type != syncx.PullActionConflict {
			t.Fatalf("empty selection action[%d] = %#v, want conflict", index, action)
		}
	}
}

func TestPreferRemotePlanFor_RemoteMissingBecomesDelete(t *testing.T) {
	plan := syncx.PullPlan{Actions: []syncx.PullAction{{
		Type: syncx.PullActionConflict, AdapterID: "pi", Category: "settings", RelPath: "settings.json", LocalContent: "local",
	}}}

	got := preferRemotePlanFor(plan, nil, []string{"pi"})
	if len(got.Actions) != 1 || got.Actions[0].Type != syncx.PullActionDelete || got.Actions[0].Content != "" {
		t.Fatalf("plan = %#v, want one delete for the missing center file", got)
	}
}

func TestPreferRemotePlan_CharacterizationUnchanged(t *testing.T) {
	config := core.HomerConfig{Adapters: map[string]core.AdapterConfig{
		"pi": {Categories: map[string]core.CategoryConfig{
			"settings": {Mode: core.SyncModeMerge, ExcludeKeys: []string{"apiKey"}},
		}},
	}}
	base := mergeScopeAdapterSnap("pi", `{"apiKey":"base-value","theme":"base"}`)
	local := mergeScopeAdapterSnap("pi", `{"apiKey":"local-value","theme":"local"}`)
	remote := mergeScopeAdapterSnap("pi", `{"apiKey":"center-value","theme":"center"}`)
	plan := syncx.PlanPull(config, []core.AdapterSnapshot{base}, []core.AdapterSnapshot{local}, []core.AdapterSnapshot{remote})
	if len(plan.Actions) != 1 || plan.Actions[0].Type != syncx.PullActionConflict {
		t.Fatalf("planned actions = %#v, want one merge conflict", plan.Actions)
	}
	if plan.Actions[0].RemoteContent != "" {
		t.Fatalf("merge conflict RemoteContent = %q, want empty so the center snapshot fallback is characterized", plan.Actions[0].RemoteContent)
	}

	// Characterize the existing excludeKeys gap without fixing it: PlanPull
	// strips apiKey for conflict detection, but preferRemotePlan's raw snapshot
	// fallback still writes the center's apiKey as part of the whole file.
	got := preferRemotePlan(plan, []core.AdapterSnapshot{remote})
	want := `{"apiKey":"center-value","theme":"center"}`
	if len(got.Actions) != 1 || got.Actions[0].Type != syncx.PullActionWrite || got.Actions[0].Content != want {
		t.Fatalf("rewritten actions = %#v, want center write %q (including the characterized apiKey behavior)", got.Actions, want)
	}
}

func mergeScopeAdapterSnap(id, content string) core.AdapterSnapshot {
	return core.AdapterSnapshot{AdapterID: id, Categories: []core.CategorySnapshot{{
		AdapterID: id,
		Category:  "settings",
		Mode:      core.SyncModeMerge,
		Files:     core.SnapshotFiles{"settings.json": {Kind: "json", Content: content}},
	}}}
}

func writeScopeHome(t *testing.T) core.HomerPaths {
	t.Helper()
	home := t.TempDir()
	paths := core.GetHomerPaths(func(string) string { return home })
	enabled := true
	disabled := false
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pad": {Root: filepath.Join(home, "pad"), Enabled: &enabled, Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
		}},
		"pi": {Root: filepath.Join(home, "pi"), Enabled: &enabled, Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
		}},
		"herdr": {Root: filepath.Join(home, "herdr"), Enabled: &disabled, Categories: map[string]core.CategoryConfig{
			"config": {Paths: []string{"config.toml"}, Mode: core.SyncModeMirror},
		}},
	}}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	return paths
}

func writeScopeTools(t *testing.T) (core.HomerPaths, map[string]string) {
	t.Helper()
	paths := writeScopeHome(t)
	roots := map[string]string{
		"pad": filepath.Join(paths.Home, "pad"),
		"pi":  filepath.Join(paths.Home, "pi"),
	}
	initial := map[string]string{"pad": "old", "pi": "keep"}
	for id, content := range initial {
		if err := os.MkdirAll(roots[id], 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(roots[id], "settings.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := core.WriteSnapshotToStore(paths, scopeAdapterSnap(id, content)); err != nil {
			t.Fatal(err)
		}
	}
	return paths, roots
}

func scopeAdapterSnap(id, content string) core.AdapterSnapshot {
	return core.AdapterSnapshot{
		AdapterID: id,
		Categories: []core.CategorySnapshot{{
			AdapterID: id,
			Category:  "settings",
			Mode:      core.SyncModeMirror,
			Files:     core.SnapshotFiles{"settings.json": {Kind: "file", Content: content}},
		}},
	}
}

func mustReadScope(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
