package resolutions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/zzjcool/homer-cli/internal/core"
)

// storeMu serializes all in-process reads and writes. Record, Clear, and
// RemoveIfSame use the private helpers below so each read-modify-write is one
// critical section.
var storeMu sync.Mutex

// Path returns <home>/resolutions.json. The literal file name appears only
// here and in types.go constants.
func Path(paths core.HomerPaths) string { return filepath.Join(paths.Home, FileName) }

// ValidChoice reports whether choice is a known decision.
func ValidChoice(choice string) bool { return choice == ChoiceCenter || choice == ChoiceLocal }

// Validate checks one entry's shape (not its staleness).
func (e Entry) Validate() error {
	if !core.ValidAdapterID(e.Adapter) {
		return fmt.Errorf("resolutions: invalid adapter id %q", e.Adapter)
	}
	if !ValidChoice(e.Choice) {
		return fmt.Errorf("resolutions: invalid choice %q", e.Choice)
	}
	if e.GenerationAtRecord < 1 {
		return fmt.Errorf("resolutions: generationAtRecord must be at least 1")
	}
	if _, err := time.Parse(time.RFC3339, e.RecordedAt); err != nil {
		return fmt.Errorf("resolutions: recordedAt must be RFC3339: %w", err)
	}
	return nil
}

// Same reports whether two entries agree on all four fields (CAS identity).
func (e Entry) Same(o Entry) bool {
	return e.Adapter == o.Adapter &&
		e.Choice == o.Choice &&
		e.RecordedAt == o.RecordedAt &&
		e.GenerationAtRecord == o.GenerationAtRecord
}

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

func emptyFile() File { return File{Version: FileVersion, Entries: []Entry{}} }

// loadUnlocked reads the file while the caller holds storeMu.
func loadUnlocked(paths core.HomerPaths) (File, error) {
	data, err := os.ReadFile(Path(paths))
	if errors.Is(err, os.ErrNotExist) {
		return emptyFile(), nil
	}
	if err != nil {
		return File{}, err
	}

	// Decode the version separately so an unknown schema is rejected even if
	// this build could not decode that schema's entries.
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil || document == nil {
		return File{}, ErrCorrupt
	}
	versionData, hasVersion := document["version"]
	if !hasVersion {
		return File{}, ErrCorrupt
	}
	var version *int
	if err := json.Unmarshal(versionData, &version); err != nil || version == nil {
		return File{}, ErrCorrupt
	}
	if *version != FileVersion {
		return File{}, ErrUnsupportedVersion
	}
	entriesData, hasEntries := document["entries"]
	if !hasEntries {
		return File{}, ErrCorrupt
	}

	var rawEntries []json.RawMessage
	if err := json.Unmarshal(entriesData, &rawEntries); err != nil || rawEntries == nil {
		return File{}, ErrCorrupt
	}

	// Invalid entries are isolated and discarded, including entries with
	// incorrectly typed fields. A repeated adapter retains its last valid
	// entry; the result is sorted for deterministic callers.
	byAdapter := make(map[string]Entry, len(rawEntries))
	for _, rawEntry := range rawEntries {
		var entry Entry
		if err := json.Unmarshal(rawEntry, &entry); err == nil && entry.Validate() == nil {
			byAdapter[entry.Adapter] = entry
		}
	}
	valid := make([]Entry, 0, len(byAdapter))
	for _, entry := range byAdapter {
		valid = append(valid, entry)
	}
	sortEntries(valid)
	return File{Version: FileVersion, Entries: valid}, nil
}

// Load reads the file. Missing file: empty, nil. Corrupt JSON: ErrCorrupt.
func Load(paths core.HomerPaths) (File, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	return loadUnlocked(paths)
}

func sortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Adapter < entries[j].Adapter
	})
}

