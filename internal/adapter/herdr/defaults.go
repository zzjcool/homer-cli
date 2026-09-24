package herdr

import "github.com/zzjcool/homer-cli/internal/core"

// HerdrAdapterID is the stable adapter identifier.
const HerdrAdapterID = "herdr"

// HERDR_ADAPTER_ID preserves the archived adapter constant spelling.
const HERDR_ADAPTER_ID = HerdrAdapterID

func boolPtr(value bool) *bool { return &value }

// DefaultHerdrAdapter is frozen by docs/m3-scout-report.md §1.5: config.toml
// is the only user setting and is mirrored; the remaining entries are runtime
// state, sockets, logs, or release metadata.
var DefaultHerdrAdapter = core.AdapterConfig{
	Root:    "~/.config/herdr",
	Enabled: boolPtr(true),
	Categories: map[string]core.CategoryConfig{
		"config": {
			Paths: []string{"config.toml"},
			Mode:  core.SyncMode("mirror"),
		},
	},
	Ignore: []string{
		"session.json",
		"*.sock",
		"*.log",
		".plugins.lock",
		"release-notes.json",
	},
}

// DEFAULT_HERDR_ADAPTER preserves the archived TypeScript constant spelling.
var DEFAULT_HERDR_ADAPTER = DefaultHerdrAdapter
