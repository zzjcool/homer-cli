package adapter_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/adapter/herdr"
	"github.com/zzjcool/homer-cli/internal/adapter/opencode"
	"github.com/zzjcool/homer-cli/internal/adapter/pi"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/manifest"
)

func writeFixture(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func withHome(t *testing.T, home string) {
	t.Helper()
	old, existed := os.LookupEnv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv("HOME", old)
		} else {
			_ = os.Unsetenv("HOME")
		}
	})
}

func category(t *testing.T, snapshot core.AdapterSnapshot, name string) core.CategorySnapshot {
	t.Helper()
	for _, cat := range snapshot.Categories {
		if cat.Category == name {
			return cat
		}
	}
	t.Fatalf("category %q not found in %#v", name, snapshot.Categories)
	return core.CategorySnapshot{}
}

func snapshotKeys(cat core.CategorySnapshot) []string {
	keys := make([]string, 0, len(cat.Files))
	for key := range cat.Files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func snapshotText(snapshot core.AdapterSnapshot) string {
	var parts []string
	for _, cat := range snapshot.Categories {
		for _, entry := range cat.Files {
			parts = append(parts, entry.Content)
		}
	}
	return strings.Join(parts, "\n")
}

func TestFrozenDefaultAdapters(t *testing.T) {
	if pi.PIAdapterID != "pi" || pi.PI_ADAPTER_ID != "pi" {
		t.Fatalf("pi adapter id mismatch: %q %q", pi.PIAdapterID, pi.PI_ADAPTER_ID)
	}
	if herdr.HerdrAdapterID != "herdr" || opencode.OpencodeAdapterID != "opencode" {
		t.Fatal("built-in adapter ids changed")
	}

	trueValue := true
	wantPI := core.AdapterConfig{
		Root:    "~/.pi/agent",
		Enabled: &trueValue,
		Categories: map[string]core.CategoryConfig{
			"settings":   {Paths: []string{"settings.json", "keybindings.json"}, Mode: core.SyncMode("merge")},
			"skills":     {Paths: []string{"skills/"}, Mode: core.SyncMode("mirror")},
			"extensions": {Paths: []string{"extensions/"}, Mode: core.SyncMode("mirror"), Exclude: []string{"*cache*"}},
			"agents":     {Paths: []string{"agents/"}, Mode: core.SyncMode("mirror")},
			"models":     {Paths: []string{"models.json"}, Mode: core.SyncMode("merge"), ExcludeKeys: []string{"apiKeys"}},
			"prompts":    {Paths: []string{"prompts/"}, Mode: core.SyncMode("mirror")},
			"themes":     {Paths: []string{"themes/"}, Mode: core.SyncMode("mirror")},
		},
		Ignore: []string{"auth.json", "trust.json", "sessions/", "npm/", "git/", "tmp/", "bin/", "*.bak", "*.bak-*", "*.bak*", "*.log", "run-history.jsonl"},
	}
	if !reflect.DeepEqual(pi.DefaultPIAdapter, wantPI) {
		t.Fatalf("pi defaults changed:\n got %#v\nwant %#v", pi.DefaultPIAdapter, wantPI)
	}

	wantHerdr := core.AdapterConfig{
		Root:    "~/.config/herdr",
		Enabled: &trueValue,
		Categories: map[string]core.CategoryConfig{
			"config": {Paths: []string{"config.toml"}, Mode: core.SyncMode("mirror")},
		},
		Ignore: []string{"session.json", "*.sock", "*.log", ".plugins.lock", "release-notes.json"},
	}
	if !reflect.DeepEqual(herdr.DefaultHerdrAdapter, wantHerdr) {
		t.Fatalf("herdr defaults changed:\n got %#v\nwant %#v", herdr.DefaultHerdrAdapter, wantHerdr)
	}

	wantOpen := core.AdapterConfig{
		Root:    "~/.config/opencode",
		Enabled: &trueValue,
		Categories: map[string]core.CategoryConfig{
			"config":  {Paths: []string{"opencode.json"}, Mode: core.SyncMode("merge")},
			"plugins": {Paths: []string{"package.json"}, Mode: core.SyncMode("merge")},
			"locks":   {Paths: []string{"package-lock.json", "bun.lock"}, Mode: core.SyncMode("mirror")},
		},
		Ignore: []string{"node_modules/", ".plugins.lock", "*.log", ".gitignore"},
	}
	if !reflect.DeepEqual(opencode.DefaultOpencodeAdapter, wantOpen) {
		t.Fatalf("opencode defaults changed:\n got %#v\nwant %#v", opencode.DefaultOpencodeAdapter, wantOpen)
	}
}

func TestPIExactSnapshotAndKinds(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)

	writeFixture(t, home, ".pi/agent/settings.json", "{\"theme\":\"dark\"}")
	writeFixture(t, home, ".pi/agent/keybindings.json", `{ "broken": `)
	writeFixture(t, home, ".pi/agent/skills/foo/SKILL.md", "# foo\n")
	writeFixture(t, home, ".pi/agent/skills/foo/reference/notes.md", "notes\n")
	writeFixture(t, home, ".pi/agent/skills/bar/SKILL.md", "# bar\n")
	writeFixture(t, home, ".pi/agent/extensions/tool/index.js", "export const x = 1;\n")
	writeFixture(t, home, ".pi/agent/extensions/pi-foo-cache/index.js", "MUST BE EXCLUDED")
	writeFixture(t, home, ".pi/agent/extensions/mycache.json", "MUST BE EXCLUDED")
	writeFixture(t, home, ".pi/agent/agents/reviewer.md", "# reviewer\n")
	writeFixture(t, home, ".pi/agent/models.json", `{"provider":"openai","apiKeys":{"openai":"sk-x"}}`)
	writeFixture(t, home, ".pi/agent/prompts/system.md", "# system\n")
	writeFixture(t, home, ".pi/agent/themes/dark.json", `{"bg":"#000"}`)

	// Runtime/junk files are physically present but must not be collected.
	writeFixture(t, home, ".pi/agent/sessions/x", "session data")
	writeFixture(t, home, ".pi/agent/npm/y", "npm data")
	writeFixture(t, home, ".pi/agent/auth.json", `{"token":"secret"}`)
	writeFixture(t, home, ".pi/agent/models.json.bak-predirect", "backup")
	writeFixture(t, home, ".pi/agent/pi-tui-crash.log", "stack trace")

	outcome := pi.ScanAdapter(pi.PIAdapterID, pi.DefaultPIAdapter)
	if len(outcome.Errors) != 0 {
		t.Fatalf("scan errors: %#v", outcome.Errors)
	}
	if got := outcome.Snapshot.AdapterID; got != "pi" {
		t.Fatalf("adapter id = %q", got)
	}
	wantCategories := []string{"settings", "skills", "extensions", "agents", "models", "prompts", "themes"}
	gotCategories := make([]string, 0, len(outcome.Snapshot.Categories))
	for _, cat := range outcome.Snapshot.Categories {
		gotCategories = append(gotCategories, cat.Category)
	}
	if !reflect.DeepEqual(gotCategories, wantCategories) {
		t.Fatalf("category order = %#v, want %#v", gotCategories, wantCategories)
	}

	if got := category(t, outcome.Snapshot, "settings").Files; !reflect.DeepEqual(got, core.SnapshotFiles{
		"settings.json":    {Kind: "json", Content: "{\"theme\":\"dark\"}"},
		"keybindings.json": {Kind: "file", Content: `{ "broken": `},
	}) {
		t.Fatalf("settings snapshot = %#v", got)
	}
	if got := snapshotKeys(category(t, outcome.Snapshot, "skills")); !reflect.DeepEqual(got, []string{"bar/SKILL.md", "foo/SKILL.md", "foo/reference/notes.md"}) {
		t.Fatalf("skills keys = %#v", got)
	}
	if got := snapshotKeys(category(t, outcome.Snapshot, "extensions")); !reflect.DeepEqual(got, []string{"tool/index.js"}) {
		t.Fatalf("extensions keys = %#v", got)
	}
	if got := snapshotKeys(category(t, outcome.Snapshot, "agents")); !reflect.DeepEqual(got, []string{"reviewer.md"}) {
		t.Fatalf("agents keys = %#v", got)
	}
	if got := category(t, outcome.Snapshot, "models").Files["models.json"]; got.Kind != "json" {
		t.Fatalf("models kind = %#v", got)
	}
	if got := category(t, outcome.Snapshot, "themes").Files["dark.json"]; got.Kind != "file" {
		t.Fatalf("mirror JSON kind = %#v", got)
	}
	if strings.Contains(snapshotText(outcome.Snapshot), "MUST BE EXCLUDED") || strings.Contains(snapshotText(outcome.Snapshot), "session data") {
		t.Fatal("ignored/excluded data entered the snapshot")
	}
}

