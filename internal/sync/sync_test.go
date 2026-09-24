package sync

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

func TestGoldenPlanPullVectors(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "golden", "planpull", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if len(files) != 20 {
		t.Fatalf("planpull golden count = %d, want 20", len(files))
	}
	for _, filename := range files {
		filename := filename
		t.Run(filepath.Base(filename), func(t *testing.T) {
			fixture := readSyncFixture(t, filename)
			input := objectValue(t, fixture.M["input"])
			config := syncConfig(t, input.M["config"])
			operation := stringValue(t, fixture.M["operation"])
			if operation == "applyExcludeKeyPlaceholders" {
				got := ApplyExcludeKeyPlaceholders(syncSnapshots(t, input.M["snapshot"])[0], config)
				want := syncSnapshots(t, fixture.M["output"])[0]
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("placeholder snapshot = %#v, want %#v", got, want)
				}
				return
			}
			got := PlanPull(config, syncSnapshots(t, input.M["base"]), syncSnapshots(t, input.M["local"]), syncSnapshots(t, input.M["remote"]))
			wantActions := actionValues(t, objectValue(t, fixture.M["output"]).M["actions"])
			assertActions(t, got.Actions, wantActions)
		})
	}
}

func TestGoldenFirstContactVectors(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "golden", "firstcontact", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if len(files) != 8 {
		t.Fatalf("firstcontact golden count = %d, want 8", len(files))
	}
	for _, filename := range files {
		filename := filename
		t.Run(filepath.Base(filename), func(t *testing.T) {
			fixture := readSyncFixture(t, filename)
			input := objectValue(t, fixture.M["input"])
			config := syncConfig(t, input.M["config"])
			mode := FirstContactMode(stringValue(t, input.M["mode"]))
			got := PlanFirstContact(config, syncSnapshots(t, input.M["local"]), syncSnapshots(t, input.M["remote"]), mode)
			output := objectValue(t, fixture.M["output"])
			if got.Mode != mode {
				t.Fatalf("mode = %q, want %q", got.Mode, mode)
			}
			assertActions(t, got.Actions, actionValues(t, output.M["actions"]))
		})
	}
}

func assertActions(t *testing.T, got []PullAction, want []*orderedjson.Object) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("actions = %d, want %d: %#v", len(got), len(want), got)
	}
	for index, expected := range want {
		actual := got[index]
		for _, key := range []string{"type", "adapterId", "category", "relPath", "content", "reason", "localContent", "remoteContent"} {
			wantValue, present := expected.M[key]
			gotValue := actionField(actual, key)
			if !present {
				if gotValue != "" {
					t.Errorf("action[%d].%s = %q, want omitted", index, key, gotValue)
				}
				continue
			}
			if value := stringValue(t, wantValue); gotValue != value {
				t.Errorf("action[%d].%s = %q, want %q", index, key, gotValue, value)
			}
		}
		wantKeys, present := expected.M["keyPaths"]
		if present {
			values := arrayValue(t, wantKeys)
			if len(actual.KeyPaths) != len(values) {
				t.Errorf("action[%d].keyPaths = %#v, want %#v", index, actual.KeyPaths, values)
			} else {
				for keyIndex, value := range values {
					if actual.KeyPaths[keyIndex] != stringValue(t, value) {
						t.Errorf("action[%d].keyPaths[%d] = %q, want %q", index, keyIndex, actual.KeyPaths[keyIndex], stringValue(t, value))
					}
				}
			}
		} else if len(actual.KeyPaths) != 0 {
			t.Errorf("action[%d].keyPaths = %#v, want omitted", index, actual.KeyPaths)
		}
	}
}

func actionField(action PullAction, key string) string {
	switch key {
	case "type":
		return action.Type
	case "adapterId":
		return action.AdapterID
	case "category":
		return action.Category
	case "relPath":
		return action.RelPath
	case "content":
		return action.Content
	case "reason":
		return action.Reason
	case "localContent":
		return action.LocalContent
	case "remoteContent":
		return action.RemoteContent
	default:
		return ""
	}
}

