package engine

import (
	"sort"

	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// Value is the JSON value representation used by the orderedjson package.  The
// alias keeps the engine signatures readable while leaving JSON parsing,
// equality, and serialization in their single owning package.
type Value = orderedjson.Value

// MergeConflict describes one ambiguous key in a three-way JSON merge.  A
// missing side is represented by its zero Value (nil); JSON null is also nil,
// which is the same limitation as the frozen Value interface.
type MergeConflict struct {
	KeyPath string
	Reason  string
	Base    Value
	Local   Value
	Remote  Value
}

type MergeResult struct {
	Status    string
	Merged    Value
	Conflicts []MergeConflict
}

// MergeJSON performs a key-level three-way merge of JSON values.
//
// Objects are merged recursively.  Arrays and all other non-object values are
// atomic: a side equal to base yields to the other side, equal edits converge,
// and different edits produce a conflict while retaining the local value.
// Conflicting keys likewise retain the local value so a later resolution layer
// can present a safe, deterministic candidate to the caller.
func MergeJSON(base, local, remote Value) MergeResult {
	conflicts := make([]MergeConflict, 0)
	resolved := mergeSlot("", presentSlot(base), presentSlot(local), presentSlot(remote), &conflicts)

	// The frozen API returns a JSON Value, but merge files are records.  Match
	// the legacy fallback for an unusual scalar/array root with an empty object.
	merged, ok := objectOf(resolved.value)
	if !resolved.has || !ok {
		merged = emptyObject()
	}

	status := "clean"
	if len(conflicts) != 0 {
		status = "conflict"
	}
	return MergeResult{Status: status, Merged: merged, Conflicts: conflicts}
}

type valueSlot struct {
	has   bool
	value Value
}

func presentSlot(value Value) valueSlot { return valueSlot{has: true, value: value} }

func absentSlot() valueSlot { return valueSlot{} }

func sameSlot(a, b valueSlot) bool {
	if a.has != b.has {
		return false
	}
	if !a.has {
		return true
	}
	return orderedjson.DeepEqual(a.value, b.value)
}

func mergeSlot(path string, base, local, remote valueSlot, conflicts *[]MergeConflict) valueSlot {
	// Neither side changed relative to base.  This also covers three absent
	// children and keeps the local insertion/value choice deterministic.
	if sameSlot(local, base) && sameSlot(remote, base) {
		if local.has {
			return local
		}
		return absentSlot()
	}

	// One side is unchanged: the other side wins, including add and delete.
	if sameSlot(local, base) {
		return remote
	}
	if sameSlot(remote, base) {
		return local
	}

	// Both sides changed relative to base.
	if !local.has && !remote.has {
		return absentSlot()
	}
	if !local.has || !remote.has {
		// With an absent base, an absent side would have been caught by one of
		// the unchanged-side branches above.  Keep this guard for malformed
		// callers and preserve the side that exists rather than inventing a
		// conflict.
		if !base.has {
			if local.has {
				return local
			}
			return remote
		}
		conflict := MergeConflict{KeyPath: path, Reason: "modify-vs-delete"}
		appendConflict(conflicts, conflict, base, local, remote)
		if local.has {
			return local
		}
		return absentSlot()
	}

	// Equal edits are clean even when both differ from base.
	if orderedjson.DeepEqual(local.value, remote.value) {
		return local
	}

	_, localIsObject := objectOf(local.value)
	_, remoteIsObject := objectOf(remote.value)
	_, baseIsObject := objectOf(base.value)
	if localIsObject && remoteIsObject && (!base.has || baseIsObject) {
		return mergeObject(path, base, local, remote, conflicts)
	}

	// Arrays are atomic.  If either side is an array, the frozen reason is
	// array-both-changed even when the other side changed from/to a scalar.
	if isArray(local.value) || isArray(remote.value) {
		conflict := MergeConflict{KeyPath: path, Reason: "array-both-changed"}
		appendConflict(conflicts, conflict, base, local, remote)
		return local
	}

	conflict := MergeConflict{KeyPath: path, Reason: "both-modified"}
	appendConflict(conflicts, conflict, base, local, remote)
	return local
}

func mergeObject(
	path string,
	base, local, remote valueSlot,
	conflicts *[]MergeConflict,
) valueSlot {
	out := emptyObject()
	for _, key := range unionObjectKeys(base, local, remote) {
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		resolved := mergeSlot(
			childPath,
			childSlot(base, key),
			childSlot(local, key),
			childSlot(remote, key),
			conflicts,
		)
		if resolved.has {
			putObject(out, key, resolved.value)
		}
	}
	return valueSlot{has: true, value: out}
}

func appendConflict(dst *[]MergeConflict, partial MergeConflict, base, local, remote valueSlot) {
	if base.has {
		partial.Base = base.value
	}
	if local.has {
		partial.Local = local.value
	}
	if remote.has {
		partial.Remote = remote.value
	}
	*dst = append(*dst, partial)
}

func childSlot(parent valueSlot, key string) valueSlot {
	if !parent.has {
		return absentSlot()
	}
	object, ok := objectOf(parent.value)
	if !ok || object == nil || object.M == nil {
		return absentSlot()
	}
	value, ok := object.M[key]
	if !ok {
		return absentSlot()
	}
	return presentSlot(value)
}

func unionObjectKeys(slots ...valueSlot) []string {
	seen := make(map[string]struct{})
	keys := make([]string, 0)
	for _, slot := range slots {
		if !slot.has {
			continue
		}
		object, ok := objectOf(slot.value)
		if !ok || object == nil {
			continue
		}
		for _, key := range objectKeys(object) {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	return keys
}

func objectKeys(object *orderedjson.Object) []string {
	if object == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(object.Keys))
	keys := make([]string, 0, len(object.Keys))
	for _, key := range object.Keys {
		if _, exists := object.M[key]; !exists {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	// Parsed values always have Keys and M in sync.  The sorted fallback makes
	// hand-built test values with only M deterministic without changing the
	// parser's insertion order.
	extra := make([]string, 0)
	for key := range object.M {
		if _, exists := seen[key]; !exists {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	return append(keys, extra...)
}

func putObject(object *orderedjson.Object, key string, value Value) {
	if object.M == nil {
		object.M = make(map[string]Value)
	}
	if _, exists := object.M[key]; !exists {
		object.Keys = append(object.Keys, key)
	}
	object.M[key] = value
}

func emptyObject() *orderedjson.Object {
	return &orderedjson.Object{Keys: make([]string, 0), M: make(map[string]Value)}
}

func objectOf(value Value) (*orderedjson.Object, bool) {
	switch object := value.(type) {
	case *orderedjson.Object:
		return object, object != nil
	case orderedjson.Object:
		// Object is documented as a pointer value, but accepting a value here
		// makes the engine tolerant of hand-built fixtures without weakening the
		// frozen representation.
		copy := object
		return &copy, true
	default:
		return nil, false
	}
}

func isArray(value Value) bool {
	_, ok := value.([]Value)
	return ok
}

// DiffJSONResult is the key-level change summary used by status/drift.
type DiffJSONResult struct {
	Changed int
	Added   int
	Deleted int
	Keys    []string
}

// DiffJSON reports local's changes relative to base.  Objects recurse to leaf
// keys; a newly added/deleted subtree is represented by its root key, while
// arrays and scalar values are atomic.
func DiffJSON(base, local Value) DiffJSONResult {
	buckets := diffBuckets{}
	walkDiff("", presentSlot(base), presentSlot(local), &buckets)
	keys := make([]string, 0, len(buckets.changed)+len(buckets.added)+len(buckets.deleted))
	keys = append(keys, buckets.changed...)
	keys = append(keys, buckets.added...)
	keys = append(keys, buckets.deleted...)
	sort.Strings(keys)
	return DiffJSONResult{
		Changed: len(buckets.changed),
		Added:   len(buckets.added),
		Deleted: len(buckets.deleted),
		Keys:    keys,
	}
}

type diffBuckets struct {
	changed []string
	added   []string
	deleted []string
}

func walkDiff(path string, base, local valueSlot, buckets *diffBuckets) {
	label := path
	if label == "" {
		label = "$"
	}

	if !base.has && !local.has {
		return
	}
	if !local.has {
		buckets.deleted = append(buckets.deleted, label)
		return
	}
	if !base.has {
		buckets.added = append(buckets.added, label)
		return
	}
	if orderedjson.DeepEqual(base.value, local.value) {
		return
	}

	_, baseObject := objectOf(base.value)
	_, localObject := objectOf(local.value)
	if baseObject && localObject {
		for _, key := range unionObjectKeys(base, local) {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			walkDiff(childPath, childSlot(base, key), childSlot(local, key), buckets)
		}
		return
	}

	buckets.changed = append(buckets.changed, label)
}
