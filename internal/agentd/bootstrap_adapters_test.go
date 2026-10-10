package agentd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter/keys"
	"github.com/zzjcool/homer-cli/internal/core"
)

func bootstrapPaths(t *testing.T, home string) core.HomerPaths {
	t.Helper()
	return core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return os.Getenv(key)
	})
}

// A machine configured before the center grew a keyring adapter must pick it
// up on the next dispatch; otherwise it answers "没有适配器 keyring" forever.
func TestBootstrapAddsAdaptersTheCenterHasAndTheMachineLacks(t *testing.T) {
	home := t.TempDir()
	paths := bootstrapPaths(t, home)
	piRoot := "~/custom/pi"
	local := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: piRoot, Categories: map[string]core.CategoryConfig{"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror}}},
	}}
	if err := core.SaveConfig(paths, local); err != nil {
		t.Fatal(err)
	}
	center := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi":           {Root: "~/.pi/agent", Categories: map[string]core.CategoryConfig{"settings": {Paths: []string{"other.json"}, Mode: core.SyncModeMirror}}},
		keys.AdapterID: keys.DefaultAdapter,
	}}
	meta := centerMeta(t, home, center)

	exec := &localExecutor{homerHome: home}
	if err := exec.bootstrapFromGeneration(nil, meta, nil); err != nil {
		t.Fatal(err)
	}
	got, err := core.LoadConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Adapters[keys.AdapterID]; !ok {
		t.Fatalf("keyring adapter not added: %v", got.Adapters)
	}
	if got.Adapters["pi"].Root != piRoot || got.Adapters["pi"].Categories["settings"].Paths[0] != "settings.json" {
		t.Fatalf("the machine's own adapter must be left as the user set it: %+v", got.Adapters["pi"])
	}
}

func TestApplyConfigPolicyCenterReplacesOnlySelectedAdapterAndBacksUp(t *testing.T) {
	home := t.TempDir()
	paths := bootstrapPaths(t, home)
	local := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {
			Root: "~/.pi/local",
			Categories: map[string]core.CategoryConfig{
				"settings": {Paths: []string{"local-settings.json"}, Mode: core.SyncModeMerge},
			},
			Ignore: []string{"local-cache/"}, AllowEscape: []string{"local-shared/"},
		},
		"herdr": {
			Root: "~/.config/local-herdr",
			Categories: map[string]core.CategoryConfig{
				"config": {Paths: []string{"local.toml"}, Mode: core.SyncModeMirror},
			},
		},
	}}
	if err := core.SaveConfig(paths, local); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	center := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {
			Root: "~/.pi/agent",
			Categories: map[string]core.CategoryConfig{
				"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
				"files":    {Paths: []string{"files/"}, Mode: core.SyncModeMirror},
			},
			Ignore: []string{"center-cache/"}, AllowEscape: []string{"center-shared/"},
		},
		"herdr": {
			Root: "~/.config/herdr",
			Categories: map[string]core.CategoryConfig{
				"config": {Paths: []string{"config.toml"}, Mode: core.SyncModeMerge},
			},
		},
	}}
	if err := applyConfigPolicy(paths, centerMeta(t, home, center), map[string]string{"pi": "center"}); err != nil {
		t.Fatalf("apply center policy: %v", err)
	}
	got, err := core.LoadConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Adapters["pi"], center.Adapters["pi"]) {
		t.Fatalf("selected adapter was not replaced: got=%+v want=%+v", got.Adapters["pi"], center.Adapters["pi"])
	}
	if !reflect.DeepEqual(got.Adapters["herdr"], local.Adapters["herdr"]) {
		t.Fatalf("unselected adapter changed: got=%+v want=%+v", got.Adapters["herdr"], local.Adapters["herdr"])
	}
	backupFiles, err := filepath.Glob(filepath.Join(paths.BackupsDir, "*", "*-pull", "homer.json"))
	if err != nil || len(backupFiles) != 1 {
		t.Fatalf("config backup files = %v, err=%v", backupFiles, err)
	}
	backedUp, err := os.ReadFile(backupFiles[0])
	if err != nil || string(backedUp) != string(before) {
		t.Fatalf("backup content = %q, err=%v; want original homer.json", backedUp, err)
	}
}

