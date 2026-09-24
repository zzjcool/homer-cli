package opencode

import "github.com/zzjcool/homer-cli/internal/adapter"

type ScanError = adapter.ScanError
type ScanOutcome = adapter.ScanOutcome

// ScanAdapter reuses the common scanner; opencode has no adapter-specific
// traversal logic.
var ScanAdapter = adapter.ScanAdapter
