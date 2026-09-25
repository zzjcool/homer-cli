package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

func testCLIConfig() core.HomerConfig {
	return core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"pi": {
				Root: "/unused",
				Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
				},
			},
		},
	}
}

func mirrorSnapshot(files map[string]string) []core.AdapterSnapshot {
	entries := make(core.SnapshotFiles, len(files))
	for path, content := range files {
		entries[path] = core.SnapshotEntry{Kind: "file", Content: content}
	}
	return []core.AdapterSnapshot{{AdapterID: "pi", Categories: []core.CategorySnapshot{{
		AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror, Files: entries,
	}}}}
}

func jsonEntry(content string) core.SnapshotEntry {
	return core.SnapshotEntry{Kind: "json", Content: content}
}

func mergeSnapshot(content string) []core.AdapterSnapshot {
	return []core.AdapterSnapshot{{AdapterID: "pi", Categories: []core.CategorySnapshot{{
		AdapterID: "pi", Category: "settings", Mode: core.SyncModeMerge,
		Files: core.SnapshotFiles{"settings.json": jsonEntry(content)},
	}}}}
}

func TestRenderStatusExactUpDownCounts(t *testing.T) {
	report := StatusReport{
		Adapters: []StatusAdapterReport{{
			ID: "pi", Push: 3, Pull: 1,
			Categories: []StatusCategoryReport{{Name: "settings", Push: 3, Pull: 1}},
		}},
		Errors: []string{},
	}
	if got, want := RenderStatus(report), "pi  ↑3 ↓1"; got != want {
		t.Fatalf("RenderStatus = %q, want %q", got, want)
	}
	if got := RenderStatus(report, RenderStatusOptions{Verbose: true}); !strings.Contains(got, "  settings  ↑3 ↓1") {
		t.Fatalf("verbose status = %q", got)
	}
}

func TestPromptFallbackDoesNotBlock(t *testing.T) {
	port := NewDefaultPromptPort(false)
	if got := port.Confirm("danger", false); got {
		t.Fatal("non-TTY confirm must default to false")
	}
	opts := []SelectOption{{Value: "local", Label: "local"}, {Value: "remote", Label: "remote"}}
	if got := port.Select("choice", opts, ""); got != "local" {
		t.Fatalf("select fallback = %q, want first option", got)
	}
	if got := port.Select("choice", opts, "remote"); got != "remote" {
		t.Fatalf("select explicit fallback = %q", got)
	}
}

