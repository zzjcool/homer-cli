package commands

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/adapter/pi"
	"github.com/zzjcool/homer-cli/internal/core"
)

type wizardCall struct {
	message string
	options []WizardOption
	checked []string
}

type scriptedWizard struct {
	calls    []wizardCall
	selectFn func(call int, message string, options []WizardOption, checked []string) ([]string, error)
}

func (wizard *scriptedWizard) MultiSelect(message string, options []WizardOption, checked []string) ([]string, error) {
	wizard.calls = append(wizard.calls, wizardCall{
		message: message,
		options: append([]WizardOption(nil), options...),
		checked: append([]string(nil), checked...),
	})
	if wizard.selectFn == nil {
		return append([]string(nil), checked...), nil
	}
	return wizard.selectFn(len(wizard.calls)-1, message, options, checked)
}

func optionValues(options []WizardOption) []string {
	values := make([]string, 0, len(options))
	for _, option := range options {
		values = append(values, option.Value)
	}
	return values
}

func TestRunSelectionWizardAdapterCategoryEntryOff(t *testing.T) {
	wizard := &scriptedWizard{selectFn: func(call int, _ string, options []WizardOption, _ []string) ([]string, error) {
		switch call {
		case 0:
			return []string{"pi"}, nil
		case 1:
			return []string{"extensions"}, nil
		case 2:
			// The wizard appends a back sentinel after the real entries;
			// selecting all but the sentinel and the last real entry excludes
			// only that last entry (last option = sentinel).
			values := optionValues(options)
			result := make([]string, 0, len(values))
			for index, value := range values {
				if value == WizardBackValue || index == len(values)-2 {
					continue
				}
				result = append(result, value)
			}
			return result, nil
		default:
			return nil, errors.New("unexpected prompt")
		}
	}}
	entries := make([]WizardEntry, 0, WizardEntryDrillThreshold+1)
	for index := 0; index < WizardEntryDrillThreshold+1; index++ {
		key := "entry-" + string(rune('a'+index))
		entries = append(entries, WizardEntry{Key: key, Label: key, Included: true})
	}
	state := WizardState{Adapters: []WizardAdapter{
		{ID: "pi", Enabled: true, Categories: []WizardCategory{
			{Name: "settings", Enabled: true, FileCount: 1},
			{Name: "extensions", Enabled: true, FileCount: len(entries), Entries: entries},
		}},
	}}
	selection, err := RunSelectionWizard(wizard, state)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Adapters["pi"] {
		t.Fatalf("adapter selection = %#v", selection.Adapters)
	}
	if selection.Categories["pi"]["settings"] || !selection.Categories["pi"]["extensions"] {
		t.Fatalf("category selection = %#v", selection.Categories)
	}
	if got := selection.Excluded["pi"]["extensions"]; len(got) != 1 || got[0] != entries[len(entries)-1].Key {
		t.Fatalf("excluded entries = %#v", got)
	}

	adapterOff := &scriptedWizard{selectFn: func(call int, _ string, _ []WizardOption, _ []string) ([]string, error) {
		if call != 0 {
			t.Fatalf("adapter-off prompted at call %d", call)
		}
		return nil, nil
	}}
	off, err := RunSelectionWizard(adapterOff, state)
	if err != nil {
		t.Fatal(err)
	}
	if off.Adapters["pi"] || off.Categories["pi"]["extensions"] {
		t.Fatalf("adapter off selection = %#v", off)
	}
}

