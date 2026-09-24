package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

func TestGoldenMergeVectors(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "golden", "merge", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if len(files) != 32 {
		t.Fatalf("golden merge vector count = %d, want 32", len(files))
	}

	for _, file := range files {
		file := file
		t.Run(filepath.Base(file), func(t *testing.T) {
			fixture := readJSONFixture(t, file)
			input := objectFieldObject(t, fixture, "input")
			result := MergeJSON(input.M["base"], input.M["local"], input.M["remote"])
			output := objectFieldObject(t, fixture, "output")

			if got, want := result.Status, stringField(t, output, "status"); got != want {
				t.Fatalf("status = %q, want %q", got, want)
			}
			wantMerged := output.M["merged"]
			if got, want := string(orderedjson.Serialize(result.Merged)), string(orderedjson.Serialize(wantMerged)); got != want {
				t.Fatalf("merged JSON differs\n got: %s\nwant: %s", got, want)
			}

			wantConflicts := valueArray(t, output.M["conflicts"])
			if len(result.Conflicts) != len(wantConflicts) {
				t.Fatalf("conflicts = %d, want %d", len(result.Conflicts), len(wantConflicts))
			}
			for index, wantValue := range wantConflicts {
				want := valueObject(t, wantValue)
				got := result.Conflicts[index]
				if got.KeyPath != stringField(t, want, "keyPath") || got.Reason != stringField(t, want, "reason") {
					t.Errorf("conflict[%d] header = (%q, %q), want (%q, %q)", index, got.KeyPath, got.Reason, want.M["keyPath"], want.M["reason"])
				}
				assertOptionalValue(t, "base", got.Base, want)
				assertOptionalValue(t, "local", got.Local, want)
				assertOptionalValue(t, "remote", got.Remote, want)
			}
		})
	}
}

func TestGoldenMirrorVectors(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "golden", "mirror", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if len(files) != 15 {
		t.Fatalf("golden mirror vector count = %d, want 15", len(files))
	}

	for _, file := range files {
		file := file
		t.Run(filepath.Base(file), func(t *testing.T) {
			fixture := readJSONFixture(t, file)
			input := objectFieldObject(t, fixture, "input")
			var got []MirrorOp
			switch stringField(t, fixture, "operation") {
			case "compareFile":
				got = []MirrorOp{CompareFile(
					fixtureEntry(t, input.M["base"]),
					fixtureEntry(t, input.M["local"]),
					fixtureEntry(t, input.M["remote"]),
					stringField(t, input, "relPath"),
				)}
			case "compareCategory":
				got = CompareCategory(
					fixtureFiles(t, input.M["base"]),
					fixtureFiles(t, input.M["local"]),
					fixtureFiles(t, input.M["remote"]),
				)
			default:
				t.Fatalf("unknown operation")
			}

			if stringField(t, fixture, "operation") == "compareFile" {
				wantOp := valueObject(t, fixture.M["output"])
				if len(got) != 1 {
					t.Fatalf("ops = %d, want one", len(got))
				}
				assertMirrorOp(t, got[0], wantOp)
				return
			}

			want := valueArray(t, fixture.M["output"])
			if len(got) != len(want) {
				t.Fatalf("ops = %d, want %d", len(got), len(want))
			}
			for index, wantValue := range want {
				assertMirrorOp(t, got[index], valueObject(t, wantValue))
			}
		})
	}
}

