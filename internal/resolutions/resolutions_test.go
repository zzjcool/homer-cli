package resolutions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/core"
)

const testRecordedAt = "2026-10-20T08:00:00Z"

func testPaths(t *testing.T) core.HomerPaths {
	t.Helper()
	return core.HomerPaths{Home: t.TempDir()}
}

func testEntry(adapter, choice, recordedAt string, generation int) Entry {
	return Entry{
		Adapter:            adapter,
		Choice:             choice,
		RecordedAt:         recordedAt,
		GenerationAtRecord: generation,
	}
}

func fixedNow(value string) func() string { return func() string { return value } }

func TestPathAndValidationHelpers(t *testing.T) {
	paths := core.HomerPaths{Home: "/tmp/homer-test"}
	if got, want := Path(paths), filepath.Join(paths.Home, FileName); got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
	if !ValidChoice(ChoiceCenter) || !ValidChoice(ChoiceLocal) || ValidChoice("other") {
		t.Fatal("ValidChoice did not recognize exactly the supported choices")
	}

	valid := testEntry("pi", ChoiceCenter, testRecordedAt, 1)
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	invalid := []Entry{
		testEntry("../x", ChoiceCenter, testRecordedAt, 1),
		testEntry("pi", "other", testRecordedAt, 1),
		testEntry("pi", ChoiceCenter, testRecordedAt, 0),
		testEntry("pi", ChoiceCenter, "not-a-time", 1),
	}
	for _, entry := range invalid {
		if err := entry.Validate(); err == nil {
			t.Errorf("Validate(%#v) unexpectedly succeeded", entry)
		}
	}

	if !valid.Same(testEntry("pi", ChoiceCenter, testRecordedAt, 1)) {
		t.Fatal("Same() rejected identical entries")
	}
	for _, other := range []Entry{
		testEntry("vscode", ChoiceCenter, testRecordedAt, 1),
		testEntry("pi", ChoiceLocal, testRecordedAt, 1),
		testEntry("pi", ChoiceCenter, "2026-10-21T08:00:00Z", 1),
		testEntry("pi", ChoiceCenter, testRecordedAt, 2),
	} {
		if valid.Same(other) {
			t.Errorf("Same() treated %#v as identical to %#v", valid, other)
		}
	}

	indexed := Index([]Entry{
		testEntry("pi", ChoiceCenter, testRecordedAt, 1),
		testEntry("vscode", ChoiceLocal, testRecordedAt, 1),
		testEntry("pi", ChoiceLocal, "2026-10-21T08:00:00Z", 2),
	})
	if len(indexed) != 2 || indexed["pi"].Choice != ChoiceLocal || indexed["vscode"].Choice != ChoiceLocal {
		t.Fatalf("Index() = %#v, want last entry per adapter", indexed)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	paths := testPaths(t)
	entries := []Entry{
		testEntry("zeta", ChoiceLocal, "2026-10-21T08:00:00Z", 8),
		testEntry("pi", ChoiceCenter, testRecordedAt, 7),
	}
	if err := Save(paths, File{Version: FileVersion, Entries: entries}); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	data, err := os.ReadFile(Path(paths))
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	wantJSON := `{"version":1,"entries":[{"adapter":"pi","choice":"center","recordedAt":"2026-10-20T08:00:00Z","generationAtRecord":7},{"adapter":"zeta","choice":"local","recordedAt":"2026-10-21T08:00:00Z","generationAtRecord":8}]}`
	if string(data) != wantJSON {
		t.Fatalf("saved JSON = %s, want %s", data, wantJSON)
	}

	got, err := Load(paths)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	want := File{Version: FileVersion, Entries: []Entry{entries[1], entries[0]}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestSaveMode0600AndAtomic(t *testing.T) {
	paths := testPaths(t)
	file := File{Version: FileVersion, Entries: []Entry{testEntry("pi", ChoiceCenter, testRecordedAt, 1)}}
	if err := Save(paths, file); err != nil {
		t.Fatalf("first Save(): %v", err)
	}
	filename := Path(paths)
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatalf("Stat(): %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("initial file mode = %04o, want 0600", got)
	}
	if err := os.Chmod(filename, 0o644); err != nil {
		t.Fatalf("Chmod pre-existing file: %v", err)
	}
	file.Entries[0].Choice = ChoiceLocal
	if err := Save(paths, file); err != nil {
		t.Fatalf("replacement Save(): %v", err)
	}
	info, err = os.Stat(filename)
	if err != nil {
		t.Fatalf("Stat() after replacement: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("replacement file mode = %04o, want 0600", got)
	}
	items, err := os.ReadDir(paths.Home)
	if err != nil {
		t.Fatalf("ReadDir(): %v", err)
	}
	for _, item := range items {
		if strings.HasPrefix(item.Name(), FileName+".tmp-") {
			t.Errorf("temporary file remained after Save(): %q", item.Name())
		}
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	got, err := Load(testPaths(t))
	if err != nil {
		t.Fatalf("Load(missing): %v", err)
	}
	if got.Version != FileVersion || got.Entries == nil || len(got.Entries) != 0 {
		t.Fatalf("Load(missing) = %#v, want version %d with non-nil empty entries", got, FileVersion)
	}
}

func TestLoadCorruptReturnsErrCorrupt(t *testing.T) {
	paths := testPaths(t)
	if err := os.WriteFile(Path(paths), []byte(`{"version":1,"entries":[`), 0o600); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	if _, err := Load(paths); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Load(corrupt) error = %v, want ErrCorrupt", err)
	}
}

func TestLoadUnsupportedVersion(t *testing.T) {
	paths := testPaths(t)
	for _, data := range []string{
		`{"version":2,"entries":[]}`,
		`{"version":99}`,
	} {
		if err := os.WriteFile(Path(paths), []byte(data), 0o600); err != nil {
			t.Fatalf("WriteFile(): %v", err)
		}
		if _, err := Load(paths); !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("Load(%q) error = %v, want ErrUnsupportedVersion", data, err)
		}
	}
}

func TestLoadMalformedVersionIsCorrupt(t *testing.T) {
	paths := testPaths(t)
	if err := os.WriteFile(Path(paths), []byte(`{"version":null,"entries":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	if _, err := Load(paths); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Load(null version) error = %v, want ErrCorrupt", err)
	}
}

func TestRecordQuarantinesCorruptFile(t *testing.T) {
	paths := testPaths(t)
	filename := Path(paths)
	bad := []byte("{bad json")
	if err := os.WriteFile(filename, bad, 0o600); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	result, err := Record(paths, ChoiceCenter, []string{"pi"}, 4, fixedNow(testRecordedAt))
	if err != nil {
		t.Fatalf("Record() after corruption: %v", err)
	}
	if len(result.Recorded) != 1 || result.Recorded[0].Adapter != "pi" {
		t.Fatalf("Record() result = %#v", result)
	}
	items, err := os.ReadDir(paths.Home)
	if err != nil {
		t.Fatalf("ReadDir(): %v", err)
	}
	quarantines := 0
	for _, item := range items {
		if strings.HasPrefix(item.Name(), FileName+".corrupt-") {
			quarantines++
			got, err := os.ReadFile(filepath.Join(paths.Home, item.Name()))
			if err != nil {
				t.Fatalf("read quarantine %q: %v", item.Name(), err)
			}
			if string(got) != string(bad) {
				t.Errorf("quarantined bytes = %q, want %q", got, bad)
			}
		}
	}
	if quarantines != 1 {
		t.Fatalf("quarantine files = %d, want 1", quarantines)
	}
	if _, err := os.Stat(filename); err != nil {
		t.Fatalf("new resolutions file missing: %v", err)
	}
}

func TestRecordRefusesUnsupportedVersion(t *testing.T) {
	paths := testPaths(t)
	filename := Path(paths)
	original := []byte(`{"version":99,"entries":[]}`)
	if err := os.WriteFile(filename, original, 0o600); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	if _, err := Record(paths, ChoiceLocal, []string{"pi"}, 3, fixedNow(testRecordedAt)); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("Record() error = %v, want ErrUnsupportedVersion", err)
	}
	got, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("ReadFile() after rejected Record(): %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("unsupported-version file changed: got %q, want %q", got, original)
	}
}

func TestLoadDropsInvalidEntries(t *testing.T) {
	paths := testPaths(t)
	data := `{"version":1,"entries":[` +
		`{"adapter":"pi","choice":"center","recordedAt":"2026-10-20T08:00:00Z","generationAtRecord":1},` +
		`{"adapter":"../x","choice":"center","recordedAt":"2026-10-20T08:00:00Z","generationAtRecord":1},` +
		`{"adapter":"vscode","choice":"bad","recordedAt":"2026-10-20T08:00:00Z","generationAtRecord":1},` +
		`{"adapter":"zed","choice":"local","recordedAt":"2026-10-20T08:00:00Z","generationAtRecord":0},` +
		`{"adapter":"cursor","choice":"local","recordedAt":"yesterday","generationAtRecord":1},` +
		`{"adapter":"broken","choice":"local","recordedAt":"2026-10-21T08:00:00Z","generationAtRecord":"two"},` +
		`{"adapter":"pi","choice":"local","recordedAt":"2026-10-21T08:00:00Z","generationAtRecord":2}]}`
	if err := os.WriteFile(Path(paths), []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	got, err := Load(paths)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	want := File{Version: FileVersion, Entries: []Entry{testEntry("pi", ChoiceLocal, "2026-10-21T08:00:00Z", 2)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestRecordOverwritesSameAdapter(t *testing.T) {
	paths := testPaths(t)
	first, err := Record(paths, ChoiceCenter, []string{"pi", "vscode"}, 2, fixedNow(testRecordedAt))
	if err != nil {
		t.Fatalf("first Record(): %v", err)
	}
	if len(first.Recorded) != 2 || len(first.Replaced) != 0 {
		t.Fatalf("first Record() = %#v", first)
	}
	secondAt := "2026-10-21T09:10:11Z"
	second, err := Record(paths, ChoiceLocal, []string{"pi"}, 3, fixedNow(secondAt))
	if err != nil {
		t.Fatalf("second Record(): %v", err)
	}
	if !reflect.DeepEqual(second.Recorded, []Entry{testEntry("pi", ChoiceLocal, secondAt, 3)}) {
		t.Fatalf("second Recorded = %#v", second.Recorded)
	}
	if !reflect.DeepEqual(second.Replaced, []Entry{testEntry("pi", ChoiceCenter, testRecordedAt, 2)}) {
		t.Fatalf("second Replaced = %#v", second.Replaced)
	}
	got, err := Load(paths)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	want := File{Version: FileVersion, Entries: []Entry{
		testEntry("pi", ChoiceLocal, secondAt, 3),
		testEntry("vscode", ChoiceCenter, testRecordedAt, 2),
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() after overwrite = %#v, want %#v", got, want)
	}
}

func TestRecordRejectsInvalid(t *testing.T) {
	tests := []struct {
		name       string
		choice     string
		adapters   []string
		generation int
		now        func() string
	}{
		{name: "empty adapters", choice: ChoiceCenter, generation: 1, now: fixedNow(testRecordedAt)},
		{name: "bad choice", choice: "other", adapters: []string{"pi"}, generation: 1, now: fixedNow(testRecordedAt)},
		{name: "zero generation", choice: ChoiceCenter, adapters: []string{"pi"}, generation: 0, now: fixedNow(testRecordedAt)},
		{name: "invalid adapter", choice: ChoiceCenter, adapters: []string{"../x"}, generation: 1, now: fixedNow(testRecordedAt)},
		{name: "invalid timestamp", choice: ChoiceCenter, adapters: []string{"pi"}, generation: 1, now: fixedNow("today")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := testPaths(t)
			if _, err := Record(paths, test.choice, test.adapters, test.generation, test.now); err == nil {
				t.Fatal("Record() unexpectedly succeeded")
			}
			if _, err := os.Stat(Path(paths)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid Record() created a file (stat error %v)", err)
			}
		})
	}
}

func TestRecordDefaultsTimestampWhenNowIsNil(t *testing.T) {
	paths := testPaths(t)
	before := time.Now().UTC().Add(-time.Second)
	if _, err := Record(paths, ChoiceCenter, []string{"pi"}, 1, nil); err != nil {
		t.Fatalf("Record(now=nil): %v", err)
	}
	file, err := Load(paths)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(file.Entries) != 1 {
		t.Fatalf("entries = %#v", file.Entries)
	}
	recorded, err := time.Parse(time.RFC3339, file.Entries[0].RecordedAt)
	if err != nil || recorded.Before(before) || recorded.After(time.Now().Add(time.Second)) {
		t.Fatalf("recordedAt = %q, parsed %v, err %v", file.Entries[0].RecordedAt, recorded, err)
	}
}

func TestClearRemovesSelectedAdapters(t *testing.T) {
	paths := testPaths(t)
	entries := []Entry{
		testEntry("pi", ChoiceCenter, testRecordedAt, 1),
		testEntry("vscode", ChoiceLocal, testRecordedAt, 1),
		testEntry("zed", ChoiceCenter, testRecordedAt, 1),
	}
	if err := Save(paths, File{Version: FileVersion, Entries: entries}); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	removed, err := Clear(paths, []string{"vscode", "unknown"})
	if err != nil {
		t.Fatalf("Clear(): %v", err)
	}
	if !reflect.DeepEqual(removed, []Entry{entries[1]}) {
		t.Fatalf("Clear() removed = %#v", removed)
	}
	file, err := Load(paths)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !reflect.DeepEqual(file.Entries, []Entry{entries[0], entries[2]}) {
		t.Fatalf("remaining entries = %#v", file.Entries)
	}
	removed, err = Clear(paths, []string{"unknown"})
	if err != nil || len(removed) != 0 {
		t.Fatalf("Clear(unknown) = %#v, %v", removed, err)
	}
	removed, err = Clear(paths, nil)
	if err != nil || removed == nil || len(removed) != 0 {
		t.Fatalf("Clear(empty) = %#v, %v, want non-nil empty", removed, err)
	}
}

func TestRemoveIfSameCAS(t *testing.T) {
	paths := testPaths(t)
	original := []Entry{
		testEntry("pi", ChoiceCenter, testRecordedAt, 1),
		testEntry("vscode", ChoiceLocal, testRecordedAt, 1),
	}
	if err := Save(paths, File{Version: FileVersion, Entries: original}); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	newPi := testEntry("pi", ChoiceLocal, "2026-10-21T08:00:00Z", 2)
	if err := Save(paths, File{Version: FileVersion, Entries: []Entry{newPi, original[1]}}); err != nil {
		t.Fatalf("Save(re-recorded): %v", err)
	}
	removed, err := RemoveIfSame(paths, []Entry{original[0], original[1]})
	if err != nil {
		t.Fatalf("RemoveIfSame(): %v", err)
	}
	if !reflect.DeepEqual(removed, []Entry{original[1]}) {
		t.Fatalf("RemoveIfSame() removed = %#v, want only matching vscode entry", removed)
	}
	file, err := Load(paths)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !reflect.DeepEqual(file.Entries, []Entry{newPi}) {
		t.Fatalf("CAS remaining entries = %#v, want %#v", file.Entries, []Entry{newPi})
	}
	removed, err = RemoveIfSame(paths, nil)
	if err != nil || removed == nil || len(removed) != 0 {
		t.Fatalf("RemoveIfSame(empty) = %#v, %v, want non-nil empty", removed, err)
	}
}

func TestEmptyFileRemoved(t *testing.T) {
	paths := testPaths(t)
	filename := Path(paths)
	entry := testEntry("pi", ChoiceCenter, testRecordedAt, 1)
	if err := Save(paths, File{Version: FileVersion, Entries: []Entry{entry}}); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	if err := Save(paths, File{Version: FileVersion, Entries: []Entry{}}); err != nil {
		t.Fatalf("Save(empty): %v", err)
	}
	if _, err := os.Stat(filename); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Save(empty) did not remove the file: stat error %v", err)
	}
	if err := Save(paths, File{Version: FileVersion, Entries: []Entry{entry}}); err != nil {
		t.Fatalf("Save(before Clear): %v", err)
	}
	removed, err := Clear(paths, []string{"pi"})
	if err != nil || !reflect.DeepEqual(removed, []Entry{entry}) {
		t.Fatalf("Clear(all) = %#v, %v", removed, err)
	}
	if _, err := os.Stat(filename); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Clear(all) did not remove the file: stat error %v", err)
	}
}

func TestStaleTable(t *testing.T) {
	tests := []struct {
		name             string
		entryGeneration  int
		centerGeneration int
		want             bool
	}{
		{name: "same", entryGeneration: 7, centerGeneration: 7, want: false},
		{name: "different", entryGeneration: 7, centerGeneration: 8, want: true},
		{name: "unknown center", entryGeneration: 7, centerGeneration: 0, want: true},
		{name: "negative center", entryGeneration: 7, centerGeneration: -1, want: true},
		{name: "invalid entry generation", entryGeneration: 0, centerGeneration: 7, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry := testEntry("pi", ChoiceCenter, testRecordedAt, test.entryGeneration)
			if got := Stale(entry, test.centerGeneration); got != test.want {
				t.Fatalf("Stale(%#v, %d) = %t, want %t", entry, test.centerGeneration, got, test.want)
			}
		})
	}
}

func TestConcurrentRecord(t *testing.T) {
	paths := testPaths(t)
	const count = 50
	var wait sync.WaitGroup
	errCh := make(chan error, count)
	for i := 0; i < count; i++ {
		i := i
		wait.Add(1)
		go func() {
			defer wait.Done()
			adapter := fmt.Sprintf("adapter-%02d", i)
			choice := ChoiceCenter
			if i%2 != 0 {
				choice = ChoiceLocal
			}
			if _, err := Record(paths, choice, []string{adapter}, 1, fixedNow(testRecordedAt)); err != nil {
				errCh <- fmt.Errorf("Record(%s): %w", adapter, err)
			}
		}()
	}
	wait.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	file, err := Load(paths)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(file.Entries) != count {
		t.Fatalf("concurrent Record() left %d entries, want %d", len(file.Entries), count)
	}
	for i, entry := range file.Entries {
		want := fmt.Sprintf("adapter-%02d", i)
		if entry.Adapter != want {
			t.Fatalf("entry[%d].Adapter = %q, want %q", i, entry.Adapter, want)
		}
	}
}

func TestStoreErrors(t *testing.T) {
	t.Run("Save rejects unsupported version", func(t *testing.T) {
		if err := Save(testPaths(t), File{Version: FileVersion + 1}); !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("Save() error = %v, want ErrUnsupportedVersion", err)
		}
	})
	t.Run("Save rejects invalid entries", func(t *testing.T) {
		if err := Save(testPaths(t), File{Version: FileVersion, Entries: []Entry{testEntry("../x", ChoiceCenter, testRecordedAt, 1)}}); err == nil {
			t.Fatal("Save() accepted an invalid entry")
		}
	})
	t.Run("Load propagates filesystem errors", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(parent, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(core.HomerPaths{Home: parent}); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Load() error = %v, want a non-ENOENT filesystem error", err)
		}
	})
	t.Run("Save propagates directory errors", func(t *testing.T) {
		if err := Save(core.HomerPaths{Home: "/dev/null"}, File{
			Version: FileVersion,
			Entries: []Entry{testEntry("pi", ChoiceCenter, testRecordedAt, 1)},
		}); err == nil {
			t.Fatal("Save() into /dev/null unexpectedly succeeded")
		}
	})
	t.Run("Clear propagates unsupported version", func(t *testing.T) {
		paths := testPaths(t)
		if err := os.WriteFile(Path(paths), []byte(`{"version":2,"entries":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Clear(paths, []string{"pi"}); !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("Clear() error = %v, want ErrUnsupportedVersion", err)
		}
	})
	t.Run("RemoveIfSame propagates corrupt file", func(t *testing.T) {
		paths := testPaths(t)
		if err := os.WriteFile(Path(paths), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := RemoveIfSame(paths, []Entry{testEntry("pi", ChoiceCenter, testRecordedAt, 1)}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("RemoveIfSame() error = %v, want ErrCorrupt", err)
		}
	})
}
