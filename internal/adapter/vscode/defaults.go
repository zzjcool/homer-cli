package vscode

import "github.com/zzjcool/homer-cli/internal/core"

// VSCodeAdapterID is the stable adapter identifier written to snapshots and
// homer.json.
const VSCodeAdapterID = "vscode"

const VSCODE_ADAPTER_ID = VSCodeAdapterID

func boolPtr(value bool) *bool { return &value }

func kindPtr(value core.CategoryKind) *core.CategoryKind { return &value }

// DefaultVSCodeAdapter is the Linux VS Code user-data configuration. Settings
// and keybindings are ordinary file categories; extensions are represented by
// a manifest command pair and have no paths on disk.
var DefaultVSCodeAdapter = core.AdapterConfig{
	Root:    "~/.config/Code",
	Enabled: boolPtr(true),
	Categories: map[string]core.CategoryConfig{
		"settings": {
			Paths: []string{"settings.json"},
			Mode:  core.SyncModeMerge,
		},
		"keybindings": {
			Paths: []string{"keybindings.json"},
			Mode:  core.SyncModeMirror,
		},
		"extensions": {
			Kind:     kindPtr(core.CategoryKindManifest),
			Mode:     core.SyncModeMirror,
			ListCmd:  "code --list-extensions",
			ApplyCmd: "code --install-extension",
		},
	},
}
