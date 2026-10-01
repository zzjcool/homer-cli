package sync

import (
	"fmt"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
)

// ParseAdapterIDs normalizes an explicit adapter selection. Blank tokens
// are skipped; duplicates collapse to the first occurrence. An empty
// result is an error — callers use a nil selection to mean "everything".
func ParseAdapterIDs(ids []string) ([]string, error) {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		for _, part := range strings.Split(raw, ",") {
			id := strings.TrimSpace(part)
			if id == "" {
				continue
			}
			if !core.ValidAdapterID(id) {
				return nil, fmt.Errorf("适配器 ID 无效: %s", id)
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("请选择至少一个适配器")
	}
	return out, nil
}

// FilterSnapshots keeps snapshots whose adapter id is in ids, preserving
// the input order. A nil ids means unrestricted and returns snapshots
// unchanged. Adapters that are not in the selection are dropped.
func FilterSnapshots(snapshots []core.AdapterSnapshot, ids []string) []core.AdapterSnapshot {
	if ids == nil {
		return snapshots
	}
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	out := make([]core.AdapterSnapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if _, ok := want[snapshot.AdapterID]; ok {
			out = append(out, snapshot)
		}
	}
	return out
}

// MissingAdapterIDs returns ids that do not appear in snapshots, in the
// same order as ids.
func MissingAdapterIDs(snapshots []core.AdapterSnapshot, ids []string) []string {
	have := make(map[string]struct{}, len(snapshots))
	for _, snapshot := range snapshots {
		have[snapshot.AdapterID] = struct{}{}
	}
	var missing []string
	for _, id := range ids {
		if _, ok := have[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing
}

// MergeScopedStore copies base and then replaces each scope adapter with
// the patch. Adapters outside the scope are left untouched. A scope
// adapter missing from the patch, or present with no files, is removed.
// Extra adapters in the patch are ignored. Neither input map is mutated.
func MergeScopedStore(base, patch map[string]map[string]string, scope []string) map[string]map[string]string {
	out := cloneStore(base)
	for _, id := range scope {
		delete(out, id)
		files := patch[id]
		if len(files) == 0 {
			continue
		}
		out[id] = cloneFiles(files)
	}
	return out
}

func cloneStore(in map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(in))
	for id, files := range in {
		out[id] = cloneFiles(files)
	}
	return out
}

func cloneFiles(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for path, content := range in {
		out[path] = content
	}
	return out
}

// DecideScopedPublish decides whether a collect of ids should publish.
// An adapter the center does not have yet is new content and publishes.
// An adapter the center already has is judged with the normal three-way
// check: a conflict is reported, a center-ahead adapter still publishes
// (the user explicitly asked to store this machine's copy), and identical
// content does not.
func DecideScopedPublish(config core.HomerConfig, base, local, remote []core.AdapterSnapshot, ids []string) (publish bool, conflicts []PullConflictAction) {
	for _, id := range ids {
		if len(MissingAdapterIDs(remote, []string{id})) > 0 {
			publish = true
			continue
		}
		one := []string{id}
		check := CheckPushSafety(config,
			FilterSnapshots(base, one),
			FilterSnapshots(local, one),
			FilterSnapshots(remote, one),
		)
		switch check.Status {
		case PushStatusConflicts:
			conflicts = append(conflicts, check.ConflictItems...)
		case PushStatusRemoteAhead:
			publish = true
		default:
			if len(check.ChangedFiles) > 0 {
				publish = true
			}
		}
	}
	return publish, conflicts
}
