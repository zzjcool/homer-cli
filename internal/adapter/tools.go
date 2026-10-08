package adapter

import "runtime"

// Tool is a program a built-in adapter's configuration belongs to: pi for the
// pi adapter, code for the vscode adapter. Every machine reports the version
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

// Tools lists every program homer tracks, in the order the console shows
// them. The result is a copy; callers may modify it.
//
// Adding an adapter's program is one entry here: the agent starts reporting
// its version, the console starts flagging it, and the upgrade button works
// for it, with no other change. Program names must stay aligned with the
// adapter's listCmd and applyCmd so a missing program is caught before a
// dispatch.
func Tools() []Tool {
	return []Tool{
		{
			ID: "pi", Adapter: "pi", Label: "pi", Binary: "pi",
			VersionArgs: []string{"--version"},
			Install:     piInstallCommand(),
			// --self names the target: pi only, never the installed
			// packages, which travel with the synced packages category.
			UpgradeArgs: []string{"update", "--self"},
		},
		{
			ID: "herdr", Adapter: "herdr", Label: "herdr", Binary: "herdr",
			VersionArgs: []string{"--version"},
			Install:     "curl -fsSL https://herdr.dev/install.sh | sh",
			// Only for installs made by herdr's own installer; Homebrew, mise
			// and Nix installs are updated by their package manager and the
			// command says so when it refuses.
			UpgradeArgs: []string{"update"},
		},
		{
			ID: "opencode", Adapter: "opencode", Label: "opencode", Binary: "opencode",
			VersionArgs: []string{"--version"},
			Install:     "curl -fsSL https://opencode.ai/install | bash",
			UpgradeArgs: []string{"upgrade"},
		},
		{
			// VS Code is updated by the operating system's package manager.
			ID: "vscode", Adapter: "vscode", Label: "VS Code", Binary: "code",
			VersionArgs: []string{"--version"},
		},
	}
}

// ToolByID finds a registered program.
func ToolByID(id string) (Tool, bool) {
	for _, tool := range Tools() {
		if tool.ID == id {
			return tool, true
		}
	}
	return Tool{}, false
}

// OfficialInstall returns the copy-paste installer for a CLI that a built-in
// adapter invokes from listCmd or applyCmd. binary is that program name, for
// example "pi" from "pi install". Programs without a one-line installer, such
// as VS Code's "code", have no entry: homer cannot tell the user a command
// that would work everywhere.
func OfficialInstall(binary string) (string, bool) {
	for _, tool := range Tools() {
		if tool.Binary == binary && tool.Install != "" {
			return tool.Install, true
		}
	}
	return "", false
}

// piInstallCommand is the official installer documented at
// https://pi.dev/docs/latest/quickstart
func piInstallCommand() string {
	if runtime.GOOS == "windows" {
		return `powershell -c "irm https://pi.dev/install.ps1 | iex"`
	}
	return "curl -fsSL https://pi.dev/install.sh | sh"
}
