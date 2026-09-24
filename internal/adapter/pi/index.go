package pi

import (
	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/core"
)

// ScanError and ScanOutcome are aliases so all built-in adapters expose the
// same scanner result type.
type ScanError = adapter.ScanError
type ScanOutcome = adapter.ScanOutcome

// ScanAdapter is the common declaration-driven scanner.
var ScanAdapter = adapter.ScanAdapter

// Scan is a concise alias used by Go callers.
var Scan = adapter.ScanAdapter

var GlobMatch = adapter.GlobMatch
var MatchesIgnore = adapter.MatchesIgnore

// Unexported compatibility wrappers keep package-local tests close to the
// archived TypeScript names while the exported aliases above form the Go API.
func scanAdapter(adapterID string, config core.AdapterConfig) ScanOutcome {
	return adapter.ScanAdapter(adapterID, config)
}

func globMatch(candidate, pattern string) bool { return adapter.GlobMatch(candidate, pattern) }
func matchesIgnore(relPath string, patterns []string) bool {
	return adapter.MatchesIgnore(relPath, patterns)
}
