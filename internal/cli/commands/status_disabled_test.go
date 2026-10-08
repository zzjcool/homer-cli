package commands_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/cli"
	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
)

func statusBool(value bool) *bool { return &value }

func TestStatusDisabledSummariesMixedAdapterAndCategory(t *testing.T) {
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"pi": {
				Root: "~/.pi/agent",
				Categories: map[string]core.CategoryConfig{
					"settings":   {Paths: []string{"settings.json"}, Mode: core.SyncModeMerge},
					"extensions": {Paths: []string{"extensions/"}, Mode: core.SyncModeMirror, Enabled: statusBool(false)},
					"agents":     {Paths: []string{"agents/"}, Mode: core.SyncModeMirror, Enabled: statusBool(false)},
				},
			},
			"herdr": {
				Root:    "~/.config/herdr",
				Enabled: statusBool(false),
				Categories: map[string]core.CategoryConfig{
					"config": {Paths: []string{"config.toml"}, Mode: core.SyncModeMirror, Enabled: statusBool(false)},
				},
			},
			"vscode": {
				Root: "~/.config/Code",
				Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMerge},
					"themes":   {Paths: []string{"themes/"}, Mode: core.SyncModeMirror, Enabled: statusBool(false)},
				},
			},
		},
	}

	got := commands.DisabledSummaries(config)
	want := []string{"herdr", "pi/extensions", "pi/agents", "vscode/themes"}
	if len(got) != len(want) {
		t.Fatalf("DisabledSummaries length = %d, got %#v, want %#v", len(got), got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("DisabledSummaries[%d] = %q, got %#v, want %#v", index, got[index], got, want)
		}
	}
}

func TestStatusCarriesMissingManifestCLIWarning(t *testing.T) {
	homerHome := t.TempDir()
	toolRoot := filepath.Join(homerHome, "tool")
	if err := os.MkdirAll(toolRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	kind := core.CategoryKindManifest
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return homerHome
		}
		return os.Getenv(key)
	})
	config := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{
		"vscode": {Root: toolRoot, Categories: map[string]core.CategoryConfig{
			"extensions": {Kind: &kind, Mode: core.SyncModeMirror, ListCmd: "definitely-not-installed-code --list", ApplyCmd: "code --install-extension"},
		}},
	}}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	report, err := commands.RunStatus(commands.StatusOptions{HomerHome: homerHome})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(report.Warnings, "\\n")
	if !strings.Contains(joined, "vscode: definitely-not-installed-code 未安装") || !strings.Contains(joined, "pull 时远端清单将视为全量待装") {
		t.Fatalf("status missing CLI warning = %#v", report.Warnings)
	}
}

func TestStatusRunCarriesDisabledSummaries(t *testing.T) {
	homerHome := t.TempDir()
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return homerHome
		}
		return os.Getenv(key)
	})
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"pi": {
				Root: "~/.pi/agent",
				Categories: map[string]core.CategoryConfig{
					"extensions": {Paths: []string{"extensions/"}, Mode: core.SyncModeMirror, Enabled: statusBool(false)},
				},
			},
		},
	}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}

	report, err := commands.RunStatus(commands.StatusOptions{HomerHome: homerHome}, commands.DriftSources{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Disabled) != 1 || report.Disabled[0] != "pi/extensions" {
		t.Fatalf("RunStatus.Disabled = %#v, want [pi/extensions]", report.Disabled)
	}
}

func TestStatusRenderDisabledFoldAndVerbose(t *testing.T) {
	report := cli.StatusReport{
		Adapters: []cli.StatusAdapterReport{{ID: "pi", Categories: []cli.StatusCategoryReport{{Name: "settings"}}}},
		Errors:   []string{},
		Disabled: []string{"pi/extensions", "pi/agents", "herdr"},
	}

	collapsed := cli.RenderStatus(report)
	if !strings.Contains(collapsed, "pi: 2 类未启用（extensions, agents）") {
		t.Fatalf("collapsed status = %q, want grouped pi summary", collapsed)
	}
	if !strings.Contains(collapsed, "herdr: adapter 未启用") {
		t.Fatalf("collapsed status = %q, want disabled adapter summary", collapsed)
	}
	if strings.Contains(collapsed, "pi/extensions") || strings.Contains(collapsed, "pi/agents") {
		t.Fatalf("collapsed status leaked category details: %q", collapsed)
	}

	verbose := cli.RenderStatus(report, cli.RenderStatusOptions{Verbose: true})
	for _, item := range report.Disabled {
		if !strings.Contains(verbose, "  "+item) {
			t.Fatalf("verbose status = %q, missing %q", verbose, item)
		}
	}
	if strings.Contains(verbose, "类未启用") || strings.Contains(verbose, "adapter 未启用") {
		t.Fatalf("verbose status contains collapsed summaries: %q", verbose)
	}
}

