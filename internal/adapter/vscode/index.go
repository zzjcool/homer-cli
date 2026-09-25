package vscode

import "github.com/zzjcool/homer-cli/internal/adapter"

// ScanError and ScanOutcome are aliases so the built-in VS Code adapter uses
// the common scanner result type.
type ScanError = adapter.ScanError
type ScanOutcome = adapter.ScanOutcome

// ScanAdapter is the common declaration-driven scanner.
var ScanAdapter = adapter.ScanAdapter
