package opencode

import "github.com/zzjcool/homer-cli/internal/core"

// OpencodeAdapterID is the stable adapter identifier.
const OpencodeAdapterID = "opencode"

// OPENCODE_ADAPTER_ID preserves the archived adapter constant spelling.
const OPENCODE_ADAPTER_ID = OpencodeAdapterID

func boolPtr(value bool) *bool { return &value }

// DefaultOpencodeAdapter is frozen by docs/m3-scout-report.md §2.4:
// opencode.json and package.json are merged, lock files are mirrored, and
// generated/runtime files are ignored.
var DefaultOpencodeAdapter = core.AdapterConfig{
	Root:    "~/.config/opencode",
	Enabled: boolPtr(true),
	Categories: map[string]core.CategoryConfig{
		"config": {
			Paths: []string{"opencode.json"},
			Mode:  core.SyncMode("merge"),
		},
		"plugins": {
			Paths: []string{"package.json"},
			Mode:  core.SyncMode("merge"),
		},
		"locks": {
			Paths: []string{"package-lock.json", "bun.lock"},
			Mode:  core.SyncMode("mirror"),
		},
	},
	Ignore: []string{
		"node_modules/",
		".plugins.lock",
		"*.log",
		".gitignore",
	},
}

// DEFAULT_OPENCODE_ADAPTER preserves the archived TypeScript constant
// spelling.
var DEFAULT_OPENCODE_ADAPTER = DefaultOpencodeAdapter
