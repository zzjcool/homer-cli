package engine

import (
	"sort"

	"github.com/zzjcool/homer-cli/internal/core"
)

// These aliases keep the public engine signatures aligned with the frozen
// core snapshot types while allowing callers to use either package's name.
type SnapshotEntry = core.SnapshotEntry
type SnapshotFiles = core.SnapshotFiles
type CategorySnapshot = core.CategorySnapshot
type AdapterSnapshot = core.AdapterSnapshot
type SyncMode = core.SyncMode

type MirrorOp struct {
	Type   string
	Path   string
	Reason string
}

// CompareFile applies the mirror 3x3 matrix.  File equality is deliberately
// byte/string equality and ignores Kind; a kind transition must not itself
// create drift.  With no base, one-sided files are new push/pull entries and a
// two-sided content mismatch is a conflict.
func CompareFile(base, local, remote *SnapshotEntry, relPath string) MirrorOp {
	noop := MirrorOp{Type: "noop", Path: relPath}

	if base == nil && local == nil && remote == nil {
		return noop
	}

	baseAbsent := base == nil
	localAbsent := local == nil
	remoteAbsent := remote == nil

	localEqualsBase := !localAbsent && !baseAbsent && local.Content == base.Content
	remoteEqualsBase := !remoteAbsent && !baseAbsent && remote.Content == base.Content

	// Both sides deleted is the completed state of the path.
	if localAbsent && remoteAbsent {
		return noop
	}

	if baseAbsent {
		switch {
		case localAbsent:
			return MirrorOp{Type: "pull", Path: relPath}
		case remoteAbsent:
			return MirrorOp{Type: "push", Path: relPath}
		case local.Content == remote.Content:
			return noop
		default:
			return MirrorOp{Type: "conflict", Path: relPath, Reason: "modify-vs-modify"}
		}
	}

	// A single deletion is safe only when the other side stayed at base.
	if localAbsent {
		if remoteEqualsBase {
			return MirrorOp{Type: "push-delete", Path: relPath}
		}
		return MirrorOp{Type: "conflict", Path: relPath, Reason: "local-delete-vs-remote-modify"}
	}
	if remoteAbsent {
		if localEqualsBase {
			return MirrorOp{Type: "pull-delete", Path: relPath}
		}
		return MirrorOp{Type: "conflict", Path: relPath, Reason: "local-modify-vs-remote-delete"}
	}

	if localEqualsBase && remoteEqualsBase {
		return noop
	}
	if localEqualsBase {
		return MirrorOp{Type: "pull", Path: relPath}
	}
	if remoteEqualsBase {
		return MirrorOp{Type: "push", Path: relPath}
	}

	// Two changed sides remain a conflict even when their resulting content is
	// equal.  This is intentionally stricter than merge JSON equality and is
	// part of the frozen mirror semantics.
	return MirrorOp{Type: "conflict", Path: relPath, Reason: "modify-vs-modify"}
}

// CompareCategory compares the sorted path union of the three snapshots.
func CompareCategory(base, local, remote SnapshotFiles) []MirrorOp {
	seen := make(map[string]struct{}, len(base)+len(local)+len(remote))
	paths := make([]string, 0, len(base)+len(local)+len(remote))
	for path := range base {
		seen[path] = struct{}{}
	}
	for path := range local {
		seen[path] = struct{}{}
	}
	for path := range remote {
		seen[path] = struct{}{}
	}
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	ops := make([]MirrorOp, 0, len(paths))
	for _, path := range paths {
		ops = append(ops, CompareFile(snapshotEntry(base, path), snapshotEntry(local, path), snapshotEntry(remote, path), path))
	}
	return ops
}

func snapshotEntry(files SnapshotFiles, path string) *SnapshotEntry {
	entry, ok := files[path]
	if !ok {
		return nil
	}
	// Return a copy so callers cannot mutate the map's value through the
	// address retained by the result calculation.
	copy := entry
	return &copy
}
