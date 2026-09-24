package pi

import "github.com/zzjcool/homer-cli/internal/adapter"

// ScanError and ScanOutcome are aliases so all built-in adapters expose the
// same scanner result type.
type ScanError = adapter.ScanError
type ScanOutcome = adapter.ScanOutcome

// ScanAdapter is the common declaration-driven scanner.
var ScanAdapter = adapter.ScanAdapter
