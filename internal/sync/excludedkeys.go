package sync

import (
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// RequiredPlaceholder is the value stored in git for an excluded local key.
// It is deliberately a plain sentinel rather than a secret-bearing value.
const RequiredPlaceholder = "__REQUIRED__"

// REQUIRED_PLACEHOLDER preserves the migration-facing spelling used by the
// TypeScript implementation and its fixture names.
const REQUIRED_PLACEHOLDER = RequiredPlaceholder

// ParseJSONContent parses a snapshot entry without exposing parser errors to
// plan callers. A false result means malformed JSON.
func ParseJSONContent(content string) (orderedjson.Value, bool) {
	value, err := orderedjson.Parse([]byte(content))
	return value, err == nil
}

func parseJSONContent(content string) (orderedjson.Value, bool) {
	return ParseJSONContent(content)
}

func parseJsonContent(content string) (orderedjson.Value, bool) {
	return ParseJSONContent(content)
}

// IsJSONObject is the shared JSON-object predicate for merge eligibility and
// excluded-key operations. Arrays, scalars, null, and malformed input are not
// object records.
func IsJSONObject(value orderedjson.Value) bool {
	object, ok := value.(*orderedjson.Object)
	return ok && object != nil
}

func isJSONObject(value orderedjson.Value) bool { return IsJSONObject(value) }
func isJsonObject(value orderedjson.Value) bool { return IsJSONObject(value) }

// IsJsonObject is a spelling-compatible alias for callers using Go's initialism
// convention less strictly.
func IsJsonObject(value orderedjson.Value) bool { return IsJSONObject(value) }

// SerializeJSONContent is the canonical on-disk JSON format for merged files
// and placeholder snapshots: JSON.stringify(value, null, 2) plus one newline.
func SerializeJSONContent(value orderedjson.Value) string {
	return string(orderedjson.SerializeFile(value))
}

func serializeJSONContent(value orderedjson.Value) string { return SerializeJSONContent(value) }
func serializeJsonContent(value orderedjson.Value) string { return SerializeJSONContent(value) }

// ExcludedKeysByCategory returns configured top-level excluded keys by
// category. The returned slices are copies so callers cannot mutate config.
func ExcludedKeysByCategory(config core.HomerConfig, adapterID string) map[string][]string {
	adapter, ok := config.Adapters[adapterID]
	if !ok {
		return map[string][]string{}
	}
	out := make(map[string][]string)
	for category, cfg := range adapter.Categories {
		if len(cfg.ExcludeKeys) == 0 {
			continue
		}
		out[category] = append([]string(nil), cfg.ExcludeKeys...)
	}
	return out
}

func excludedKeysByCategory(config core.HomerConfig, adapterID string) map[string][]string {
	return ExcludedKeysByCategory(config, adapterID)
}

// ExcludedKeysFor returns the configured excluded top-level keys for one
// adapter/category. No keys is represented by an empty slice.
func ExcludedKeysFor(config core.HomerConfig, adapterID, category string) []string {
	adapter, ok := config.Adapters[adapterID]
	if !ok {
		return nil
	}
	return append([]string(nil), adapter.Categories[category].ExcludeKeys...)
}

func excludedKeysFor(config core.HomerConfig, adapterID, category string) []string {
	return ExcludedKeysFor(config, adapterID, category)
}

// StripSnapshotExcludeKeys removes configured top-level keys from JSON entries
// for judgement. Like the TypeScript JSON.stringify path, valid entries are
// compactly serialized; raw entries are never mutated.
func StripSnapshotExcludeKeys(snapshot core.AdapterSnapshot, config core.HomerConfig) core.AdapterSnapshot {
	byCategory := ExcludedKeysByCategory(config, snapshot.AdapterID)
	out := cloneSnapshot(snapshot)
	if len(byCategory) == 0 {
		return out
	}
	for index := range out.Categories {
		category := &out.Categories[index]
		keys := byCategory[category.Category]
		if len(keys) == 0 {
			continue
		}
		files := make(core.SnapshotFiles, len(category.Files))
		for path, entry := range category.Files {
			files[path] = stripEntry(entry, keys)
		}
		category.Files = files
	}
	return out
}

func stripSnapshotExcludeKeys(snapshot core.AdapterSnapshot, config core.HomerConfig) core.AdapterSnapshot {
	return StripSnapshotExcludeKeys(snapshot, config)
}

// ApplyExcludeKeyPlaceholders replaces only excluded keys that actually exist
// in a local snapshot. It never invents a missing required key.
func ApplyExcludeKeyPlaceholders(snapshot core.AdapterSnapshot, config core.HomerConfig) core.AdapterSnapshot {
	byCategory := ExcludedKeysByCategory(config, snapshot.AdapterID)
	out := cloneSnapshot(snapshot)
	for index := range out.Categories {
		category := &out.Categories[index]
		keys := byCategory[category.Category]
		if len(keys) == 0 {
			continue
		}
		files := make(core.SnapshotFiles, len(category.Files))
		for path, entry := range category.Files {
			files[path] = placeholderEntry(entry, keys)
		}
		category.Files = files
	}
	return out
}

func applyExcludeKeyPlaceholders(snapshot core.AdapterSnapshot, config core.HomerConfig) core.AdapterSnapshot {
	return ApplyExcludeKeyPlaceholders(snapshot, config)
}

// PlantExcludedKeys puts local values back into a successful merged object.
// Existing local keys win; a key absent from local is deliberately not
// reintroduced. The returned value is a new object only when a planting was
// needed.
func PlantExcludedKeys(merged, localValue orderedjson.Value, keys []string) orderedjson.Value {
	mergedObject, ok := merged.(*orderedjson.Object)
	if !ok || mergedObject == nil || !IsJSONObject(localValue) || len(keys) == 0 {
		return merged
	}
	localObject := localValue.(*orderedjson.Object)
	keySet := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		keySet[key] = struct{}{}
	}

	out := &orderedjson.Object{Keys: make([]string, 0, len(mergedObject.M)+len(keys)), M: make(map[string]orderedjson.Value)}
	changed := false
	for _, key := range objectKeys(mergedObject) {
		value := mergedObject.M[key]
		if _, excluded := keySet[key]; excluded {
			if local, exists := localObject.M[key]; exists {
				value = local
				changed = true
			}
		}
		out.Keys = append(out.Keys, key)
		out.M[key] = value
	}
	for _, key := range keys {
		if _, exists := mergedObject.M[key]; exists {
			continue
		}
		if local, exists := localObject.M[key]; exists {
			out.Keys = append(out.Keys, key)
			out.M[key] = local
			changed = true
		}
	}
	if !changed {
		return merged
	}
	return out
}