func TestDiffLinesAndJSONShape(t *testing.T) {
	if got := FormatValue("dark"); got != "dark" {
		t.Fatalf("FormatValue string = %q", got)
	}
	parsedValue, err := orderedjson.Parse([]byte(`{"arr":[1,2]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatValue(parsedValue); got != `{"arr":[1,2]}` {
		t.Fatalf("FormatValue object = %q", got)
	}
	numberValue, err := orderedjson.Parse([]byte("1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatValue(numberValue); got != "1" {
		t.Fatalf("FormatValue number = %q", got)
	}
	if got, want := DiffLines("a\nb\n", "a\nc\n"), []string{" a", "-b", "+c"}; !equalStrings(got, want) {
		t.Fatalf("DiffLines = %#v, want %#v", got, want)
	}
	if got := DiffLines("x\ny\n", ""); !equalStrings(got, []string{"-x", "-y"}) {
		t.Fatalf("delete diff = %#v", got)
	}

	report := StatusReport{Adapters: []StatusAdapterReport{{ID: "pi", Push: 3, Pull: 1, Categories: []StatusCategoryReport{{Name: "settings", Push: 3, Pull: 1}}}}, Errors: []string{}, Warnings: []string{"store dirty"}}
	raw := RenderStatusJSON(report)
	value, err := orderedjson.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("status JSON parse: %v\n%s", err, raw)
	}
	object, ok := value.(*orderedjson.Object)
	if !ok || object == nil {
		t.Fatalf("status JSON root = %#v", value)
	}
	for _, key := range []string{"errors", "adapters", "warnings"} {
		if _, ok := object.M[key]; !ok {
			t.Fatalf("status JSON missing %q: %s", key, raw)
		}
	}
}

func TestRunDispatchStrictUnknownFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runWithIO([]string{"status", "--not-a-flag"}, &out, &errOut); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "未知选项") || !strings.Contains(errOut.String(), "用法: homer status") {
		t.Fatalf("usage error = %q", errOut.String())
	}
}

func TestRunDispatchHelpDoesNotNeedConfig(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runWithIO([]string{"diff", "--help"}, &out, &errOut); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "用法: homer diff") {
		t.Fatalf("help = %q", out.String())
	}
}

func TestInitWithHandSnapshotDeps(t *testing.T) {
	home := t.TempDir()
	fake := core.AdapterSnapshot{
		AdapterID: "pi",
		Categories: []core.CategorySnapshot{{
			AdapterID: "pi", Category: "settings", Mode: core.SyncModeMerge,
			Files: core.SnapshotFiles{"settings.json": jsonEntry(`{"theme":"dark"}`)},
		}},
	}
	report, err := commands.RunInitWithDeps(commands.InitOptions{HomerHome: home, Adapters: []string{"pi"}}, commands.InitDeps{
		Scan: func(adapterID string, _ core.AdapterConfig) adapter.ScanOutcome {
			return adapter.ScanOutcome{Snapshot: fake}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Adapters) != 1 || report.Adapters[0].Categories[0].FileCount != 1 {
		t.Fatalf("init report = %#v", report)
	}
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return ""
	})
	if _, err := os.Stat(filepath.Join(paths.StoreDir, "pi", "settings", "settings.json")); err != nil {
		t.Fatalf("snapshot was not written: %v", err)
	}
}

func TestInitExistingConfigRefusesOverwrite(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return ""
	})
	if err := core.SaveConfig(paths, testCLIConfig()); err != nil {
		t.Fatal(err)
	}
	_, err := commands.RunInit(commands.InitOptions{HomerHome: home})
	if err == nil || !strings.Contains(err.Error(), "拒绝覆盖") {
		t.Fatalf("RunInit error = %v", err)
	}
}

func TestInitRegistersFourAdapters(t *testing.T) {
	home := t.TempDir()
	oldHome, hadHome := os.LookupEnv("HOME")
	if err := os.Setenv("HOME", filepath.Join(home, "fake-home")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadHome {
			_ = os.Setenv("HOME", oldHome)
		} else {
			_ = os.Unsetenv("HOME")
		}
	})

	report, err := commands.RunInit(commands.InitOptions{HomerHome: filepath.Join(home, "homer")})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Adapters) != 4 {
		t.Fatalf("init adapters = %#v", report.Adapters)
	}
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return filepath.Join(home, "homer")
		}
		return ""
	})
	config, err := core.LoadConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pi", "herdr", "opencode", "vscode"} {
		if _, ok := config.Adapters[id]; !ok {
			t.Fatalf("config missing adapter %q", id)
		}
	}
	raw, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	value, err := orderedjson.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	root := value.(*orderedjson.Object)
	adapters := root.M["adapters"].(*orderedjson.Object)
	if got := strings.Join(adapters.Keys, ","); got != "pi,herdr,opencode,vscode" {
		t.Fatalf("adapter registration order = %q", got)
	}
}

func TestStatusCollectorMAMessageAndDirtyStoreWarning(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return ""
	})
	config := testCLIConfig()
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	base := mirrorSnapshot(map[string]string{"settings.json": "base\n"})
	if err := core.WriteSnapshotToStore(paths, base[0]); err != nil {
		t.Fatal(err)
	}
	if err := gitx.EnsureGitRepo(home); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"branch", "-M", "main"}, {"config", "user.email", "w8@example.invalid"}, {"config", "user.name", "W8"}} {
		if result := gitx.Exec(home, args, 0); !result.OK {
			t.Fatalf("git %v: %s", args, result.Stderr)
		}
	}
	if commit := gitx.CommitAllStore(home, "initial store"); commit == "" {
		t.Fatal("initial store commit missing")
	}

	// The configured adapter root is intentionally absent.  The collector must
	// retain base for local comparison and surface the M-A error instead of
	// reporting a bogus push-delete.
	sources, err := commands.CollectSnapshotSources(paths, &config)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources.ScanErrors) != 1 || !sources.ScanErrors[0].RootUnreadable {
		t.Fatalf("scan errors = %#v", sources.ScanErrors)
	}
	report, err := commands.RunStatus(commands.StatusOptions{HomerHome: home}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) == 0 || report.Adapters[0].Push != 0 {
		t.Fatalf("M-A report = %#v", report)
	}

	storeFile := filepath.Join(home, "store", "pi", "settings", "settings.json")
	if err := os.WriteFile(storeFile, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	warningSources, err := commands.CollectSnapshotSources(paths, &config)
	if err != nil {
		t.Fatal(err)
	}
	if len(warningSources.Warnings) == 0 || !strings.Contains(warningSources.Warnings[0], "store 工作区") {
		t.Fatalf("dirty store warnings = %#v", warningSources.Warnings)
	}
}

func TestStatusCollectorInjectsGitUpstreamRemote(t *testing.T) {
	parent := t.TempDir()
	origin := filepath.Join(parent, "origin.git")
	if result := gitx.Exec(parent, []string{"init", "--bare", "-b", "main", origin}, 0); !result.OK {
		t.Fatalf("init bare: %s", result.Stderr)
	}
	localHome := filepath.Join(parent, "local")
	if err := os.MkdirAll(localHome, 0o755); err != nil {
		t.Fatal(err)
	}
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return localHome
		}
		return ""
	})
	config := testCLIConfig()
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	base := mirrorSnapshot(map[string]string{"settings.json": "base\n"})
	if err := core.WriteSnapshotToStore(paths, base[0]); err != nil {
		t.Fatal(err)
	}
	if err := gitx.EnsureGitRepo(localHome); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"branch", "-M", "main"}, {"config", "user.email", "w8@example.invalid"}, {"config", "user.name", "W8"}} {
		if result := gitx.Exec(localHome, args, 0); !result.OK {
			t.Fatalf("git %v: %s", args, result.Stderr)
		}
	}
	if result := gitx.Exec(localHome, []string{"remote", "add", "origin", origin}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if commit := gitx.CommitAllStore(localHome, "initial store"); commit == "" {
		t.Fatal("initial store commit missing")
	}
	if result := gitx.Exec(localHome, []string{"push", "-u", "origin", "main"}, 0); !result.OK {
		t.Fatalf("initial push: %s", result.Stderr)
	}

	remoteHome := filepath.Join(parent, "remote")
	if result := gitx.Exec(parent, []string{"clone", "--", origin, remoteHome}, 0); !result.OK {
		t.Fatalf("clone: %s", result.Stderr)
	}
	for _, args := range [][]string{{"config", "user.email", "w8@example.invalid"}, {"config", "user.name", "W8"}} {
		if result := gitx.Exec(remoteHome, args, 0); !result.OK {
			t.Fatalf("remote git %v: %s", args, result.Stderr)
		}
	}
	remoteFile := filepath.Join(remoteHome, "store", "pi", "settings", "settings.json")
	if err := os.WriteFile(remoteFile, []byte("remote\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if commit := gitx.CommitAllStore(remoteHome, "remote change"); commit == "" {
		t.Fatal("remote store commit missing")
	}
	if result := gitx.Exec(remoteHome, []string{"push"}, 0); !result.OK {
		t.Fatalf("remote push: %s", result.Stderr)
	}

	sources, err := commands.CollectSnapshotSources(paths, &config)
	if err != nil {
		t.Fatal(err)
	}
	if sources.Remote == nil {
		t.Fatal("upstream remote snapshot was not injected")
	}
	report, err := commands.RunStatus(commands.StatusOptions{HomerHome: localHome}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Adapters) != 1 || report.Adapters[0].Pull != 1 {
		t.Fatalf("upstream pull report = %#v", report)
	}
}

func TestCommandStatusAndDiffInjectedSnapshots(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return ""
	})
	if err := core.SaveConfig(paths, testCLIConfig()); err != nil {
		t.Fatal(err)
	}

	base := mirrorSnapshot(map[string]string{"a.md": "base\n", "b.md": "gone\n"})
	local := mirrorSnapshot(map[string]string{"a.md": "mine\n", "c.md": "new\n"})
	remote := mirrorSnapshot(map[string]string{"a.md": "base\n", "b.md": "gone\n", "d.md": "remote\n"})
	sources := commands.DriftSources{Base: base, Local: local, Remote: remote}
	report, err := commands.RunStatus(commands.StatusOptions{HomerHome: home}, sources)
	if err != nil {
		t.Fatal(err)
	}
	adapterReport := report.Adapters[0]
	if adapterReport.Push != 3 || adapterReport.Pull != 1 {
		t.Fatalf("status counts = push %d pull %d, want 3/1", adapterReport.Push, adapterReport.Pull)
	}

	text, err := commands.RunDiff(commands.DiffOptions{HomerHome: home}, sources)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pi/settings", "-base", "+mine", "+new", "+remote"} {
		if !strings.Contains(text, want) {
			t.Fatalf("diff missing %q:\n%s", want, text)
		}
	}

	mergeBase := mergeSnapshot(`{"theme":"light","models":{"x":"a"}}`)
	mergeLocal := mergeSnapshot(`{"theme":"dark","models":{"x":"b"}}`)
	mergeSources := commands.DriftSources{Base: mergeBase, Local: mergeLocal, Remote: mergeBase}
	mergeText, err := commands.RunDiff(commands.DiffOptions{HomerHome: home}, mergeSources)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mergeText, "theme: light → dark") || !strings.Contains(mergeText, "models.x: a → b") {
		t.Fatalf("merge diff = %q", mergeText)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRunStripsArbitraryExecutableName guards the real-world dispatch path:
// installed binaries may be renamed (homer_linux_amd64, homer-bin, ...), so
// argv[0] must be stripped by "not a known command", never by matching a
// fixed binary name. (Regression for the v1 smoke bug.)
func TestRunStripsArbitraryExecutableName(t *testing.T) {
	// "init" as argv[0] with a real command after it must still dispatch.
	code := Run([]string{"/usr/local/bin/homer_linux_amd64", "--help"})
	if code != 0 {
		t.Fatalf("help via renamed binary: exit=%d", code)
	}
	// A fully stripped argv still works (package tests use this form).
	if code := Run([]string{"--help"}); code != 0 {
		t.Fatalf("help via stripped argv: exit=%d", code)
	}
}
