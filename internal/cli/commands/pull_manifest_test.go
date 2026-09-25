package commands

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
	"github.com/zzjcool/homer-cli/internal/manifest"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
)

type w4ManifestCommandFake struct {
	output      []byte
	outputErr   error
	applyErrs   map[string]error
	outputCmds  []string
	applyCmds   []string
	applyIDs    []string
	beforeApply func(id string)
}

func (fake *w4ManifestCommandFake) Output(command string) ([]byte, error) {
	fake.outputCmds = append(fake.outputCmds, command)
	return fake.output, fake.outputErr
}

func (fake *w4ManifestCommandFake) Apply(command, id string) error {
	fake.applyCmds = append(fake.applyCmds, command)
	fake.applyIDs = append(fake.applyIDs, id)
	if fake.beforeApply != nil {
		fake.beforeApply(id)
	}
	return fake.applyErrs[id]
}

var _ manifest.CommandPort = (*w4ManifestCommandFake)(nil)

type w4ManifestConfirmFake struct {
	confirmed bool
	message   string
}

func (fake *w4ManifestConfirmFake) Confirm(message string, _ bool) bool {
	fake.message = message
	return fake.confirmed
}

func w4ManifestCategoryConfig(listCmd, applyCmd string) core.CategoryConfig {
	kind := core.CategoryKindManifest
	return core.CategoryConfig{
		Kind:     &kind,
		Mode:     core.SyncModeMirror,
		ListCmd:  listCmd,
		ApplyCmd: applyCmd,
	}
}

func w4ManifestSnapshot(adapterID, category, content string) core.AdapterSnapshot {
	return core.AdapterSnapshot{
		AdapterID: adapterID,
		Categories: []core.CategorySnapshot{{
			AdapterID: adapterID,
			Category:  category,
			Mode:      core.SyncModeMirror,
			Files: core.SnapshotFiles{
				manifest.VirtualFileName(category): {Kind: "file", Content: content},
			},
		}},
	}
}

func w4WriteManifestPullFixture(t *testing.T, listCmd, applyCmd string) (core.HomerPaths, core.HomerConfig, syncx.SyncSources) {
	t.Helper()
	paths := writeTestPaths(t)
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"fake": {
				Root: filepath.Join(paths.Home, "tool"),
				Categories: map[string]core.CategoryConfig{
					"plugins": w4ManifestCategoryConfig(listCmd, applyCmd),
				},
			},
		},
	}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(config.Adapters["fake"].Root, 0o755); err != nil {
		t.Fatal(err)
	}
	return paths, config, syncx.SyncSources{
		Base:   []core.AdapterSnapshot{w4ManifestSnapshot("fake", "plugins", "pub.one\n")},
		Local:  []core.AdapterSnapshot{w4ManifestSnapshot("fake", "plugins", "pub.one\n")},
		Remote: []core.AdapterSnapshot{w4ManifestSnapshot("fake", "plugins", "pub.one\npub.two\n")},
	}
}

func TestRunPullWarnsBeforeFastForwardWhenManifestCommandChanges(t *testing.T) {
	paths, _, sources := w4WriteManifestPullFixture(t, "fake list --remote", "fake install")
	raw, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	remoteConfig := strings.Replace(string(raw), "fake list --remote", "fake new-list --remote", 1)
	git := fakeWriteGit("base-commit", "remote-commit")
	git.Exec = func(_ string, args []string, _ time.Duration) gitx.ExecResult {
		if len(args) == 2 && args[0] == "show" {
			return gitx.ExecResult{OK: true, Stdout: remoteConfig}
		}
		return gitx.ExecResult{}
	}
	commands := &w4ManifestCommandFake{output: []byte("pub.one\n")}
	report := RunPull(PullOptions{HomerHome: paths.Home, Yes: true}, &PullDeps{
		Sources:  sources,
		NoFetch:  true,
		Git:      git,
		Commands: commands,
	})
	joined := strings.Join(report.Warnings, "\n")
	if !strings.Contains(joined, "远端配置变更了 manifest 命令") || !strings.Contains(joined, "fake list --remote") || !strings.Contains(joined, "fake new-list --remote") {
		t.Fatalf("manifest command warning = %#v", report.Warnings)
	}
	if !strings.Contains(RenderPullReport(report), "远端配置变更了 manifest 命令") {
		t.Fatalf("pull output omitted manifest command warning: %s", RenderPullReport(report))
	}
}

func TestRunPullCarriesMissingManifestCLIWarning(t *testing.T) {
	paths := writeTestPaths(t)
	root := filepath.Join(paths.Home, "tool")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	kind := core.CategoryKindManifest
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"vscode": {Root: root, Categories: map[string]core.CategoryConfig{
			"extensions": {Kind: &kind, Mode: core.SyncModeMirror, ListCmd: "definitely-not-installed-code --list", ApplyCmd: "code --install-extension"},
		}},
	}}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	report := RunPull(PullOptions{HomerHome: paths.Home, Yes: true}, &PullDeps{
		NoFetch: true,
		Git:     fakeWriteGit("base-commit", "remote-commit"),
	})
	joined := strings.Join(report.Warnings, "\\n")
	if !strings.Contains(joined, "vscode: definitely-not-installed-code 未安装") || !strings.Contains(joined, "pull 时远端清单将视为全量待装") {
		t.Fatalf("pull missing CLI warning = %#v", report.Warnings)
	}
}

