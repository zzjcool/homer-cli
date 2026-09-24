package core

import (
	"reflect"

	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// isParsableJson reports whether content is any valid JSON value. JSON arrays,
// scalars, and null are intentionally valid here; the merge engine decides
// later whether a value has object semantics.
func isParsableJson(content string) bool {
	_, err := orderedjson.Parse([]byte(content))
	return err == nil
}

// IsParsableJSON is the exported spelling for packages outside core.
func IsParsableJSON(content string) bool { return isParsableJson(content) }

// IsParsableJson is kept as a Go-style compatibility alias.
func IsParsableJson(content string) bool { return isParsableJson(content) }

// entryKindFor is shared by adapter scanning and store reading. Only merge
// categories use JSON/key-level semantics; mirror categories remain files even
// when their bytes happen to contain valid JSON.
func entryKindFor(mode SyncMode, content string) string {
	if mode == SyncModeMerge && isParsableJson(content) {
		return "json"
	}
	return "file"
}

// EntryKindFor is the exported form used by the other internal packages.
func EntryKindFor(mode SyncMode, content string) string { return entryKindFor(mode, content) }

// isPlainObject is the JSON-object shape predicate used by config/state
// validation. Arrays and nil are not objects for Homer configuration purposes.
func isPlainObject(value any) bool {
	if value == nil {
		return false
	}
	if _, ok := value.(*orderedjson.Object); ok {
		return true
	}
	rv := reflect.ValueOf(value)
	return rv.Kind() == reflect.Map && rv.Type().Key().Kind() == reflect.String && !rv.IsNil()
}

// IsPlainObject is the exported form used by package tests and future workers.
func IsPlainObject(value any) bool { return isPlainObject(value) }
