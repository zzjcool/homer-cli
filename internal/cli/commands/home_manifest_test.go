package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/manifest"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
)

func w4WriteManifestHomeClone(t *testing.T, config core.HomerConfig, snapshot core.AdapterSnapshot, dest string) {
	t.Helper()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return dest
		}
		return ""
	})
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	if err := core.WriteSnapshotToStore(paths, snapshot); err != nil {
		t.Fatal(err)
	}
}

func w4ManifestHomeFixture(t *testing.T) (core.HomerConfig, core.AdapterSnapshot, string) {
	t.Helper()
	root := t.TempDir()
	toolRoot := filepath.Join(root, "tool")
	if err := os.MkdirAll(toolRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"remote": {
				Root: toolRoot,
				Categories: map[string]core.CategoryConfig{
					"plugins": w4ManifestCategoryConfig("remote-list --raw", "remote-install --plugin"),
				},
			},
		},
	}
	snapshot := w4ManifestSnapshot("remote", "plugins", "pub.local\npub.remote\n")
	return config, snapshot, root
}

func TestRunHomeManifestGatePreviewAbortAndYesWarning(t *testing.T) {
	config, snapshot, root := w4ManifestHomeFixture(t)
	commands := &w4ManifestCommandFake{output: []byte("pub.local\n")}
	ui := &w4ManifestConfirmFake{}
	target := filepath.Join(root, "aborted")
	aborted := RunHome(HomeOptions{HomerHome: target, RepoURL: "fixture", Mode: syncx.FirstContactPull}, &HomeDeps{
		UI:       ui,
		Commands: commands,
		Clone: func(_, dest string) error {
			w4WriteManifestHomeClone(t, config, snapshot, dest)
			return nil
		},
	})
	if aborted.Status != HomeStatusAborted || aborted.ExitCode() != 1 {
		t.Fatalf("home confirm=false report = %#v", aborted)
	}
	if len(commands.applyIDs) != 0 {
		t.Fatalf("home confirm=false executed manifest IDs: %#v", commands.applyIDs)
	}
	for _, want := range []string{
		"+ pub.remote",
		"⚠ 远端配置声明了 manifest 分类，将执行外部命令",
		"listCmd: remote-list --raw",
		"applyCmd: remote-install --plugin",
		"manifest",
	} {
		if !strings.Contains(ui.message, want) {
			t.Fatalf("home manifest preview missing %q: %s", want, ui.message)
		}
	}
	if !strings.Contains(strings.Join(aborted.Errors, "\n"), "manifest") {
		t.Fatalf("aborted home error lacks manifest hint: %#v", aborted.Errors)
	}

	commands = &w4ManifestCommandFake{output: []byte("pub.local\n")}
	yesTarget := filepath.Join(root, "yes")
	homed := RunHome(HomeOptions{HomerHome: yesTarget, RepoURL: "fixture", Mode: syncx.FirstContactPull, Yes: true}, &HomeDeps{
		Commands: commands,
		Clone: func(_, dest string) error {
			w4WriteManifestHomeClone(t, config, snapshot, dest)
			return nil
		},
	})
	if homed.Status != HomeStatusHomed || homed.ExitCode() != 0 {
		t.Fatalf("home --yes report = %#v", homed)
	}
	if homed.Manifest == nil || len(homed.Manifest.Installed) != 1 || homed.Manifest.Installed[0] != "remote/plugins:pub.remote" {
		t.Fatalf("home manifest report = %#v", homed.Manifest)
	}
	if !strings.Contains(strings.Join(homed.Warnings, "\n"), "已按 --yes 确认执行远端声明的 manifest 命令") {
		t.Fatalf("home --yes gate warning missing: %#v", homed.Warnings)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(RenderHomeJSON(homed)), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["manifest"]; !ok {
		t.Fatalf("home JSON omitted manifest: %s", RenderHomeJSON(homed))
	}
}

func TestRunHomeManifestNonTTYWithoutYesAborts(t *testing.T) {
	config, snapshot, root := w4ManifestHomeFixture(t)
	commands := &w4ManifestCommandFake{output: []byte("pub.local\n")}
	report := RunHome(HomeOptions{HomerHome: filepath.Join(root, "non-tty"), RepoURL: "fixture", Mode: syncx.FirstContactPull}, &HomeDeps{
		Commands: commands,
		Clone: func(_, dest string) error {
			w4WriteManifestHomeClone(t, config, snapshot, dest)
			return nil
		},
	})
	if report.Status != HomeStatusAborted {
		t.Fatalf("non-TTY home report = %#v", report)
	}
	if len(commands.applyIDs) != 0 {
		t.Fatalf("non-TTY home executed manifest IDs: %#v", commands.applyIDs)
	}
	message := strings.Join(report.Errors, "\n")
	if !strings.Contains(message, "manifest") {
		t.Fatalf("non-TTY error lacks manifest hint: %q", message)
	}
}

func TestRunHomeManifestSkipHasNoTaskOrApply(t *testing.T) {
	config, snapshot, root := w4ManifestHomeFixture(t)
	commands := &w4ManifestCommandFake{output: []byte("pub.local\n")}
	report := RunHome(HomeOptions{HomerHome: filepath.Join(root, "skip"), RepoURL: "fixture", Mode: syncx.FirstContactSkip, Yes: true}, &HomeDeps{
		Commands: commands,
		Clone: func(_, dest string) error {
			w4WriteManifestHomeClone(t, config, snapshot, dest)
			return nil
		},
	})
	if report.Status != HomeStatusHomed || report.ExitCode() != 0 {
		t.Fatalf("home skip report = %#v", report)
	}
	if report.Manifest != nil {
		t.Fatalf("home skip unexpectedly reported manifest task: %#v", report.Manifest)
	}
	if len(commands.applyIDs) != 0 {
		t.Fatalf("home skip executed manifest IDs: %#v", commands.applyIDs)
	}
	if strings.Contains(strings.Join(report.Warnings, "\n"), "远端声明") {
		t.Fatalf("home skip emitted remote manifest warning: %#v", report.Warnings)
	}
}

func TestRunHomeAppliesFilesBeforeManifest(t *testing.T) {
	root := t.TempDir()
	toolRoot := filepath.Join(root, "tool")
	if err := os.MkdirAll(toolRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	kind := core.CategoryKindManifest
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"remote": {
				Root: toolRoot,
				Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
					"plugins":  {Kind: &kind, Mode: core.SyncModeMirror, ListCmd: "list", ApplyCmd: "install"},
				},
			},
		},
	}
	snapshot := core.AdapterSnapshot{AdapterID: "remote", Categories: []core.CategorySnapshot{
		{AdapterID: "remote", Category: "settings", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{"settings.json": {Kind: "file", Content: "remote\n"}}},
		{AdapterID: "remote", Category: "plugins", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{manifest.VirtualFileName("plugins"): {Kind: "file", Content: "pub.remote\n"}}},
	}}
	commands := &w4ManifestCommandFake{output: []byte("")}
	commands.beforeApply = func(string) {
		content, err := os.ReadFile(filepath.Join(toolRoot, "settings.json"))
		if err != nil || string(content) != "remote\n" {
			t.Fatalf("manifest ran before file apply: content=%q err=%v", content, err)
		}
	}
	report := RunHome(HomeOptions{HomerHome: filepath.Join(root, "home"), RepoURL: "fixture", Mode: syncx.FirstContactPull, Yes: true}, &HomeDeps{
		Commands: commands,
		Clone: func(_, dest string) error {
			w4WriteManifestHomeClone(t, config, snapshot, dest)
			return nil
		},
	})
	if report.Status != HomeStatusHomed || report.Manifest == nil || len(report.Manifest.Installed) != 1 {
		t.Fatalf("ordering home report = %#v", report)
	}
}
