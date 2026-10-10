package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/manifest"
	"github.com/zzjcool/homer-cli/internal/pluginregistry"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
)

func TestMissingToolErrorsNamesOfficialInstaller(t *testing.T) {
	kind := core.CategoryKindManifest
	config := core.HomerConfig{Adapters: map[string]core.AdapterConfig{
		"pi": {Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
			"packages": {Kind: &kind, Mode: core.SyncModeMirror, ListCmd: "pi list", ApplyCmd: "pi install"},
		}},
		"herdr": {Categories: map[string]core.CategoryConfig{
			"config": {Paths: []string{"config.toml"}, Mode: core.SyncModeMirror},
		}},
	}}
	plan := syncx.PullPlan{Actions: []syncx.PullAction{
		{Type: syncx.PullActionWrite, AdapterID: "pi", Category: "settings", RelPath: "settings.json"},
		{Type: syncx.PullActionWrite, AdapterID: "herdr", Category: "config", RelPath: "config.toml"},
	}}
	missing := func(string) (string, error) { return "", errors.New("missing") }
	got := missingToolErrors(config, applyAdapterIDs(plan, nil), missing)
	if len(got) != 1 || !strings.Contains(got[0], "未安装 pi，无法下发适配器 pi。") {
		t.Fatalf("errors = %#v", got)
	}
	install, _ := pluginregistry.OfficialInstall("pi")
	if !strings.Contains(got[0], install) || !strings.Contains(got[0], "复制下面这一行安装后再下发：") {
		t.Fatalf("errors = %#v", got)
	}

	present := func(string) (string, error) { return "/usr/bin/pi", nil }
	if again := missingToolErrors(config, applyAdapterIDs(plan, nil), present); len(again) != 0 {
		t.Fatalf("installed pi still blocked: %#v", again)
	}
}

func TestMissingToolErrorsIgnoresUnknownCommands(t *testing.T) {
	kind := core.CategoryKindManifest
	disabled := false
	config := core.HomerConfig{Adapters: map[string]core.AdapterConfig{
		"vscode": {Categories: map[string]core.CategoryConfig{
			"extensions": {Kind: &kind, Mode: core.SyncModeMirror, ListCmd: "code --list-extensions", ApplyCmd: "code --install-extension"},
		}},
		"pi": {Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
			"packages": {Kind: &kind, Mode: core.SyncModeMirror, ListCmd: "pi list", ApplyCmd: "pi install", Enabled: &disabled},
		}},
	}}
	plan := syncx.PullPlan{Actions: []syncx.PullAction{
		{Type: syncx.PullActionWrite, AdapterID: "pi", Category: "settings", RelPath: "settings.json"},
	}}
	tasks := []manifest.Task{{AdapterID: "vscode", Category: "extensions", ApplyCmd: "code --install-extension", IDs: []string{"pub.one"}}}
	look := func(string) (string, error) { return "", errors.New("missing") }
	if got := missingToolErrors(config, applyAdapterIDs(plan, tasks), look); len(got) != 0 {
		t.Fatalf("unknown or disabled tools blocked dispatch: %#v", got)
	}
}

func TestRunPullStopsWhenPiIsMissing(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(string) string { return home })
	tool := filepath.Join(home, "tool")
	if err := os.MkdirAll(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	kind := core.CategoryKindManifest
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: tool, Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
			"packages": {Kind: &kind, Mode: core.SyncModeMirror, ListCmd: "pi list", ApplyCmd: "pi install"},
		}},
	}}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool, "settings.json"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := func(content string) core.AdapterSnapshot {
		return core.AdapterSnapshot{AdapterID: "pi", Categories: []core.CategorySnapshot{
			{AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{
				"settings.json": {Kind: "file", Content: content},
			}},
		}}
	}
	sources := syncx.SyncSources{
		Base:   []core.AdapterSnapshot{file("local\n")},
		Local:  []core.AdapterSnapshot{file("local\n")},
		Remote: []core.AdapterSnapshot{file("from-hub\n")},
	}
	report := RunPull(PullOptions{HomerHome: home, Yes: true}, &PullDeps{
		UI:          HeadlessUI{},
		NoFetch:     true,
		HubSnapshot: sources.Remote,
		Sources:     sources,
		Look:        func(string) (string, error) { return "", errors.New("missing") },
	})
	if report.OK || report.Status != PullStatusError {
		t.Fatalf("report = %#v", report)
	}
	if !strings.Contains(strings.Join(report.Errors, "\n"), "curl -fsSL https://pi.dev/install.sh | sh") && !strings.Contains(strings.Join(report.Errors, "\n"), "https://pi.dev/install.ps1") {
		t.Fatalf("errors = %#v", report.Errors)
	}
	applied, err := os.ReadFile(filepath.Join(tool, "settings.json"))
	if err != nil || string(applied) != "local\n" {
		t.Fatalf("tool file = %q err=%v", applied, err)
	}
}

func TestRunPullAppliesWhenPiIsInstalled(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(string) string { return home })
	tool := filepath.Join(home, "tool")
	if err := os.MkdirAll(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	kind := core.CategoryKindManifest
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"pi": {Root: tool, Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
			"packages": {Kind: &kind, Mode: core.SyncModeMirror, ListCmd: "pi list", ApplyCmd: "pi install"},
		}},
	}}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool, "settings.json"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := func(content string) core.AdapterSnapshot {
		return core.AdapterSnapshot{AdapterID: "pi", Categories: []core.CategorySnapshot{
			{AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{
				"settings.json": {Kind: "file", Content: content},
			}},
		}}
	}
	sources := syncx.SyncSources{
		Base:   []core.AdapterSnapshot{file("local\n")},
		Local:  []core.AdapterSnapshot{file("local\n")},
		Remote: []core.AdapterSnapshot{file("from-hub\n")},
	}
	report := RunPull(PullOptions{HomerHome: home, Yes: true}, &PullDeps{
		UI:          HeadlessUI{},
		NoFetch:     true,
		HubSnapshot: sources.Remote,
		Sources:     sources,
		Look: func(name string) (string, error) {
			if name != "pi" {
				return "", errors.New(name)
			}
			return "/usr/bin/pi", nil
		},
	})
	if !report.OK || report.Status != PullStatusApplied {
		t.Fatalf("report = %#v errors=%v", report, report.Errors)
	}
	applied, err := os.ReadFile(filepath.Join(tool, "settings.json"))
	if err != nil || string(applied) != "from-hub\n" {
		t.Fatalf("tool file = %q err=%v", applied, err)
	}
}
