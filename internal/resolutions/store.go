package resolutions

import (
	"errors"
	"path/filepath"

	"github.com/zzjcool/homer-cli/internal/core"
)

// Path returns <home>/resolutions.json. The literal file name appears only
// here and in types.go constants.
func Path(paths core.HomerPaths) string { return filepath.Join(paths.Home, FileName) }

// ValidChoice reports whether choice is a known decision.
func ValidChoice(choice string) bool { return choice == ChoiceCenter || choice == ChoiceLocal }

// Validate checks one entry's shape (not its staleness).
func (e Entry) Validate() error { return errors.New("resolutions: not implemented (S0 stub)") }

// Same reports whether two entries agree on all four fields (CAS identity).
func (e Entry) Same(o Entry) bool { return false }

// Stale reports whether the entry must not be consumed at centerGeneration.
// Fail-safe: unknown center generation (<1) is treated as stale.
func Stale(e Entry, centerGeneration int) bool {
	return centerGeneration < 1 || e.GenerationAtRecord != centerGeneration
}

// Index maps adapter -> entry; with duplicates the last wins.
func Index(entries []Entry) map[string]Entry {
	out := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		out[entry.Adapter] = entry
	}
	return out
}

// Load reads the file. Missing file: empty, nil. Corrupt JSON: ErrCorrupt.
func Load(paths core.HomerPaths) (File, error) {
	return File{}, errors.New("resolutions: not implemented (S0 stub)")
}

// Save writes the file atomically with 0600.
func Save(paths core.HomerPaths, file File) error {
	return errors.New("resolutions: not implemented (S0 stub)")
}

// RecordResult reports a record operation.
type RecordResult struct {
	Recorded []Entry
	Replaced []Entry
}

// Record writes decisions for adapters (D2: same adapter overwrites).
func Record(paths core.HomerPaths, choice string, adapters []string, generation int, now func() string) (RecordResult, error) {
	return RecordResult{}, errors.New("resolutions: not implemented (S0 stub)")
}

// Clear removes entries for adapters unconditionally.
func Clear(paths core.HomerPaths, adapters []string) ([]Entry, error) {
	return nil, errors.New("resolutions: not implemented (S0 stub)")
}

// RemoveIfSame removes only entries that still match consumed (CAS).
func RemoveIfSame(paths core.HomerPaths, consumed []Entry) ([]Entry, error) {
	return nil, errors.New("resolutions: not implemented (S0 stub)")
}