func TestRunSelectionWizardBackNavigationRestoresState(t *testing.T) {
	// Exact user-reported scenario: entered pi's category question, then
	// remembered herdr was also wanted. Selecting the back sentinel
	// restarts the adapter question; the resumed pi category question
	// preserves the in-progress selection as defaults.
	var calls []wizardCall
	wizard := &scriptedWizard{selectFn: func(call int, message string, options []WizardOption, checked []string) ([]string, error) {
		_ = call
		calls = append(calls, wizardCall{message: message, options: options, checked: checked})
		switch message {
		case "选择要初始化的 adapter（空格勾选，Enter 确认）":
			if len(calls) == 1 {
				return []string{"pi"}, nil // initially only pi
			}
			return []string{"pi", "herdr"}, nil // after back: herdr added
		case "选择 pi 的分类（空格勾选，Enter 确认；选 < 返回上一级 回到 adapter 选择）":
			if len(calls) == 2 {
				return []string{WizardBackValue}, nil // go back on first visit
			}
			// Second visit must default to the in-progress choices.
			return append([]string(nil), checked...), nil
		default:
			return append([]string(nil), checked...), nil
		}
	}}
	state := WizardState{Adapters: []WizardAdapter{
		{ID: "pi", Enabled: true, Categories: []WizardCategory{
			{Name: "settings", Enabled: true, FileCount: 1},
		}},
		{ID: "herdr", Enabled: true, Categories: []WizardCategory{
			{Name: "config", Enabled: true, FileCount: 1},
		}},
	}}
	selection, err := RunSelectionWizard(wizard, state)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Adapters["pi"] || !selection.Adapters["herdr"] {
		t.Fatalf("adapter selection after back = %#v", selection.Adapters)
	}
	// 1st adapter prompt, pi category (back), 2nd adapter prompt (pi+herdr),
	// pi category resumed, herdr category.
	if len(calls) != 5 {
		t.Fatalf("prompt count = %d, want 5: %+v", len(calls), calls)
	}
	// The resumed pi category question must offer the sentinel and carry the
	// in-progress defaults, never the sentinel itself.
	resumed := calls[3]
	if optionValues(resumed.options)[len(resumed.options)-1] != WizardBackValue {
		t.Fatal("resumed category prompt missing back sentinel")
	}
	for _, value := range resumed.checked {
		if value == WizardBackValue {
			t.Fatal("back sentinel leaked into restored defaults")
		}
	}
	// herdr was asked after the resume.
	if !strings.Contains(calls[4].message, "herdr") {
		t.Fatalf("herdr never asked: %q", calls[4].message)
	}
}

func TestRunSelectionWizardEntryBackReturnsToCategory(t *testing.T) {
	entries := make([]WizardEntry, 0, WizardEntryDrillThreshold+1)
	for index := 0; index < WizardEntryDrillThreshold+1; index++ {
		key := "entry-" + string(rune('a'+index))
		entries = append(entries, WizardEntry{Key: key, Label: key, Included: true})
	}
	var categoryVisits int
	wizard := &scriptedWizard{selectFn: func(_ int, message string, _ []WizardOption, checked []string) ([]string, error) {
		if strings.Contains(message, "的目录条目") {
			if categoryVisits == 0 {
				categoryVisits++
				return []string{WizardBackValue}, nil // go back from entries
			}
			return []string{"entry-a"}, nil
		}
		if strings.Contains(message, "的分类") {
			categoryVisits++
			return append([]string(nil), checked...), nil
		}
		return append([]string(nil), checked...), nil
	}}
	state := WizardState{Adapters: []WizardAdapter{
		{ID: "pi", Enabled: true, Categories: []WizardCategory{
			{Name: "extensions", Enabled: true, FileCount: len(entries), Entries: entries},
		}},
	}}
	selection, err := RunSelectionWizard(wizard, state)
	if err != nil {
		t.Fatal(err)
	}
	if got := selection.Excluded["pi"]["extensions"]; len(got) != len(entries)-1 {
		t.Fatalf("excluded entries = %#v, want all but entry-a", got)
	}
}

func TestApplySelectionToConfigPreservesWildcardAndDedupes(t *testing.T) {
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {
			Root:        "/changed/root",
			Ignore:      []string{"sessions/"},
			AllowEscape: []string{"skills/agent-browser"},
			Categories: map[string]core.CategoryConfig{
				"extensions": {
					Paths:   []string{"extensions/"},
					Mode:    core.SyncModeMirror,
					Exclude: []string{"*cache*"},
				},
			},
		},
	}, Backup: &core.BackupConfig{}, Secrets: &core.SecretsConfig{Files: map[string]string{"token": "~/.token"}}}
	before := cloneHomerConfig(config)
	ApplySelectionToConfig(&config, WizardSelection{
		Adapters:   map[string]bool{"pi": false},
		Categories: map[string]map[string]bool{"pi": {"extensions": false}},
		Excluded:   map[string]map[string][]string{"pi": {"extensions": {"new-dir", "new-dir", "cache"}}},
	})
	adapterConfig := config.Adapters["pi"]
	if adapterConfig.Enabled == nil || *adapterConfig.Enabled {
		t.Fatal("adapter should be disabled")
	}
	category := adapterConfig.Categories["extensions"]
	if category.Enabled == nil || *category.Enabled {
		t.Fatal("category should be disabled")
	}
	if got, want := category.Exclude, []string{"*cache*", "new-dir"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("exclude = %#v, want %#v", got, want)
	}
	if config.Adapters["pi"].Root != before.Adapters["pi"].Root || !reflect.DeepEqual(config.Backup, before.Backup) || !reflect.DeepEqual(config.Secrets, before.Secrets) {
		t.Fatal("selection changed preserved fields")
	}
}

