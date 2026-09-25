package commands

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
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