func TestBuiltInAdapterFixturesAndMissingRoots(t *testing.T) {
	t.Run("herdr", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		config := "onboarding = false\n[theme]\nname = \"gruvbox\"\n"
		writeFixture(t, home, ".config/herdr/config.toml", config)
		writeFixture(t, home, ".config/herdr/session.json", "MUST NOT APPEAR")
		writeFixture(t, home, ".config/herdr/herdr.sock", "MUST NOT APPEAR")
		writeFixture(t, home, ".config/herdr/herdr.log", "MUST NOT APPEAR")
		writeFixture(t, home, ".config/herdr/.plugins.lock", "MUST NOT APPEAR")
		writeFixture(t, home, ".config/herdr/release-notes.json", "MUST NOT APPEAR")

		outcome := herdr.ScanAdapter(herdr.HerdrAdapterID, herdr.DefaultHerdrAdapter)
		if len(outcome.Errors) != 0 {
			t.Fatalf("scan errors: %#v", outcome.Errors)
		}
		got := category(t, outcome.Snapshot, "config")
		want := core.SnapshotFiles{"config.toml": {Kind: "file", Content: config}}
		if !reflect.DeepEqual(got.Files, want) {
			t.Fatalf("herdr snapshot = %#v, want %#v", got.Files, want)
		}
	})

	t.Run("opencode", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		opencodeJSON := `{"provider":{"ccrb":{"options":{"apiKey":"ccrb"}}}}`
		packageJSON := `{"dependencies":{"@opencode-ai/plugin":"1.17.12"}}`
		lockJSON := `{"lockfileVersion":3}`
		writeFixture(t, home, ".config/opencode/opencode.json", opencodeJSON)
		writeFixture(t, home, ".config/opencode/package.json", packageJSON)
		writeFixture(t, home, ".config/opencode/package-lock.json", lockJSON)
		writeFixture(t, home, ".config/opencode/bun.lock", "// bun lockfile\n")
		writeFixture(t, home, ".config/opencode/node_modules/plugin/package.json", "MUST NOT APPEAR")
		writeFixture(t, home, ".config/opencode/.plugins.lock", "MUST NOT APPEAR")
		writeFixture(t, home, ".config/opencode/tool.log", "MUST NOT APPEAR")
		writeFixture(t, home, ".config/opencode/.gitignore", "node_modules\n")

		outcome := opencode.ScanAdapter(opencode.OpencodeAdapterID, opencode.DefaultOpencodeAdapter)
		if len(outcome.Errors) != 0 {
			t.Fatalf("scan errors: %#v", outcome.Errors)
		}
		if category(t, outcome.Snapshot, "config").Files["opencode.json"].Kind != "json" || category(t, outcome.Snapshot, "plugins").Files["package.json"].Kind != "json" {
			t.Fatal("merge categories did not preserve JSON kind")
		}
		if category(t, outcome.Snapshot, "locks").Files["package-lock.json"].Kind != "file" {
			t.Fatal("lock category unexpectedly got JSON kind")
		}
		if got := snapshotKeys(category(t, outcome.Snapshot, "locks")); !reflect.DeepEqual(got, []string{"bun.lock", "package-lock.json"}) {
			t.Fatalf("lock keys = %#v", got)
		}
		if strings.Contains(snapshotText(outcome.Snapshot), "MUST NOT APPEAR") {
			t.Fatal("opencode ignored data entered the snapshot")
		}
	})

	t.Run("missing roots", func(t *testing.T) {
		cases := []struct {
			name string
			id   string
			cfg  core.AdapterConfig
		}{
			{name: "pi", id: pi.PIAdapterID, cfg: pi.DefaultPIAdapter},
			{name: "herdr", id: herdr.HerdrAdapterID, cfg: herdr.DefaultHerdrAdapter},
			{name: "opencode", id: opencode.OpencodeAdapterID, cfg: opencode.DefaultOpencodeAdapter},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				home := t.TempDir()
				withHome(t, home)
				var outcome adapter.ScanOutcome
				switch test.name {
				case "pi":
					outcome = pi.ScanAdapter(test.id, test.cfg)
				case "herdr":
					outcome = herdr.ScanAdapter(test.id, test.cfg)
				case "opencode":
					outcome = opencode.ScanAdapter(test.id, test.cfg)
				}
				if len(outcome.Snapshot.Categories) != 0 || len(outcome.Errors) != 1 {
					t.Fatalf("missing-root outcome = %#v", outcome)
				}
			})
		}
	})
}