func TestRunPullManifestPreviewAbortAndApplyReport(t *testing.T) {
	paths, _, sources := w4WriteManifestPullFixture(t, "fake list --remote", "fake install")
	commands := &w4ManifestCommandFake{output: []byte("pub.one\n")}
	ui := &w4ManifestConfirmFake{}
	git := fakeWriteGit("base-commit", "remote-commit")

	aborted := RunPull(PullOptions{HomerHome: paths.Home}, &PullDeps{
		UI:       ui,
		Sources:  sources,
		NoFetch:  true,
		Git:      git,
		Commands: commands,
	})
	if aborted.Status != PullStatusAborted || aborted.ExitCode() != 1 {
		t.Fatalf("aborted pull report = %#v", aborted)
	}
	if len(commands.applyIDs) != 0 {
		t.Fatalf("confirm=false executed manifest IDs: %#v", commands.applyIDs)
	}
	for _, want := range []string{"+ pub.two", "安装扩展 1 个（fake/plugins）"} {
		if !strings.Contains(ui.message, want) {
			t.Fatalf("manifest preview missing %q: %s", want, ui.message)
		}
	}

	commands = &w4ManifestCommandFake{output: []byte("pub.one\n")}
	applied := RunPull(PullOptions{HomerHome: paths.Home, Yes: true}, &PullDeps{
		Sources:  sources,
		NoFetch:  true,
		Git:      fakeWriteGit("base-commit", "remote-commit"),
		Commands: commands,
	})
	if applied.Status != PullStatusApplied || applied.ExitCode() != 0 {
		t.Fatalf("applied pull report = %#v", applied)
	}
	if applied.Manifest == nil || len(applied.Manifest.Installed) != 1 || applied.Manifest.Installed[0] != "fake/plugins:pub.two" {
		t.Fatalf("pull manifest report = %#v", applied.Manifest)
	}
	if got, want := commands.applyIDs, []string{"pub.two"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("applied IDs = %#v, want %#v", got, want)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(RenderPullJSON(applied)), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["manifest"]; !ok {
		t.Fatalf("pull JSON omitted manifest: %s", RenderPullJSON(applied))
	}
}

func TestRunPullManifestFailureContinuesAndWarns(t *testing.T) {
	paths, _, sources := w4WriteManifestPullFixture(t, "fake list", "fake install")
	commands := &w4ManifestCommandFake{
		output:    []byte("pub.one\n"),
		applyErrs: map[string]error{"pub.bad": errors.New("installer failed")},
	}
	sources.Remote[0].Categories[0].Files[manifest.VirtualFileName("plugins")] = core.SnapshotEntry{Kind: "file", Content: "pub.bad\npub.good\n"}

	report := RunPull(PullOptions{HomerHome: paths.Home, Yes: true}, &PullDeps{
		Sources:  sources,
		NoFetch:  true,
		Git:      fakeWriteGit("base-commit", "remote-commit"),
		Commands: commands,
	})
	if report.Status != PullStatusApplied || report.ExitCode() != 0 {
		t.Fatalf("failure pull report = %#v", report)
	}
	if report.Manifest == nil || len(report.Manifest.Installed) != 1 || report.Manifest.Installed[0] != "fake/plugins:pub.good" {
		t.Fatalf("failure manifest report = %#v", report.Manifest)
	}
	if len(report.Manifest.Failed) != 1 || !strings.Contains(report.Manifest.Failed[0], "fake/plugins:pub.bad: installer failed") {
		t.Fatalf("failure details = %#v", report.Manifest.Failed)
	}
	if !strings.Contains(strings.Join(report.Warnings, "\n"), "pub.bad") {
		t.Fatalf("failure warning missing: %#v", report.Warnings)
	}
}

func TestRunPullNoApplySkipsManifestCommand(t *testing.T) {
	paths, _, sources := w4WriteManifestPullFixture(t, "fake list", "fake install")
	commands := &w4ManifestCommandFake{output: []byte("pub.one\n")}
	report := RunPull(PullOptions{HomerHome: paths.Home, Yes: true}, &PullDeps{
		Sources:  sources,
		NoFetch:  true,
		NoApply:  true,
		Git:      fakeWriteGit("base-commit", "remote-commit"),
		Commands: commands,
	})
	if report.Status != PullStatusApplied || report.Manifest == nil {
		t.Fatalf("no-apply report = %#v", report)
	}
	if len(commands.applyIDs) != 0 || len(report.Manifest.Installed) != 0 {
		t.Fatalf("no-apply executed manifest: commands=%#v report=%#v", commands.applyIDs, report.Manifest)
	}
}