func TestStatusRenderJSONDisabledIsAdditiveAndOmittable(t *testing.T) {
	withoutDisabled := cli.RenderStatusJSON(cli.StatusReport{
		Adapters: []cli.StatusAdapterReport{{ID: "pi", Categories: []cli.StatusCategoryReport{}}},
		Errors:   []string{},
	})
	var without map[string]json.RawMessage
	if err := json.Unmarshal([]byte(withoutDisabled), &without); err != nil {
		t.Fatalf("status JSON without disabled: %v\n%s", err, withoutDisabled)
	}
	if _, ok := without["disabled"]; ok {
		t.Fatalf("status JSON without disabled unexpectedly has field: %s", withoutDisabled)
	}
	for _, key := range []string{"errors", "adapters"} {
		if _, ok := without[key]; !ok {
			t.Fatalf("status JSON without disabled missing existing field %q: %s", key, withoutDisabled)
		}
	}

	withDisabled := cli.RenderStatusJSON(cli.StatusReport{
		Adapters: []cli.StatusAdapterReport{{ID: "pi", Categories: []cli.StatusCategoryReport{}}},
		Errors:   []string{},
		Disabled: []string{"pi/extensions", "herdr"},
	})
	var decoded struct {
		Disabled []string `json:"disabled"`
	}
	if err := json.Unmarshal([]byte(withDisabled), &decoded); err != nil {
		t.Fatalf("status JSON with disabled: %v\n%s", err, withDisabled)
	}
	want := []string{"pi/extensions", "herdr"}
	if len(decoded.Disabled) != len(want) {
		t.Fatalf("JSON disabled = %#v, want %#v", decoded.Disabled, want)
	}
	for index := range want {
		if decoded.Disabled[index] != want[index] {
			t.Fatalf("JSON disabled[%d] = %q, want %q", index, decoded.Disabled[index], want[index])
		}
	}
}

func TestREADMECustomAdapterJSONValidates(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	readmePath := filepath.Join(filepath.Dir(filename), "..", "..", "..", "README.md")
	readme, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read README: %v", err)
	}

	const heading = "### 自定义 adapter（v1.2 起）"
	sectionStart := strings.Index(string(readme), heading)
	if sectionStart < 0 {
		t.Fatalf("README missing %q", heading)
	}
	section := string(readme)[sectionStart:]
	fenceStart := strings.Index(section, "```json\n")
	if fenceStart < 0 {
		t.Fatal("README custom adapter section missing JSON fence")
	}
	jsonStart := fenceStart + len("```json\n")
	fenceEnd := strings.Index(section[jsonStart:], "\n```")
	if fenceEnd < 0 {
		t.Fatal("README custom adapter JSON fence is not closed")
	}
	raw := []byte(section[jsonStart : jsonStart+fenceEnd])

	config, validationErrors := core.ValidateConfig(raw)
	if config == nil || len(validationErrors) != 0 {
		t.Fatalf("README custom adapter JSON does not validate: config=%#v errors=%v\n%s", config, validationErrors, raw)
	}
	adapter, ok := config.Adapters["my-tool"]
	if !ok {
		t.Fatal("README custom adapter JSON missing my-tool")
	}
	if adapter.Root != "~/.config/my-tool" || len(adapter.Ignore) == 0 || len(adapter.AllowEscape) == 0 {
		t.Fatalf("README custom adapter fields = %#v, want root/ignore/allowEscape", adapter)
	}
	manifest, ok := adapter.Categories["plugins"]
	if !ok || !manifest.IsManifest() || manifest.ListCmd == "" || manifest.ApplyCmd == "" {
		t.Fatalf("README manifest category = %#v, want valid manifest declaration", manifest)
	}
}
