package opencode

import (
	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/core"
)

type ScanError = adapter.ScanError
type ScanOutcome = adapter.ScanOutcome

// ScanAdapter reuses the common scanner; opencode has no adapter-specific
// traversal logic.
var ScanAdapter = adapter.ScanAdapter
var Scan = adapter.ScanAdapter

func scanAdapter(adapterID string, config core.AdapterConfig) ScanOutcome {
	return adapter.ScanAdapter(adapterID, config)
}
