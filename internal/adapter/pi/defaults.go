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

// SecretFiles are the agent-directory files Pi's configuration spec uses
// to store credentials. They are not collected as plaintext. The console
// offers them as encrypt-by-default.
var SecretFiles = []string{"auth.json", "mcp-auth.json"}

// SecretDestination is the tool path for one SecretFiles entry.
func SecretDestination(name string) string {
	return "~/.pi/agent/" + name
}

// DefaultPIAdapter is the frozen pi configuration from the legacy TypeScript
// source (the sole source of truth for this adapter's categories and ignore
// list).
var DefaultPIAdapter = core.AdapterConfig{
	Root:    "~/.pi/agent",
	Enabled: boolPtr(true),
	Categories: map[string]core.CategoryConfig{
		"settings": {
			Paths: []string{"settings.json", "keybindings.json", "mcp.json"},
			Mode:  core.SyncMode("merge"),
		},
		// User-level instructions. Pi reads whichever of these names exist;
		// a missing file is not an error. OAuth material stays out: auth.json
		// and mcp-auth.json are ignored below.
		"context": {
			Paths: []string{
				"SYSTEM.md",
				"APPEND_SYSTEM.md",
				"AGENTS.override.md",
				"AGENTS.md",
				"AGENTS.MD",
				"CLAUDE.md",
				"CLAUDE.MD",
			},
			Mode: core.SyncMode("mirror"),
		},
		"skills": {
			Paths: []string{"skills/"},
			Mode:  core.SyncMode("mirror"),
		},
		// Local extension source. pi install packages stay in the packages
		// category; this tree is the TypeScript the user wrote under
		// extensions/. Cache directories and installed npm deps are not
		// configuration.
		"extensions": {
			Paths:   []string{"extensions/"},
			Mode:    core.SyncMode("mirror"),
			Exclude: []string{"*cache*", "node_modules/"},
		},
		"agents": {
			Paths: []string{"agents/"},
			Mode:  core.SyncMode("mirror"),
		},
		// packages is THE flagship category: plugin sync by name and
		// installed version (listCmd inventories what is installed;
		// applyCmd installs that exact build on pull). A fresh machine's
		// plugins must be visible (and syncable) without any configuration.
		// The "pi" program name is checked before a dispatch. The copy-paste
		// installer for a missing binary lives in adapter.OfficialInstall.
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
		// Everything else under the agent directory. Named categories above
		// own their paths; this catch-all is how a new file still reaches
		// the center. Secret stores stay on the ignore list and are
		// encrypted through the keyring instead of this plaintext tree.
		"files": {
			Paths: []string{"./"},
			Mode:  core.SyncMode("mirror"),
			Exclude: []string{
				"settings.json", "keybindings.json", "mcp.json", "models.json",
				"SYSTEM.md", "APPEND_SYSTEM.md", "AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD",
				"skills/", "extensions/", "agents/", "prompts/", "themes/",
				"sessions/", "npm/", "git/", "tmp/", "bin/", "node_modules/", "*cache*",
			},
		},
	},
	Ignore: []string{
		"auth.json",
		"mcp-auth.json",
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