func plantExcludedKeys(merged, localValue orderedjson.Value, keys []string) orderedjson.Value {
	return PlantExcludedKeys(merged, localValue, keys)
}

func cloneSnapshot(snapshot core.AdapterSnapshot) core.AdapterSnapshot {
	out := snapshot
	out.Categories = make([]core.CategorySnapshot, len(snapshot.Categories))
	for index, category := range snapshot.Categories {
		out.Categories[index] = category
		out.Categories[index].Files = make(core.SnapshotFiles, len(category.Files))
		for path, entry := range category.Files {
			out.Categories[index].Files[path] = entry
		}
	}
	return out
}

func stripEntry(entry core.SnapshotEntry, keys []string) core.SnapshotEntry {
	if entry.Kind != "json" {
		return entry
	}
	value, ok := ParseJSONContent(entry.Content)
	if !ok {
		return entry
	}
	stripped := orderedjson.StripTopKeys(value, keys)
	return core.SnapshotEntry{Kind: "json", Content: compactSerialize(stripped)}
}

func placeholderEntry(entry core.SnapshotEntry, keys []string) core.SnapshotEntry {
	if entry.Kind != "json" {
		return entry
	}
	value, ok := ParseJSONContent(entry.Content)
	if !ok || !IsJSONObject(value) {
		return entry
	}
	object := value.(*orderedjson.Object)
	keySet := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		keySet[key] = struct{}{}
	}
	matched := false
	for key := range object.M {
		if _, ok := keySet[key]; ok {
			matched = true
			break
		}
	}
	if !matched {
		return entry
	}

	replaced := &orderedjson.Object{Keys: make([]string, 0, len(object.M)), M: make(map[string]orderedjson.Value, len(object.M))}
	for _, key := range objectKeys(object) {
		value := object.M[key]
		if _, replace := keySet[key]; replace {
			value = RequiredPlaceholder
		}
		replaced.Keys = append(replaced.Keys, key)
		replaced.M[key] = value
	}
	return core.SnapshotEntry{Kind: "json", Content: SerializeJSONContent(replaced)}
}

func objectKeys(object *orderedjson.Object) []string {
	if object == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(object.M))
	keys := make([]string, 0, len(object.M))
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
	extra := make([]string, 0)
	for key := range object.M {
		if _, exists := seen[key]; !exists {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	return append(keys, extra...)
}

// compactSerialize minifies orderedjson's canonical output without touching
// whitespace inside JSON strings. This preserves insertion order while
// matching JSON.stringify(value) for the judgement snapshots.
func compactSerialize(value orderedjson.Value) string {
	pretty := orderedjson.Serialize(value)
	var out strings.Builder
	out.Grow(len(pretty))
	inString := false
	escaped := false
	for _, char := range string(pretty) {
		if inString {
			out.WriteRune(char)
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
			continue
		}
		if char == '"' {
			inString = true
			out.WriteRune(char)
			continue
		}
		switch char {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			out.WriteRune(char)
		}
	}
	return out.String()
}