// saveUnlocked writes a document while the caller holds storeMu.
func saveUnlocked(paths core.HomerPaths, file File) error {
	if file.Version != FileVersion {
		return ErrUnsupportedVersion
	}
	entries := append([]Entry(nil), file.Entries...)
	for _, entry := range entries {
		if err := entry.Validate(); err != nil {
			return err
		}
	}
	if len(entries) == 0 {
		err := os.Remove(Path(paths))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	sortEntries(entries)
	data, err := json.Marshal(File{Version: FileVersion, Entries: entries})
	if err != nil {
		return err
	}

	filename := Path(paths)
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("resolutions: create directory: %w", err)
	}
	// CreateTemp uses O_CREATE|O_EXCL in the target directory. The explicit
	// Chmod below preserves the required mode regardless of umask.
	temp, err := os.CreateTemp(dir, filepath.Base(filename)+".tmp-*")
	if err != nil {
		return fmt.Errorf("resolutions: create temporary file: %w", err)
	}
	tempName := temp.Name()
	keepTemp := true
	defer func() {
		_ = temp.Close()
		if keepTemp {
			_ = os.Remove(tempName)
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("resolutions: chmod temporary file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("resolutions: write temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("resolutions: sync temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("resolutions: close temporary file: %w", err)
	}
	if err := os.Rename(tempName, filename); err != nil {
		return fmt.Errorf("resolutions: replace file: %w", err)
	}
	keepTemp = false
	return nil
}

// Save writes the file atomically with 0600.
func Save(paths core.HomerPaths, file File) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	return saveUnlocked(paths, file)
}

// RecordResult reports a record operation.
type RecordResult struct {
	Recorded []Entry
	Replaced []Entry
}

// quarantineCorrupt renames an unreadable JSON file before Record replaces it.
func quarantineCorrupt(paths core.HomerPaths) error {
	filename := Path(paths)
	quarantine := fmt.Sprintf("%s.corrupt-%d", filename, time.Now().Unix())
	if err := os.Rename(filename, quarantine); err != nil {
		return fmt.Errorf("resolutions: quarantine corrupt file: %w", err)
	}
	return nil
}

// Record writes decisions for adapters (D2: same adapter overwrites).
func Record(paths core.HomerPaths, choice string, adapters []string, generation int, now func() string) (RecordResult, error) {
	storeMu.Lock()
	defer storeMu.Unlock()

	if !ValidChoice(choice) {
		return RecordResult{}, fmt.Errorf("resolutions: invalid choice %q", choice)
	}
	if generation < 1 {
		return RecordResult{}, fmt.Errorf("resolutions: generation must be at least 1")
	}
	if len(adapters) == 0 {
		return RecordResult{}, fmt.Errorf("resolutions: adapters must not be empty")
	}

	uniqueAdapters := make([]string, 0, len(adapters))
	seenAdapters := make(map[string]struct{}, len(adapters))
	for _, adapter := range adapters {
		if !core.ValidAdapterID(adapter) {
			return RecordResult{}, fmt.Errorf("resolutions: invalid adapter id %q", adapter)
		}
		if _, exists := seenAdapters[adapter]; exists {
			continue
		}
		seenAdapters[adapter] = struct{}{}
		uniqueAdapters = append(uniqueAdapters, adapter)
	}

	recordedAt := ""
	if now == nil {
		recordedAt = time.Now().UTC().Format(time.RFC3339Nano)
	} else {
		recordedAt = now()
	}
	newEntries := make([]Entry, 0, len(uniqueAdapters))
	for _, adapter := range uniqueAdapters {
		entry := Entry{
			Adapter:            adapter,
			Choice:             choice,
			RecordedAt:         recordedAt,
			GenerationAtRecord: generation,
		}
		if err := entry.Validate(); err != nil {
			return RecordResult{}, err
		}
		newEntries = append(newEntries, entry)
	}

	current, err := loadUnlocked(paths)
	if errors.Is(err, ErrCorrupt) {
		if err := quarantineCorrupt(paths); err != nil {
			return RecordResult{}, err
		}
		current = emptyFile()
	} else if err != nil {
		return RecordResult{}, err
	}

	byAdapter := Index(current.Entries)
	result := RecordResult{
		Recorded: make([]Entry, 0, len(newEntries)),
		Replaced: make([]Entry, 0),
	}
	for _, entry := range newEntries {
		if previous, exists := byAdapter[entry.Adapter]; exists {
			result.Replaced = append(result.Replaced, previous)
		}
		byAdapter[entry.Adapter] = entry
		result.Recorded = append(result.Recorded, entry)
	}
	entries := make([]Entry, 0, len(byAdapter))
	for _, entry := range byAdapter {
		entries = append(entries, entry)
	}
	sortEntries(entries)
	if err := saveUnlocked(paths, File{Version: FileVersion, Entries: entries}); err != nil {
		return RecordResult{}, err
	}
	return result, nil
}

// Clear removes entries for adapters unconditionally.
func Clear(paths core.HomerPaths, adapters []string) ([]Entry, error) {
	storeMu.Lock()
	defer storeMu.Unlock()

	if len(adapters) == 0 {
		return []Entry{}, nil
	}
	current, err := loadUnlocked(paths)
	if err != nil {
		return nil, err
	}
	remove := make(map[string]struct{}, len(adapters))
	for _, adapter := range adapters {
		remove[adapter] = struct{}{}
	}

	removed := make([]Entry, 0)
	remaining := make([]Entry, 0, len(current.Entries))
	for _, entry := range current.Entries {
		if _, ok := remove[entry.Adapter]; ok {
			removed = append(removed, entry)
			continue
		}
		remaining = append(remaining, entry)
	}
	if len(removed) == 0 {
		return removed, nil
	}
	if err := saveUnlocked(paths, File{Version: FileVersion, Entries: remaining}); err != nil {
		return nil, err
	}
	return removed, nil
}

// RemoveIfSame removes only entries that still match consumed (CAS).
func RemoveIfSame(paths core.HomerPaths, consumed []Entry) ([]Entry, error) {
	storeMu.Lock()
	defer storeMu.Unlock()

	if len(consumed) == 0 {
		return []Entry{}, nil
	}
	current, err := loadUnlocked(paths)
	if err != nil {
		return nil, err
	}
	expected := Index(consumed)
	removed := make([]Entry, 0)
	remaining := make([]Entry, 0, len(current.Entries))
	for _, entry := range current.Entries {
		previous, ok := expected[entry.Adapter]
		if ok && entry.Same(previous) {
			removed = append(removed, entry)
			continue
		}
		remaining = append(remaining, entry)
	}
	if len(removed) == 0 {
		return removed, nil
	}
	if err := saveUnlocked(paths, File{Version: FileVersion, Entries: remaining}); err != nil {
		return nil, err
	}
	return removed, nil
}