func TestApplyConfigPolicyKeepAddsCategoriesAndUnionsPaths(t *testing.T) {
	home := t.TempDir()
	paths := bootstrapPaths(t, home)
	localSettings := core.CategoryConfig{
		Paths:   []string{"settings.json", filepath.Join("nested", "local.json")},
		Mode:    core.SyncModeMerge,
		Exclude: []string{"local-exclude"}, ExcludeKeys: []string{"local-key"},
	}
	localMachineCategory := core.CategoryConfig{Paths: []string{"machine-only/"}, Mode: core.SyncModeMirror}
	local := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {
			Root: "~/.pi/local",
			Categories: map[string]core.CategoryConfig{
				"settings": localSettings,
				"machine":  localMachineCategory,
			},
			Ignore: []string{"local-cache/"}, AllowEscape: []string{"local-shared/"},
		},
	}}
	if err := core.SaveConfig(paths, local); err != nil {
		t.Fatal(err)
	}
	centerSettings := core.CategoryConfig{
		Paths: []string{"settings.json", "mcp.json", filepath.ToSlash(filepath.Join("nested", "local.json")), "center.json"},
		Mode:  core.SyncModeMirror,
	}
	centerFiles := core.CategoryConfig{Paths: []string{"files/"}, Mode: core.SyncModeMirror}
	center := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {
			Root: "~/.pi/agent",
			Categories: map[string]core.CategoryConfig{
				"settings": centerSettings,
				"files":    centerFiles,
			},
			Ignore: []string{"center-cache/"}, AllowEscape: []string{"center-shared/"},
		},
	}}
	if err := applyConfigPolicy(paths, centerMeta(t, home, center), map[string]string{"pi": "keep"}); err != nil {
		t.Fatalf("apply keep policy: %v", err)
	}
	got, err := core.LoadConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	gotPI := got.Adapters["pi"]
	wantPaths := []string{"settings.json", filepath.Join("nested", "local.json"), "mcp.json", "center.json"}
	if !reflect.DeepEqual(gotPI.Categories["settings"].Paths, wantPaths) {
		t.Fatalf("settings paths = %#v, want %#v", gotPI.Categories["settings"].Paths, wantPaths)
	}
	wantSettings := localSettings
	wantSettings.Paths = wantPaths
	if !reflect.DeepEqual(gotPI.Categories["settings"], wantSettings) {
		t.Fatalf("keep changed shared category fields beyond paths: got=%+v want=%+v", gotPI.Categories["settings"], wantSettings)
	}
	if !reflect.DeepEqual(gotPI.Categories["files"], centerFiles) {
		t.Fatalf("new category definition = %+v, want center definition %+v", gotPI.Categories["files"], centerFiles)
	}
	if !reflect.DeepEqual(gotPI.Categories["machine"], localMachineCategory) {
		t.Fatalf("machine-only category changed: %+v", gotPI.Categories["machine"])
	}
	if gotPI.Root != local.Adapters["pi"].Root ||
		!reflect.DeepEqual(gotPI.Ignore, local.Adapters["pi"].Ignore) ||
		!reflect.DeepEqual(gotPI.AllowEscape, local.Adapters["pi"].AllowEscape) {
		t.Fatalf("keep changed adapter-level fields: %+v", gotPI)
	}
}

func TestBootstrapFreshConfigKeepsWholeFileWriteAndFailOpen(t *testing.T) {
	home := t.TempDir()
	paths := bootstrapPaths(t, home)
	exec := &localExecutor{homerHome: home}
	if err := exec.bootstrapFromGeneration(nil, nil, map[string]string{"pi": "center"}); err != nil {
		t.Fatalf("empty metadata bootstrap: %v", err)
	}
	if _, err := os.Stat(paths.ConfigFile); !os.IsNotExist(err) {
		t.Fatalf("empty metadata wrote homer.json: err=%v", err)
	}
	center := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: "~/.pi/agent", Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
		}},
	}}
	meta := centerMeta(t, home, center)
	if err := exec.bootstrapFromGeneration(nil, meta, map[string]string{"pi": "keep"}); err != nil {
		t.Fatalf("fresh machine bootstrap: %v", err)
	}
	written, err := os.ReadFile(paths.ConfigFile)
	if err != nil || string(written) != string(meta) {
		t.Fatalf("fresh machine config = %q, err=%v; want exact center metadata", written, err)
	}
	if err := applyConfigPolicy(paths, nil, map[string]string{"pi": "center"}); err != nil {
		t.Fatalf("empty metadata must fail open: %v", err)
	}
	if err := applyConfigPolicy(paths, []byte("not valid config"), map[string]string{"pi": "center"}); err != nil {
		t.Fatalf("invalid center config must fail open: %v", err)
	}
	afterFailOpen, err := os.ReadFile(paths.ConfigFile)
	if err != nil || string(afterFailOpen) != string(written) {
		t.Fatalf("fail-open config changed local bytes: %q, err=%v", afterFailOpen, err)
	}

	badHome := t.TempDir()
	badPaths := bootstrapPaths(t, badHome)
	if err := os.MkdirAll(filepath.Dir(badPaths.ConfigFile), 0o755); err != nil {
		t.Fatal(err)
	}
	badLocal := []byte("not valid config\n")
	if err := os.WriteFile(badPaths.ConfigFile, badLocal, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyConfigPolicy(badPaths, meta, map[string]string{"pi": "center"}); err != nil {
		t.Fatalf("broken local config must fail open: %v", err)
	}
	stillBad, err := os.ReadFile(badPaths.ConfigFile)
	if err != nil || string(stillBad) != string(badLocal) {
		t.Fatalf("broken local config was changed: %q, err=%v", stillBad, err)
	}
}

func TestBootstrapKeepsConfigWhenNothingIsMissing(t *testing.T) {
	home := t.TempDir()
	paths := bootstrapPaths(t, home)
	local := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{keys.AdapterID: keys.DefaultAdapter}}
	if err := core.SaveConfig(paths, local); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	exec := &localExecutor{homerHome: home}
	if err := exec.bootstrapFromGeneration(nil, centerMeta(t, home, local), nil); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(paths.ConfigFile)
	if string(before) != string(after) {
		t.Fatal("config must not be rewritten when the machine already has every adapter")
	}
	// An unreadable center config must never block a pull.
	if err := exec.bootstrapFromGeneration(nil, []byte("not json"), nil); err != nil {
		t.Fatalf("bad center meta = %v", err)
	}
}

// centerMeta serializes a config the way the hub stores homer.json.
func centerMeta(t *testing.T, home string, config core.HomerConfig) []byte {
	t.Helper()
	other := bootstrapPaths(t, t.TempDir())
	if err := core.SaveConfig(other, config); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(other.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