func TestApplySelectionIdentityDoesNotAddFields(t *testing.T) {
	kind := core.CategoryKindDir
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {
			Root: "/custom/root",
			Categories: map[string]core.CategoryConfig{
				"files": {Paths: []string{"files/"}, Mode: core.SyncModeMirror, Kind: &kind, Exclude: []string{"*cache*"}},
			},
		},
	}, Backup: &core.BackupConfig{Keep: intPointer(4)}, Secrets: &core.SecretsConfig{Recipients: []string{"age1"}}}
	before := cloneHomerConfig(config)
	ApplySelectionToConfig(&config, WizardSelection{
		Adapters:   map[string]bool{"pi": true},
		Categories: map[string]map[string]bool{"pi": {"files": true}},
	})
	if !reflect.DeepEqual(config, before) {
		t.Fatalf("identity selection changed config: got %#v want %#v", config, before)
	}
}

func TestFilterSnapshotsBySelectionDirectoryAndFileKeys(t *testing.T) {
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {
			Categories: map[string]core.CategoryConfig{
				"dirs":  {Paths: []string{"dirs/"}, Mode: core.SyncModeMirror, Exclude: []string{"cache"}},
				"files": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror, Exclude: []string{"skip.json"}},
			},
		},
	}}
	snapshots := []core.AdapterSnapshot{{AdapterID: "pi", Categories: []core.CategorySnapshot{
		{AdapterID: "pi", Category: "dirs", Files: core.SnapshotFiles{
			"cache/a.txt": {Kind: "file", Content: "drop"},
			"keep/a.txt":  {Kind: "file", Content: "keep"},
		}},
		{AdapterID: "pi", Category: "files", Files: core.SnapshotFiles{
			"settings.json": {Kind: "file", Content: "keep"},
			"skip.json":     {Kind: "file", Content: "drop"},
		}},
	}}}
	filtered := FilterSnapshotsBySelection(snapshots, config)
	if len(filtered) != 1 || len(filtered[0].Categories) != 2 {
		t.Fatalf("filtered snapshots = %#v", filtered)
	}
	if len(filtered[0].Categories[0].Files) != 1 || len(filtered[0].Categories[1].Files) != 1 {
		t.Fatalf("filtered files = %#v", filtered)
	}
	if _, ok := filtered[0].Categories[0].Files["keep/a.txt"]; !ok {
		t.Fatal("directory first segment was filtered unexpectedly")
	}
	if _, ok := filtered[0].Categories[1].Files["settings.json"]; !ok {
		t.Fatal("file key was filtered unexpectedly")
	}
}

func TestDefaultWizardPortNonTTYIsIdentity(t *testing.T) {
	port := NewDefaultWizardPort(false)
	checked := []string{"pi", "settings"}
	got, err := port.MultiSelect("ignored", []WizardOption{{Value: "pi"}}, checked)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, checked) {
		t.Fatalf("identity selection = %#v, want %#v", got, checked)
	}
}

