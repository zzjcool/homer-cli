package pi

import "github.com/zzjcool/homer-cli/internal/core"

// PIAdapterID is the stable adapter identifier written to snapshots and
// homer.json.
const PIAdapterID = "pi"

// PI_ADAPTER_ID is kept as a source-compatible spelling for callers ported
// directly from the archived TypeScript adapter.
const PI_ADAPTER_ID = PIAdapterID

func boolPtr(value bool) *bool { return &value }

func kindPtr(kind core.CategoryKind) *core.CategoryKind { return &kind }

// DefaultPIAdapter is the frozen pi configuration from the legacy TypeScript
// source (the sole source of truth for this adapter's categories and ignore
// list).
var DefaultPIAdapter = core.AdapterConfig{
	Root:    "~/.pi/agent",
	Enabled: boolPtr(true),
	Categories: map[string]core.CategoryConfig{
		"settings": {
			Paths: []string{"settings.json", "keybindings.json"},
			Mode:  core.SyncMode("merge"),
		},
		"skills": {
			Paths: []string{"skills/"},
			Mode:  core.SyncMode("mirror"),
		},
		"agents": {
			Paths: []string{"agents/"},
			Mode:  core.SyncMode("mirror"),
		},
		// packages is THE flagship category: plugin sync by name and
		// installed version (listCmd inventories what is installed;
		// applyCmd installs that exact build on pull). A fresh machine's
		// plugins must be visible (and syncable) without any configuration.
		"packages": {
			Mode:      core.SyncMode("mirror"),
			Kind:      kindPtr(core.CategoryKindManifest),
			ListCmd:   "pi list",
			ApplyCmd:  "pi install",
			IDPattern: "^  (npm:[A-Za-z0-9@/._-]+)$",
		},
		"models": {
			Paths:       []string{"models.json"},
			Mode:        core.SyncMode("merge"),
			ExcludeKeys: []string{"apiKeys"},
		},
		"prompts": {
			Paths: []string{"prompts/"},
			Mode:  core.SyncMode("mirror"),
		},
		"themes": {
			Paths: []string{"themes/"},
			Mode:  core.SyncMode("mirror"),
		},
	},
	Ignore: []string{
		"auth.json",
		"trust.json",
		"sessions/",
		"npm/",
		"git/",
		"tmp/",
		"bin/",
		// macOS AppleDouble metadata noise (._foo next to foo) — never
		// configuration, never wanted in a synced tree. "**/" = any depth.
		"**/._*",
		"*.bak",
		"*.bak-*",
		"*.bak*",
		"*.log",
		"run-history.jsonl",
	},
}
