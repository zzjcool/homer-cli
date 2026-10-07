package keys

import "github.com/zzjcool/homer-cli/internal/core"

// AdapterID is the synced keyring. The id is "keyring", not "keys": homer's
// gitignore entry "keys/" hides the local age identity directory and would
// also hide a store/keys tree.
const AdapterID = "keyring"

func boolPtr(value bool) *bool { return &value }

// DefaultAdapter mirrors ~/.homer/keyring/items. Each key is one directory.
var DefaultAdapter = core.AdapterConfig{
	Root:    "~/.homer/keyring",
	Enabled: boolPtr(true),
	Categories: map[string]core.CategoryConfig{
		"items": {
			Paths: []string{"items/"},
			Mode:  core.SyncModeMirror,
		},
	},
}