func readSyncFixture(t *testing.T, filename string) *orderedjson.Object {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	value, err := orderedjson.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return objectValue(t, value)
}

func syncConfig(t *testing.T, value orderedjson.Value) core.HomerConfig {
	t.Helper()
	root := objectValue(t, value)
	config := core.HomerConfig{Adapters: map[string]core.AdapterConfig{}}
	adapters := objectValue(t, root.M["adapters"])
	for adapterID, adapterValue := range adapters.M {
		adapterObject := objectValue(t, adapterValue)
		adapter := core.AdapterConfig{Categories: map[string]core.CategoryConfig{}}
		if value, ok := adapterObject.M["root"].(string); ok {
			adapter.Root = value
		}
		categories := objectValue(t, adapterObject.M["categories"])
		for categoryName, categoryValue := range categories.M {
			categoryObject := objectValue(t, categoryValue)
			category := core.CategoryConfig{Mode: core.SyncMode(stringValue(t, categoryObject.M["mode"]))}
			paths := arrayValue(t, categoryObject.M["paths"])
			for _, pathValue := range paths {
				category.Paths = append(category.Paths, stringValue(t, pathValue))
			}
			if excluded, ok := categoryObject.M["excludeKeys"]; ok {
				for _, key := range arrayValue(t, excluded) {
					category.ExcludeKeys = append(category.ExcludeKeys, stringValue(t, key))
				}
			}
			adapter.Categories[categoryName] = category
		}
		config.Adapters[adapterID] = adapter
	}
	return config
}

func syncSnapshots(t *testing.T, value orderedjson.Value) []core.AdapterSnapshot {
	t.Helper()
	if value == nil {
		return nil
	}
	items := arrayValue(t, value)
	out := make([]core.AdapterSnapshot, 0, len(items))
	for _, item := range items {
		adapterObject := objectValue(t, item)
		adapterID := stringValue(t, adapterObject.M["adapterId"])
		adapter := core.AdapterSnapshot{AdapterID: adapterID, Categories: []core.CategorySnapshot{}}
		for _, categoryValue := range arrayValue(t, adapterObject.M["categories"]) {
			categoryObject := objectValue(t, categoryValue)
			category := core.CategorySnapshot{AdapterID: adapterID, Category: stringValue(t, categoryObject.M["category"]), Mode: core.SyncMode(stringValue(t, categoryObject.M["mode"])), Files: core.SnapshotFiles{}}
			files := objectValue(t, categoryObject.M["files"])
			for relPath, entryValue := range files.M {
				entryObject := objectValue(t, entryValue)
				category.Files[relPath] = core.SnapshotEntry{Kind: stringValue(t, entryObject.M["kind"]), Content: stringValue(t, entryObject.M["content"])}
			}
			adapter.Categories = append(adapter.Categories, category)
		}
		out = append(out, adapter)
	}
	return out
}

func actionValues(t *testing.T, value orderedjson.Value) []*orderedjson.Object {
	t.Helper()
	values := arrayValue(t, value)
	out := make([]*orderedjson.Object, 0, len(values))
	for _, value := range values {
		out = append(out, objectValue(t, value))
	}
	return out
}

func objectValue(t *testing.T, value orderedjson.Value) *orderedjson.Object {
	t.Helper()
	object, ok := value.(*orderedjson.Object)
	if !ok || object == nil {
		t.Fatalf("value %T is not an object", value)
	}
	return object
}

func arrayValue(t *testing.T, value orderedjson.Value) []orderedjson.Value {
	t.Helper()
	array, ok := value.([]orderedjson.Value)
	if !ok {
		t.Fatalf("value %T is not an array", value)
	}
	return array
}

func stringValue(t *testing.T, value orderedjson.Value) string {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		t.Fatalf("value %T is not a string", value)
	}
	return text
}
