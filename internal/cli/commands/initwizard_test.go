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
			values := optionValues(options)
			return values[:len(values)-1], nil
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