func TestMergeSemanticMatrix(t *testing.T) {
	one := json.Number("1")
	two := json.Number("2")
	three := json.Number("3")
	tests := []struct {
		name       string
		base       Value
		local      Value
		remote     Value
		status     string
		conflict   string
		wantMerged Value
	}{
		{
			name:       "nested disjoint keys recurse",
			base:       object("settings", object("theme", "light", "name", "base")),
			local:      object("settings", object("theme", "light", "name", "local")),
			remote:     object("settings", object("theme", "dark", "name", "base")),
			status:     "clean",
			wantMerged: object("settings", object("theme", "dark", "name", "local")),
		},
		{
			name:       "array local unchanged takes remote",
			base:       object("items", []Value{one}),
			local:      object("items", []Value{one}),
			remote:     object("items", []Value{two}),
			status:     "clean",
			wantMerged: object("items", []Value{two}),
		},
		{
			name:       "array remote unchanged takes local",
			base:       object("items", []Value{one}),
			local:      object("items", []Value{two}),
			remote:     object("items", []Value{one}),
			status:     "clean",
			wantMerged: object("items", []Value{two}),
		},
		{
			name:       "array same edit converges",
			base:       object("items", []Value{one}),
			local:      object("items", []Value{two}),
			remote:     object("items", []Value{two}),
			status:     "clean",
			wantMerged: object("items", []Value{two}),
		},
		{
			name:       "array different edits conflict",
			base:       object("items", []Value{one}),
			local:      object("items", []Value{two}),
			remote:     object("items", []Value{three}),
			status:     "conflict",
			conflict:   "array-both-changed",
			wantMerged: object("items", []Value{two}),
		},
		{
			name:       "local deletion remote unchanged",
			base:       object("keep", true, "gone", "x"),
			local:      object("keep", true),
			remote:     object("keep", true, "gone", "x"),
			status:     "clean",
			wantMerged: object("keep", true),
		},
		{
			name:       "remote deletion local unchanged",
			base:       object("keep", true, "gone", "x"),
			local:      object("keep", true, "gone", "x"),
			remote:     object("keep", true),
			status:     "clean",
			wantMerged: object("keep", true),
		},
		{
			name:       "local deletion versus remote modify",
			base:       object("key", "base"),
			local:      object(),
			remote:     object("key", "remote"),
			status:     "conflict",
			conflict:   "modify-vs-delete",
			wantMerged: object(),
		},
		{
			name:       "local modify versus remote deletion",
			base:       object("key", "base"),
			local:      object("key", "local"),
			remote:     object(),
			status:     "conflict",
			conflict:   "modify-vs-delete",
			wantMerged: object("key", "local"),
		},
		{
			name:       "different keys auto merge",
			base:       object("a", one, "b", two),
			local:      object("a", three, "b", two),
			remote:     object("a", one, "b", three),
			status:     "clean",
			wantMerged: object("a", three, "b", three),
		},
		{
			name:       "both new same value",
			base:       object(),
			local:      object("new", object("value", true)),
			remote:     object("new", object("value", true)),
			status:     "clean",
			wantMerged: object("new", object("value", true)),
		},
		{
			name:       "both new different values",
			base:       object(),
			local:      object("new", "local"),
			remote:     object("new", "remote"),
			status:     "conflict",
			conflict:   "both-modified",
			wantMerged: object("new", "local"),
		},
		{
			name:       "non object root falls back to empty object",
			base:       []Value{one},
			local:      []Value{two},
			remote:     []Value{three},
			status:     "conflict",
			conflict:   "array-both-changed",
			wantMerged: object(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := MergeJSON(test.base, test.local, test.remote)
			if got.Status != test.status {
				t.Fatalf("status = %q, want %q", got.Status, test.status)
			}
			if len(got.Conflicts) != boolInt(test.conflict != "") {
				t.Fatalf("conflicts = %#v, want one iff %q", got.Conflicts, test.conflict)
			}
			if test.conflict != "" && got.Conflicts[0].Reason != test.conflict {
				t.Fatalf("reason = %q, want %q", got.Conflicts[0].Reason, test.conflict)
			}
			if gotJSON, wantJSON := string(orderedjson.Serialize(got.Merged)), string(orderedjson.Serialize(test.wantMerged)); gotJSON != wantJSON {
				t.Fatalf("merged = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestDiffJSONCountsAndLeafKeys(t *testing.T) {
	one := json.Number("1")
	two := json.Number("2")
	three := json.Number("3")
	base := object("same", true, "changed", one, "removed", "x", "nested", object("keep", true, "old", one))
	local := object("same", true, "changed", two, "added", "new", "nested", object("keep", true, "new", three))
	got := DiffJSON(base, local)
	if got.Changed != 1 || got.Added != 2 || got.Deleted != 2 {
		t.Fatalf("counts = %#v, want changed=1 added=2 deleted=2", got)
	}
	want := []string{"added", "changed", "nested.new", "nested.old", "removed"}
	if strings.Join(got.Keys, "|") != strings.Join(want, "|") {
		t.Fatalf("keys = %#v, want %#v", got.Keys, want)
	}
}

func TestMirrorMatrixIncludesKindAgnosticEquality(t *testing.T) {
	base := &SnapshotEntry{Kind: "json", Content: "v1"}
	local := &SnapshotEntry{Kind: "file", Content: "v1"}
	remote := &SnapshotEntry{Kind: "json", Content: "v1"}
	got := CompareFile(base, local, remote, "x")
	if got.Type != "noop" || got.Path != "x" {
		t.Fatalf("got %#v, want noop", got)
	}
}

func TestComputeDriftAggregatesMergeMirrorDegradedAndRemoteDefault(t *testing.T) {
	mergeBase := jsonEntry(object("a", json.Number("1"), "b", json.Number("2")))
	mergeLocal := jsonEntry(object("a", json.Number("3"), "b", json.Number("2")))
	mergeRemote := jsonEntry(object("a", json.Number("1"), "b", json.Number("4")))
	mirrorBase := fileEntry("v1")
	mirrorLocal := fileEntry("v2")
	mirrorRemote := fileEntry("v1")

	base := []AdapterSnapshot{{AdapterID: "tool", Categories: []CategorySnapshot{
		{AdapterID: "tool", Category: "merge", Mode: SyncMode("merge"), Files: SnapshotFiles{"settings.json": mergeBase}},
		{AdapterID: "tool", Category: "mirror", Mode: SyncMode("mirror"), Files: SnapshotFiles{"notes.txt": mirrorBase}},
	}}}
	local := []AdapterSnapshot{{AdapterID: "tool", Categories: []CategorySnapshot{
		{AdapterID: "tool", Category: "merge", Mode: SyncMode("merge"), Files: SnapshotFiles{"settings.json": mergeLocal}},
		{AdapterID: "tool", Category: "mirror", Mode: SyncMode("mirror"), Files: SnapshotFiles{"notes.txt": mirrorLocal}},
	}}}
	remote := []AdapterSnapshot{{AdapterID: "tool", Categories: []CategorySnapshot{
		{AdapterID: "tool", Category: "merge", Mode: SyncMode("merge"), Files: SnapshotFiles{"settings.json": mergeRemote}},
		{AdapterID: "tool", Category: "mirror", Mode: SyncMode("mirror"), Files: SnapshotFiles{"notes.txt": mirrorRemote}},
	}}}

	got := ComputeDrift(base, local, remote)
	if len(got) != 2 {
		t.Fatalf("categories = %d, want 2", len(got))
	}
	if got[0].Category != "merge" || got[0].Push != 1 || got[0].Pull != 1 || got[0].Conflicts != 0 || strings.Join(got[0].ChangedKeys, "|") != "settings.json:a" {
		t.Fatalf("merge drift = %#v", got[0])
	}
	if got[1].Category != "mirror" || got[1].Push != 1 || got[1].Pull != 0 || got[1].Conflicts != 0 {
		t.Fatalf("mirror drift = %#v", got[1])
	}

	// A malformed JSON entry in a merge category is downgraded to mirror.
	degraded := []AdapterSnapshot{{AdapterID: "tool", Categories: []CategorySnapshot{{
		AdapterID: "tool", Category: "merge", Mode: SyncMode("merge"), Files: SnapshotFiles{"bad.json": {Kind: "file", Content: "old"}},
	}}}}
	degradedLocal := []AdapterSnapshot{{AdapterID: "tool", Categories: []CategorySnapshot{{
		AdapterID: "tool", Category: "merge", Mode: SyncMode("merge"), Files: SnapshotFiles{"bad.json": {Kind: "file", Content: "new"}},
	}}}}
	degradedDrift := ComputeDrift(degraded, degradedLocal, degraded)
	if len(degradedDrift) != 1 || degradedDrift[0].Push != 1 || len(degradedDrift[0].Ops) != 1 || degradedDrift[0].Ops[0].Type != "push" {
		t.Fatalf("degraded drift = %#v", degradedDrift)
	}

	// Nil remote is the frozen remote==base default, so local==base is clean.
	if got := ComputeDrift(base, base, nil); len(got) != 2 || got[0].Push != 0 || got[0].Pull != 0 || got[0].Conflicts != 0 || got[1].Push != 0 {
		t.Fatalf("remote default drift = %#v", got)
	}
}

func TestComputeDriftBaseAbsentLocalNewFileIsPush(t *testing.T) {
	local := []AdapterSnapshot{{AdapterID: "tool", Categories: []CategorySnapshot{{
		AdapterID: "tool", Category: "settings", Mode: SyncMode("merge"), Files: SnapshotFiles{"new.json": jsonEntry(object("new", true))},
	}}}}
	got := ComputeDrift(nil, local, nil)
	if len(got) != 1 || got[0].Push != 1 || got[0].Conflicts != 0 || len(got[0].MergeConflicts) != 0 {
		t.Fatalf("base-absent drift = %#v", got)
	}
}

func TestComputeDriftExcludedTopKeyCanBeStrippedBeforeComparison(t *testing.T) {
	baseValue, err := orderedjson.Parse([]byte(`{"theme":"light","apiKeys":{"service":"old"}}`))
	if err != nil {
		t.Fatal(err)
	}
	localValue, err := orderedjson.Parse([]byte(`{"theme":"light","apiKeys":{"service":"new"}}`))
	if err != nil {
		t.Fatal(err)
	}
	base := snapshotWithEntry("settings", "settings.json", jsonEntry(baseValue))
	local := snapshotWithEntry("settings", "settings.json", jsonEntry(localValue))
	sourceLocal := local[0]
	base = []AdapterSnapshot{StripExcludeKeys(base[0], map[string][]string{"settings": {"apiKeys"}})}
	local = []AdapterSnapshot{StripExcludeKeys(local[0], map[string][]string{"settings": {"apiKeys"}})}
	got := ComputeDrift(base, local, nil)
	if len(got) != 1 || got[0].Push != 0 || got[0].Pull != 0 || got[0].Conflicts != 0 || len(got[0].ChangedKeys) != 0 {
		t.Fatalf("stripped excluded-key drift = %#v", got)
	}
	if strings.Contains(base[0].Categories[0].Files["settings.json"].Content, "apiKeys") {
		t.Fatal("stripped snapshot still contains excluded key")
	}
	if !strings.Contains(sourceLocal.Categories[0].Files["settings.json"].Content, "apiKeys") {
		t.Fatal("source snapshot was mutated while stripping")
	}
}

func assertMirrorOp(t *testing.T, got MirrorOp, want *orderedjson.Object) {
	t.Helper()
	if got.Type != stringField(t, want, "type") || got.Path != stringField(t, want, "path") {
		t.Errorf("op = %#v, want type/path from %s", got, orderedjson.Serialize(want))
	}
	wantReason, hasReason := want.M["reason"]
	if hasReason && got.Reason != stringValue(t, wantReason) {
		t.Errorf("op.reason = %q, want %q", got.Reason, stringValue(t, wantReason))
	}
}

func readJSONFixture(t *testing.T, path string) *orderedjson.Object {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value, err := orderedjson.Parse(data)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return valueObject(t, value)
}

func objectFieldObject(t *testing.T, object *orderedjson.Object, key string) *orderedjson.Object {
	t.Helper()
	value, ok := object.M[key]
	if !ok {
		t.Fatalf("missing object field %q", key)
	}
	return valueObject(t, value)
}

func valueObject(t *testing.T, value Value) *orderedjson.Object {
	t.Helper()
	object, ok := value.(*orderedjson.Object)
	if !ok || object == nil {
		t.Fatalf("value %T is not an object", value)
	}
	return object
}

func valueArray(t *testing.T, value Value) []Value {
	t.Helper()
	array, ok := value.([]Value)
	if !ok {
		t.Fatalf("value %T is not an array", value)
	}
	return array
}

func stringField(t *testing.T, object *orderedjson.Object, key string) string {
	t.Helper()
	value, ok := object.M[key]
	if !ok {
		t.Fatalf("missing string field %q", key)
	}
	return stringValue(t, value)
}

func stringValue(t *testing.T, value Value) string {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		t.Fatalf("value %T is not a string", value)
	}
	return text
}

func assertOptionalValue(t *testing.T, key string, got Value, want *orderedjson.Object) {
	t.Helper()
	wantValue, exists := want.M[key]
	if !exists {
		if got != nil {
			t.Errorf("conflict.%s = %#v, want absent", key, got)
		}
		return
	}
	if !orderedjson.DeepEqual(got, wantValue) {
		t.Errorf("conflict.%s = %s, want %s", key, orderedjson.Serialize(got), orderedjson.Serialize(wantValue))
	}
}

func fixtureEntry(t *testing.T, value Value) *SnapshotEntry {
	t.Helper()
	if value == nil {
		return nil
	}
	object := valueObject(t, value)
	return &SnapshotEntry{Kind: stringField(t, object, "kind"), Content: stringField(t, object, "content")}
}

func fixtureFiles(t *testing.T, value Value) SnapshotFiles {
	t.Helper()
	if value == nil {
		return SnapshotFiles{}
	}
	object := valueObject(t, value)
	files := make(SnapshotFiles, len(object.M))
	for _, key := range object.Keys {
		entry, ok := object.M[key]
		if ok {
			files[key] = *fixtureEntry(t, entry)
		}
	}
	return files
}

func object(values ...Value) *orderedjson.Object {
	result := &orderedjson.Object{Keys: make([]string, 0, len(values)/2), M: make(map[string]Value, len(values)/2)}
	for index := 0; index+1 < len(values); index += 2 {
		key := values[index].(string)
		result.Keys = append(result.Keys, key)
		result.M[key] = values[index+1]
	}
	return result
}

func jsonEntry(value Value) SnapshotEntry {
	return SnapshotEntry{Kind: "json", Content: string(orderedjson.Serialize(value))}
}

func fileEntry(content string) SnapshotEntry { return SnapshotEntry{Kind: "file", Content: content} }

func snapshotWithEntry(category, path string, entry SnapshotEntry) []AdapterSnapshot {
	return []AdapterSnapshot{{AdapterID: "tool", Categories: []CategorySnapshot{{
		AdapterID: "tool", Category: category, Mode: SyncMode("merge"), Files: SnapshotFiles{path: entry},
	}}}}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