func TestRunInitReInitPreservesCustomFieldsAndSkipsStore(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return ""
	})
	config := initTestConfig()
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	wizard := &scriptedWizard{selectFn: func(call int, _ string, options []WizardOption, _ []string) ([]string, error) {
		values := optionValues(options)
		if call == 0 {
			return values, nil
		}
		if strings.Contains(options[0].Label, "settings") && len(values) == 1 {
			return nil, nil
		}
		return values, nil
	}}
	var saved *core.HomerConfig
	writes := 0
	_, err := RunInitWithDeps(InitOptions{HomerHome: home}, InitDeps{
		Wizard: wizard,
		Scan: func(adapterID string, cfg core.AdapterConfig) adapter.ScanOutcome {
			categories := make([]core.CategorySnapshot, 0, len(cfg.Categories))
			for name := range cfg.Categories {
				categories = append(categories, core.CategorySnapshot{AdapterID: adapterID, Category: name, Mode: cfg.Categories[name].Mode, Files: core.SnapshotFiles{"settings.json": {Kind: "file", Content: "x"}}})
			}
			sort.Slice(categories, func(i, j int) bool { return categories[i].Category < categories[j].Category })
			return adapter.ScanOutcome{Snapshot: core.AdapterSnapshot{AdapterID: adapterID, Categories: categories}}
		},
		WriteSnapshot: func(core.HomerPaths, core.AdapterSnapshot) error {
			writes++
			return nil
		},
		SaveConfig: func(_ core.HomerPaths, got core.HomerConfig) error {
			saved = &got
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("re-init wrote %d snapshots", writes)
	}
	if saved == nil {
		t.Fatal("selection adjustment did not save config")
	}
	if saved.Adapters["pi"].Root != config.Adapters["pi"].Root || saved.Adapters["custom"].Root != config.Adapters["custom"].Root {
		t.Fatal("re-init changed adapter roots")
	}
	if !reflect.DeepEqual(saved.Adapters["custom"].Ignore, config.Adapters["custom"].Ignore) || !reflect.DeepEqual(saved.Adapters["custom"].AllowEscape, config.Adapters["custom"].AllowEscape) {
		t.Fatal("re-init changed custom adapter fields")
	}
	if saved.Adapters["pi"].Categories["settings"].Enabled == nil || *saved.Adapters["pi"].Categories["settings"].Enabled {
		t.Fatal("re-init category choice was not persisted")
	}
	if !reflect.DeepEqual(saved.Backup, config.Backup) || !reflect.DeepEqual(saved.Secrets, config.Secrets) {
		t.Fatalf("re-init changed backup/secrets: got %#v/%#v want %#v/%#v", saved.Backup, saved.Secrets, config.Backup, config.Secrets)
	}
}

func TestRunInitCancellationDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	wizard := &scriptedWizard{selectFn: func(int, string, []WizardOption, []string) ([]string, error) {
		return nil, errors.New("cancel")
	}}
	writes, saves := 0, 0
	_, err := RunInitWithDeps(InitOptions{HomerHome: home}, InitDeps{
		Wizard: wizard,
		Scan: func(adapterID string, _ core.AdapterConfig) adapter.ScanOutcome {
			return adapter.ScanOutcome{Snapshot: core.AdapterSnapshot{AdapterID: adapterID}}
		},
		WriteSnapshot: func(core.HomerPaths, core.AdapterSnapshot) error { writes++; return nil },
		SaveConfig:    func(core.HomerPaths, core.HomerConfig) error { saves++; return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "已取消初始化") {
		t.Fatalf("cancel error = %v", err)
	}
	if writes != 0 || saves != 0 {
		t.Fatalf("cancel writes = snapshots %d config %d", writes, saves)
	}
}

func TestRunInitForcePreservesNonBuiltinAndResetsBuiltin(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return ""
	})
	config := initTestConfig()
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	var saved *core.HomerConfig
	_, err := RunInitWithDeps(InitOptions{HomerHome: home, Force: true}, InitDeps{
		Scan: func(adapterID string, _ core.AdapterConfig) adapter.ScanOutcome {
			return adapter.ScanOutcome{Snapshot: core.AdapterSnapshot{AdapterID: adapterID}}
		},
		WriteSnapshot: func(core.HomerPaths, core.AdapterSnapshot) error { return nil },
		SaveConfig: func(_ core.HomerPaths, got core.HomerConfig) error {
			saved = &got
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil {
		t.Fatal("force did not save config")
	}
	if got := saved.Adapters["pi"].Root; got != pi.DefaultPIAdapter.Root {
		t.Fatalf("builtin root = %q, want %q", got, pi.DefaultPIAdapter.Root)
	}
	if got := saved.Adapters["custom"].Root; got != config.Adapters["custom"].Root {
		t.Fatalf("custom root = %q, want %q", got, config.Adapters["custom"].Root)
	}
	if saved.Backup == nil || saved.Backup.Keep == nil || *saved.Backup.Keep != *config.Backup.Keep || saved.Secrets == nil || !reflect.DeepEqual(saved.Secrets.Files, config.Secrets.Files) || !reflect.DeepEqual(saved.Secrets.IgnorePaths, config.Secrets.IgnorePaths) {
		t.Fatalf("force did not preserve backup/secrets: got %#v/%#v want %#v/%#v", saved.Backup, saved.Secrets, config.Backup, config.Secrets)
	}
}

func initTestConfig() core.HomerConfig {
	return core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"pi": {
				Root: "/changed/pi",
				Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
				},
			},
			"custom": {
				Root:        "/custom/root",
				Ignore:      []string{"ignored/"},
				AllowEscape: []string{"allowed/file"},
				Categories: map[string]core.CategoryConfig{
					"config": {Paths: []string{"config.toml"}, Mode: core.SyncModeMirror},
				},
			},
		},
		Backup:  &core.BackupConfig{Keep: intPointer(9)},
		Secrets: &core.SecretsConfig{IgnorePaths: []string{"private/"}, Recipients: []string{}, Files: map[string]string{"token": "~/.token"}},
	}
}

func intPointer(value int) *int { return &value }