func TestSymlinkEscapeAllowlistLoopAndDangling(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	outside2 := t.TempDir()
	withHome(t, home)

	writeFixture(t, home, ".pi/agent/skills/foo/SKILL.md", "# foo\n")
	writeFixture(t, outside, "browser/SKILL.md", "# browser\n")
	writeFixture(t, outside, "browser/sub/a.ts", "export const a = 1;\n")
	if err := os.Symlink(filepath.Join(outside, "browser"), filepath.Join(home, ".pi/agent/skills/agent-browser")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(home, ".pi/agent/skills"), filepath.Join(home, ".pi/agent/skills/loop")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(home, ".pi/agent/skills/missing"), filepath.Join(home, ".pi/agent/skills/dangling")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	denied := pi.ScanAdapter(pi.PIAdapterID, pi.DefaultPIAdapter)
	if got := snapshotKeys(category(t, denied.Snapshot, "skills")); !reflect.DeepEqual(got, []string{"foo/SKILL.md"}) {
		t.Fatalf("denied escape keys = %#v", got)
	}
	if len(denied.Errors) != 1 || !strings.Contains(denied.Errors[0].Message, "逃逸") {
		t.Fatalf("denied escape errors = %#v", denied.Errors)
	}

	// A bare allowlist wildcard is unsafe even when a caller bypasses config
	// validation; scan-side matching fails closed for the guard spellings.
	bareCfg := pi.DefaultPIAdapter
	bareCfg.AllowEscape = []string{"*"}
	bare := pi.ScanAdapter(pi.PIAdapterID, bareCfg)
	if len(bare.Errors) != 1 || strings.Contains(snapshotText(bare.Snapshot), "# browser") {
		t.Fatalf("bare allowEscape did not fail closed: %#v", bare)
	}

	allowedCfg := pi.DefaultPIAdapter
	allowedCfg.AllowEscape = []string{"skills/agent-browser"}
	allowed := pi.ScanAdapter(pi.PIAdapterID, allowedCfg)
	if got := snapshotKeys(category(t, allowed.Snapshot, "skills")); !reflect.DeepEqual(got, []string{"agent-browser/SKILL.md", "agent-browser/sub/a.ts", "foo/SKILL.md"}) {
		t.Fatalf("allowed escape keys = %#v", got)
	}
	if len(allowed.Errors) != 0 {
		t.Fatalf("allowed escape errors = %#v", allowed.Errors)
	}

	// A trailing slash allowlist is an explicit subtree opt-in.  The same
	// scanner must still enforce the ignore/exclude checks inside it.
	writeFixture(t, outside, "browser/private/secret.md", "SECRET\n")
	writeFixture(t, outside, "browser/cache/blob.bin", "CACHE\n")
	allowedCfg.AllowEscape = []string{"skills/agent-browser/"}
	allowedCfg.Categories = map[string]core.CategoryConfig{
		"skills": {Paths: []string{"skills/"}, Mode: core.SyncMode("mirror"), Exclude: []string{"*cache*"}},
	}
	allowedCfg.Ignore = []string{"agent-browser/private/"}
	filtered := pi.ScanAdapter(pi.PIAdapterID, allowedCfg)
	if got := snapshotKeys(category(t, filtered.Snapshot, "skills")); !reflect.DeepEqual(got, []string{"agent-browser/SKILL.md", "agent-browser/sub/a.ts", "foo/SKILL.md"}) {
		t.Fatalf("filtered allowed keys = %#v", got)
	}
	if len(filtered.Errors) != 0 {
		t.Fatalf("filtered allowed errors = %#v", filtered.Errors)
	}

	// The outer link may be allowed, but an independently escaping nested
	// link must be denied unless its own path is allowlisted.
	if err := os.Symlink(filepath.Join(outside2, "deep"), filepath.Join(outside, "browser", "nested")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeFixture(t, outside2, "deep/secret.md", "DEEP SECRET\n")
	allowedCfg.AllowEscape = []string{"skills/agent-browser"}
	nestedDenied := pi.ScanAdapter(pi.PIAdapterID, allowedCfg)
	if strings.Contains(snapshotText(nestedDenied.Snapshot), "DEEP SECRET") {
		t.Fatal("nested escape was followed without its own allowlist")
	}
	foundNestedError := false
	for _, err := range nestedDenied.Errors {
		if strings.Contains(err.Path, "nested") && strings.Contains(err.Message, "逃逸") {
			foundNestedError = true
		}
	}
	if !foundNestedError {
		t.Fatalf("nested escape error missing: %#v", nestedDenied.Errors)
	}
}

func TestDeclaredPathSymlinkAndDisabledAdapter(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	withHome(t, home)
	if err := os.MkdirAll(filepath.Join(home, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, outside, "settings.json", `{"outside":true}`)
	if err := os.Symlink(filepath.Join(outside, "settings.json"), filepath.Join(home, ".pi/agent/settings.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	denied := pi.ScanAdapter(pi.PIAdapterID, pi.DefaultPIAdapter)
	if len(category(t, denied.Snapshot, "settings").Files) != 0 || len(denied.Errors) != 1 {
		t.Fatalf("declared escape denied outcome = %#v", denied)
	}
	cfg := pi.DefaultPIAdapter
	cfg.AllowEscape = []string{"settings.json"}
	allowed := pi.ScanAdapter(pi.PIAdapterID, cfg)
	if got := allowed.Snapshot.Categories[0].Files["settings.json"].Content; got != `{"outside":true}` {
		t.Fatalf("declared escape content = %q", got)
	}
	if len(allowed.Errors) != 0 {
		t.Fatalf("declared escape allowed errors = %#v", allowed.Errors)
	}

	off := pi.DefaultPIAdapter
	falseValue := false
	off.Enabled = &falseValue
	off.Root = filepath.Join(home, "does-not-exist")
	outcome := pi.ScanAdapter(pi.PIAdapterID, off)
	if len(outcome.Snapshot.Categories) != 0 || len(outcome.Errors) != 0 {
		t.Fatalf("disabled adapter outcome = %#v", outcome)
	}
}

func TestResolveCategoryFilePathInverse(t *testing.T) {
	root := filepath.Join("/tmp", "fake-agent")
	cfg := core.CategoryConfig{Paths: []string{"skills/"}, Mode: core.SyncMode("mirror")}
	got, err := adapter.ResolveCategoryFilePath(root, cfg, "foo/SKILL.md")
	if err != nil || got != filepath.Join(root, "skills", "foo", "SKILL.md") {
		t.Fatalf("directory reverse mapping = %q, %v", got, err)
	}
	fileCfg := core.CategoryConfig{Paths: []string{"settings.json", "keybindings.json"}, Mode: core.SyncMode("merge")}
	got, err = adapter.ResolveCategoryFilePath(root, fileCfg, "keybindings.json")
	if err != nil || got != filepath.Join(root, "keybindings.json") {
		t.Fatalf("file reverse mapping = %q, %v", got, err)
	}
	for _, rel := range []string{"", "../escape", "a/../../etc/passwd", "/etc/passwd", "a/b/"} {
		if _, err := adapter.ResolveCategoryFilePath(root, cfg, rel); err == nil {
			t.Errorf("ResolveCategoryFilePath(%q) unexpectedly succeeded", rel)
		}
	}
}

type fakeManifestPort struct {
	output    []byte
	outputErr error
}

func (fake *fakeManifestPort) Output(string) ([]byte, error) {
	return fake.output, fake.outputErr
}

func (fake *fakeManifestPort) Apply(string, string) error { return nil }

func manifestKind() *core.CategoryKind {
	kind := core.CategoryKindManifest
	return &kind
}

func TestScanManifestWithInjectedPortAndLegacyCall(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "settings.json", "{}\n")
	port := &fakeManifestPort{output: []byte("pub.two\r\npub.one\npub.two\n")}
	config := core.AdapterConfig{
		Root: root,
		Categories: map[string]core.CategoryConfig{
			"settings":   {Paths: []string{"settings.json"}, Mode: core.SyncModeMerge},
			"extensions": {Kind: manifestKind(), Mode: core.SyncModeMirror, ListCmd: "fake list"},
		},
	}
	outcome := adapter.ScanAdapter("fake", config, adapter.ScanDeps{Commands: port})
	if len(outcome.Errors) != 0 {
		t.Fatalf("manifest scan errors = %#v", outcome.Errors)
	}
	if got := category(t, outcome.Snapshot, "extensions"); got.AdapterID != "fake" || got.Mode != core.SyncModeMirror {
		t.Fatalf("manifest category metadata = %#v", got)
	}
	if got := category(t, outcome.Snapshot, "extensions").Files[manifest.VirtualFileName("extensions")]; got.Kind != "file" || got.Content != "pub.two\npub.one\n" {
		t.Fatalf("manifest file = %#v", got)
	}
	if got := category(t, outcome.Snapshot, "settings").Files["settings.json"].Content; got != "{}\n" {
		t.Fatalf("regular category was not scanned = %q", got)
	}

	// A two-argument call remains source-compatible and does not require a
	// command dependency when the config has only ordinary categories.
	legacy := adapter.ScanAdapter("fake", core.AdapterConfig{
		Root: root,
		Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMerge},
		},
	})
	if len(legacy.Errors) != 0 || len(legacy.Snapshot.Categories) != 1 {
		t.Fatalf("legacy two-argument scan = %#v", legacy)
	}
}

func TestScanManifestFailureDoesNotBlockOtherCategories(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "settings.json", "settings\n")
	port := &fakeManifestPort{outputErr: errors.New("context deadline exceeded")}
	kind := core.CategoryKindManifest
	outcome := adapter.ScanAdapter("fake", core.AdapterConfig{
		Root: root,
		Categories: map[string]core.CategoryConfig{
			"settings":   {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
			"extensions": {Kind: &kind, Mode: core.SyncModeMirror, ListCmd: "fake list"},
		},
	}, adapter.ScanDeps{Commands: port})
	if len(outcome.Errors) != 1 || outcome.Errors[0].Path != "fake list" || outcome.Errors[0].Message != "context deadline exceeded" {
		t.Fatalf("manifest failure errors = %#v", outcome.Errors)
	}
	if got := category(t, outcome.Snapshot, "extensions"); len(got.Files) != 0 {
		t.Fatalf("failed manifest files = %#v", got.Files)
	}
	if got := category(t, outcome.Snapshot, "settings").Files["settings.json"].Content; got != "settings\n" {
		t.Fatalf("other category was blocked = %q", got)
	}
}

// TestScanManifestMissingCLIDegradesQuietly locks the product semantics added
// after the v1.2 e2e ripple: a manifest listCmd failing because the tool CLI
// is simply not installed (fresh machine, no `code` in PATH) must NOT surface
// as a scan error — it degrades to an empty manifest so status stays clean.
func TestScanManifestMissingCLIDegradesQuietly(t *testing.T) {
	outcome := adapter.ScanAdapter("vscode", core.AdapterConfig{
		Root: t.TempDir(),
		Categories: map[string]core.CategoryConfig{
			"extensions": {Kind: manifestKind(), Mode: core.SyncModeMirror, ListCmd: "definitely-not-a-real-cli-xyz --list"},
		},
	})
	if len(outcome.Errors) != 0 {
		t.Fatalf("missing CLI must degrade quietly, got errors: %+v", outcome.Errors)
	}
	if got, want := outcome.Warnings, []string{"vscode: definitely-not-a-real-cli-xyz 未安装，extensions 已按空清单处理；pull 时远端清单将视为全量待装"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("missing CLI warning = %#v, want %#v", got, want)
	}
	var found bool
	for _, cat := range outcome.Snapshot.Categories {
		if cat.Category == "extensions" {
			found = true
			if len(cat.Files) != 0 {
				t.Fatalf("expected empty manifest files, got %+v", cat.Files)
			}
		}
	}
	if !found {
		t.Fatal("extensions category missing from snapshot")
	}
}

func TestScanManifestExitErrorIsNotMissingCLI(t *testing.T) {
	root := t.TempDir()
	port := &fakeManifestPort{outputErr: errors.New("executable file not found")}
	outcome := adapter.ScanAdapter("vscode", core.AdapterConfig{
		Root: root,
		Categories: map[string]core.CategoryConfig{
			"extensions": {Kind: manifestKind(), Mode: core.SyncModeMirror, ListCmd: "code --list-extensions"},
		},
	}, adapter.ScanDeps{Commands: port})
	if len(outcome.Errors) != 1 || outcome.Errors[0].Message != "executable file not found" {
		t.Fatalf("exit-like scan error = %#v", outcome.Errors)
	}
	if len(outcome.Warnings) != 0 {
		t.Fatalf("exit-like error was misclassified as missing CLI: %#v", outcome.Warnings)
	}
}
