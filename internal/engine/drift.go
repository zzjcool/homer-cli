package engine

import (
	"sort"

	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

type CategoryDrift struct {
	AdapterID      string
	Category       string
	Mode           SyncMode
	Push           int
	Pull           int
	Conflicts      int
	Ops            []MirrorOp
	MergeConflicts []MergeConflict
	ChangedKeys    []string
}

// StripExcludeKeys returns a deep, immutable snapshot copy with the configured
// top-level keys removed from JSON entries in each named category.  The
// operation is kept beside drift because callers must apply the same transform
// to base/local/remote before ComputeDrift; mirror and degraded entries are
// deliberately left byte-for-byte untouched.
func StripExcludeKeys(snapshot AdapterSnapshot, excludeKeysByCategory map[string][]string) AdapterSnapshot {
	out := snapshot
	out.Categories = make([]CategorySnapshot, len(snapshot.Categories))
	for index, category := range snapshot.Categories {
		copyCategory := category
		copyCategory.Files = make(SnapshotFiles, len(category.Files))
		keys := excludeKeysByCategory[category.Category]
		for path, entry := range category.Files {
			copyEntry := entry
			if len(keys) != 0 && entry.Kind == "json" {
				if value, ok := parseEntry(&entry); ok {
					copyEntry.Content = string(orderedjson.Serialize(orderedjson.StripTopKeys(value, keys)))
				}
			}
			copyCategory.Files[path] = copyEntry
		}
		out.Categories[index] = copyCategory
	}
	return out
}

// StripExcludeKeysSnapshots applies StripExcludeKeys without mutating the
// caller's slice.  It is a convenience for the common three-snapshot path.
func StripExcludeKeysSnapshots(snapshots []AdapterSnapshot, excludeKeysByCategory map[string][]string) []AdapterSnapshot {
	out := make([]AdapterSnapshot, len(snapshots))
	for index, snapshot := range snapshots {
		out[index] = StripExcludeKeys(snapshot, excludeKeysByCategory)
	}
	return out
}

// ComputeDrift aggregates the three-way engine decisions by category.  A nil
// remote slice means the M1/MVP default remote==base; a non-nil empty slice is
// a genuinely empty remote snapshot.
func ComputeDrift(base, local, remote []AdapterSnapshot) []CategoryDrift {
	if remote == nil {
		remote = base
	}

	adapterIDs := unionAdapterIDs(base, local, remote)
	out := make([]CategoryDrift, 0)
	for _, adapterID := range adapterIDs {
		baseAdapter := findAdapter(base, adapterID)
		localAdapter := findAdapter(local, adapterID)
		remoteAdapter := findAdapter(remote, adapterID)

		for _, categoryName := range unionCategoryNames(baseAdapter, localAdapter, remoteAdapter) {
			baseCategory := findCategory(baseAdapter, categoryName)
			localCategory := findCategory(localAdapter, categoryName)
			remoteCategory := findCategory(remoteAdapter, categoryName)

			mode, ok := categoryMode(localCategory, baseCategory, remoteCategory)
			if !ok {
				continue
			}
			out = append(out, computeCategoryDrift(
				adapterID,
				categoryName,
				mode,
				baseCategory,
				localCategory,
				remoteCategory,
			))
		}
	}
	return out
}

func computeCategoryDrift(
	adapterID, category string,
	mode SyncMode,
	baseCategory, localCategory, remoteCategory *CategorySnapshot,
) CategoryDrift {
	drift := CategoryDrift{
		AdapterID:      adapterID,
		Category:       category,
		Mode:           mode,
		Ops:            make([]MirrorOp, 0),
		MergeConflicts: make([]MergeConflict, 0),
		ChangedKeys:    make([]string, 0),
	}

	baseFiles := categoryFiles(baseCategory)
	localFiles := categoryFiles(localCategory)
	remoteFiles := categoryFiles(remoteCategory)

	if mode == SyncMode("mirror") {
		drift.Ops = CompareCategory(baseFiles, localFiles, remoteFiles)
		for _, op := range drift.Ops {
			countMirrorOp(&drift, op)
		}
		return drift
	}

	for _, relPath := range unionPaths(baseFiles, localFiles, remoteFiles) {
		accumulateMergeFile(
			&drift,
			relPath,
			snapshotEntry(baseFiles, relPath),
			snapshotEntry(localFiles, relPath),
			snapshotEntry(remoteFiles, relPath),
		)
	}
	return drift
}

func countMirrorOp(drift *CategoryDrift, op MirrorOp) {
	switch op.Type {
	case "push", "push-delete":
		drift.Push++
	case "pull", "pull-delete":
		drift.Pull++
	case "conflict":
		drift.Conflicts++
	}
}

func accumulateMergeFile(
	drift *CategoryDrift,
	relPath string,
	baseEntry, localEntry, remoteEntry *SnapshotEntry,
) {
	// A merge category can contain raw files (or malformed JSON).  Such a file
	// is a mirror item, not a reason for the status command to fail parsing.
	if isDegraded(baseEntry) || isDegraded(localEntry) || isDegraded(remoteEntry) {
		op := CompareFile(baseEntry, localEntry, remoteEntry, relPath)
		drift.Ops = append(drift.Ops, op)
		countMirrorOp(drift, op)
		return
	}

	// Both sides deleted is already converged and must not create a synthetic
	// empty-object diff.
	if localEntry == nil && remoteEntry == nil {
		return
	}

	baseValue := emptyObjectValue()
	if baseEntry != nil {
		parsed, ok := parseEntry(baseEntry)
		if !ok {
			// isDegraded should have caught this.  Keep a defensive mirror
			// fallback in case a future SnapshotEntry kind changes underneath us.
			op := CompareFile(baseEntry, localEntry, remoteEntry, relPath)
			drift.Ops = append(drift.Ops, op)
			countMirrorOp(drift, op)
			return
		}
		baseValue = parsed
	}
	localValue := emptyObjectValue()
	if localEntry != nil {
		parsed, ok := parseEntry(localEntry)
		if !ok {
			op := CompareFile(baseEntry, localEntry, remoteEntry, relPath)
			drift.Ops = append(drift.Ops, op)
			countMirrorOp(drift, op)
			return
		}
		localValue = parsed
	}

	localDiff := DiffJSON(baseValue, localValue)
	drift.Push += localDiff.Changed + localDiff.Added + localDiff.Deleted
	for _, key := range localDiff.Keys {
		drift.ChangedKeys = append(drift.ChangedKeys, relPath+":"+key)
	}

	// Base-absent is the first-contact union case.  It must precede the
	// remote-absent branch: with remote==base, a local-only new file is a push,
	// not a modify-vs-delete conflict.
	if baseEntry == nil {
		if localEntry == nil {
			drift.Pull++
			return
		}
		if remoteEntry == nil {
			return
		}
		if localEntry.Content != remoteEntry.Content {
			drift.Conflicts++
			drift.MergeConflicts = append(drift.MergeConflicts, MergeConflict{
				KeyPath: relPath,
				Reason:  "both-modified",
			})
		}
		return
	}

	// Local deletion is safe when remote stayed byte-for-byte at base.  A
	// remote content change makes it a file-level delete/modify conflict.
	if localEntry == nil {
		if remoteEntry != nil && remoteEntry.Content != baseEntry.Content {
			drift.Conflicts++
			drift.MergeConflicts = append(drift.MergeConflicts, MergeConflict{
				KeyPath: relPath,
				Reason:  "modify-vs-delete",
			})
		}
		return
	}

	// Remote deletion is a pull when local is unchanged; otherwise it conflicts
	// at file level and leaves local content as the safe candidate.
	if remoteEntry == nil {
		if len(localDiff.Keys) == 0 {
			drift.Pull++
		} else {
			drift.Conflicts++
			drift.MergeConflicts = append(drift.MergeConflicts, MergeConflict{
				KeyPath: relPath,
				Reason:  "modify-vs-delete",
			})
		}
		return
	}

	remoteValue, ok := parseEntry(remoteEntry)
	if !ok {
		// Defensive only; the degraded check above handles normal paths.
		op := CompareFile(baseEntry, localEntry, remoteEntry, relPath)
		drift.Ops = append(drift.Ops, op)
		countMirrorOp(drift, op)
		return
	}

	result := MergeJSON(baseValue, localValue, remoteValue)
	drift.Conflicts += len(result.Conflicts)
	drift.MergeConflicts = append(drift.MergeConflicts, result.Conflicts...)

	// Pull counts only remote keys that local did not also change.  Conflicting
	// keys are therefore not double-counted as pulls.
	localChanged := make(map[string]struct{}, len(localDiff.Keys))
	for _, key := range localDiff.Keys {
		localChanged[key] = struct{}{}
	}
	remoteDiff := DiffJSON(baseValue, remoteValue)
	for _, key := range remoteDiff.Keys {
		if _, changedLocally := localChanged[key]; !changedLocally {
			drift.Pull++
		}
	}
}

func isDegraded(entry *SnapshotEntry) bool {
	if entry == nil {
		return false
	}
	if entry.Kind == "file" {
		return true
	}
	_, ok := parseEntry(entry)
	return !ok
}

func parseEntry(entry *SnapshotEntry) (Value, bool) {
	if entry == nil {
		return nil, false
	}
	value, err := orderedjson.Parse([]byte(entry.Content))
	return value, err == nil
}

func emptyObjectValue() Value { return emptyObject() }

func categoryFiles(category *CategorySnapshot) SnapshotFiles {
	if category == nil || category.Files == nil {
		return SnapshotFiles{}
	}
	return category.Files
}

func categoryMode(categories ...*CategorySnapshot) (SyncMode, bool) {
	for _, category := range categories {
		if category != nil && category.Mode != SyncMode("") {
			return category.Mode, true
		}
	}
	return SyncMode(""), false
}

func unionAdapterIDs(groups ...[]AdapterSnapshot) []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, group := range groups {
		for _, adapter := range group {
			if _, exists := seen[adapter.AdapterID]; exists {
				continue
			}
			seen[adapter.AdapterID] = struct{}{}
			ids = append(ids, adapter.AdapterID)
		}
	}
	return ids
}

func unionCategoryNames(adapters ...*AdapterSnapshot) []string {
	seen := make(map[string]struct{})
	names := make([]string, 0)
	for _, adapter := range adapters {
		if adapter == nil {
			continue
		}
		for _, category := range adapter.Categories {
			if _, exists := seen[category.Category]; exists {
				continue
			}
			seen[category.Category] = struct{}{}
			names = append(names, category.Category)
		}
	}
	return names
}

func unionPaths(fileSets ...SnapshotFiles) []string {
	seen := make(map[string]struct{})
	for _, files := range fileSets {
		for path := range files {
			seen[path] = struct{}{}
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func findAdapter(snapshots []AdapterSnapshot, adapterID string) *AdapterSnapshot {
	for index := range snapshots {
		if snapshots[index].AdapterID == adapterID {
			return &snapshots[index]
		}
	}
	return nil
}

func findCategory(adapter *AdapterSnapshot, categoryName string) *CategorySnapshot {
	if adapter == nil {
		return nil
	}
	for index := range adapter.Categories {
		if adapter.Categories[index].Category == categoryName {
			return &adapter.Categories[index]
		}
	}
	return nil
}
