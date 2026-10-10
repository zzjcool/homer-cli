package adapter

// Tool is a program a plugin's configuration belongs to: pi for the pi
// adapter, code for the vscode adapter. Every machine reports the version
// of each Tool it has installed, the console flags the ones that lag behind,
// and a Tool that knows how to upgrade itself can be upgraded from the console.
//
// A request from the hub names a Tool by ID; it never carries a command line.
// The argument lists below are the only commands an agent will run on its
// own for this purpose, and they always run Binary.
type Tool struct {
	// ID is the stable name used by the console and the HTTP API.
	ID string
	// Adapter is the adapter whose configuration this program reads.
	Adapter string
	// Label is the name shown in the console.
	Label string
	// Binary is looked up on the machine's login PATH.
	Binary string
	// VersionArgs make Binary print its version, for example --version.
	VersionArgs []string
	// MinVersion is a floor: a machine below it is flagged even when no
	// machine in the fleet is newer. Empty means no floor.
	MinVersion string
	// Install is the official copy-paste installer, shown when the program is
	// missing. Empty when the vendor has no one-line installer.
	Install string
	// UpgradeArgs upgrade Binary in place. Empty means homer only reports the
	// version and leaves updates to the machine's package manager.
	UpgradeArgs []string
}

// CanUpgrade reports whether the console may upgrade this program.
func (t Tool) CanUpgrade() bool { return len(t.UpgradeArgs) > 0 }
