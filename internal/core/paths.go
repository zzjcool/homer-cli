package core

import (
	"os"
	"path/filepath"
	"strings"
)

// HomerPaths contains every path in a Homer workspace.
type HomerPaths struct {
	Home       string
	StoreDir   string
	ConfigFile string
	StateFile  string
	BackupsDir string
	SecretsDir string
	KeysDir    string
}

// expandHome expands only a leading ~ or ~/ (and the Windows spelling ~\\).
// Other occurrences of ~ are ordinary path characters, matching shell
// expansion semantics and the TypeScript implementation.
func expandHome(input string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	if input == "~" {
		return home
	}
	if strings.HasPrefix(input, "~/") || strings.HasPrefix(input, `~\`) {
		return filepath.Join(home, input[2:])
	}
	return input
}

// ExpandHome is the exported form used by adapter and command packages.
func ExpandHome(input string) string { return expandHome(input) }

// GetHomerPaths resolves the Homer workspace. A non-empty, trimmed
// HOMER_HOME returned by env wins over ~/.homer; relative values are resolved
// against the current working directory. Passing nil uses os.Getenv.
func GetHomerPaths(env func(string) string) HomerPaths {
	if env == nil {
		env = os.Getenv
	}

	raw := strings.TrimSpace(env("HOMER_HOME"))
	home := ""
	if raw != "" {
		home = expandHome(raw)
		if absolute, err := filepath.Abs(home); err == nil {
			home = absolute
		}
	} else {
		userHome, err := os.UserHomeDir()
		if err != nil {
			userHome = "."
		}
		home = filepath.Join(userHome, ".homer")
	}

	return HomerPaths{
		Home:       home,
		StoreDir:   filepath.Join(home, "store"),
		ConfigFile: filepath.Join(home, "homer.json"),
		StateFile:  filepath.Join(home, "state.json"),
		BackupsDir: filepath.Join(home, "backups"),
		SecretsDir: filepath.Join(home, "secrets"),
		KeysDir:    filepath.Join(home, "keys"),
	}
}
