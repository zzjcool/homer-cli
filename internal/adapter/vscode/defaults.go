package vscode

import (
	"runtime"

	"github.com/zzjcool/homer-cli/internal/core"
)

// VSCodeAdapterID is the stable adapter identifier written to snapshots and
// homer.json.
const VSCodeAdapterID = "vscode"

const (
	DefaultVSCodeRootLinux  = "~/.config/Code"
	DefaultVSCodeRootDarwin = "~/Library/Application Support/Code"
)

// DefaultVSCodeRoot selects the per-platform VS Code user-data root. The
// explicit argument keeps both supported release platforms testable without
// mutating runtime.GOOS.
func DefaultVSCodeRoot(goos string) string {
	if goos == "darwin" {
		return DefaultVSCodeRootDarwin
	}
	return DefaultVSCodeRootLinux
}

func boolPtr(value bool) *bool { return &value }

func kindPtr(value core.CategoryKind) *core.CategoryKind { return &value }

// DefaultVSCodeAdapter is the platform-specific VS Code user-data
// configuration. Settings and keybindings are ordinary file categories;
// extensions are represented by a manifest command pair and have no paths on
// disk.
var DefaultVSCodeAdapter = core.AdapterConfig{
	Root:    DefaultVSCodeRoot(runtime.GOOS),
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
