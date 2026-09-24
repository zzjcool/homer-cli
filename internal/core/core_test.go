package core

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func testBool(value bool) *bool { return &value }

func testConfig() HomerConfig {
	return HomerConfig{
		Version: 1,
		Adapters: map[string]AdapterConfig{
			"pi": {
				Root: "~/.pi/agent",
				Categories: map[string]CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: SyncModeMerge},
					"skills":   {Paths: []string{"skills/"}, Mode: SyncModeMirror},
				},
			},
		},
	}
}

func TestGetHomerPathsHOMERHomeAndDerivedLayout(t *testing.T) {
	home := filepath.Join(t.TempDir(), "nested", "homer")
	paths := GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return ""
	})
	if paths.Home != home {
		t.Fatalf("Home = %q, want %q", paths.Home, home)
	}
	want := HomerPaths{
		Home:       home,
		StoreDir:   filepath.Join(home, "store"),
		ConfigFile: filepath.Join(home, "homer.json"),
		StateFile:  filepath.Join(home, "state.json"),
		BackupsDir: filepath.Join(home, "backups"),
		SecretsDir: filepath.Join(home, "secrets"),
		KeysDir:    filepath.Join(home, "keys"),
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}

	tilde := GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return "~/custom-homer"
		}
		return ""
	})
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if tilde.Home != filepath.Join(userHome, "custom-homer") {
		t.Fatalf("tilde Home = %q", tilde.Home)
	}
}

func TestValidateConfigTable(t *testing.T) {
	valid := `{"version":1,"adapters":{"pi":{"root":"~/.pi/agent","categories":{"settings":{"paths":["settings.json"],"mode":"merge"}}}},"backup":{"keep":7},"secrets":{"ignorePaths":["pi/skills/"]}}`
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"valid", valid, true},
		{"empty adapters", `{"version":1,"adapters":{}}`, true},
		{"missing version", `{"adapters":{}}`, false},
		{"wrong mode", `{"version":1,"adapters":{"pi":{"root":"x","categories":{"x":{"paths":["x"],"mode":"copy"}}}}}`, false},
		{"empty paths", `{"version":1,"adapters":{"pi":{"root":"x","categories":{"x":{"paths":[],"mode":"merge"}}}}}`, false},
		{"non string path", `{"version":1,"adapters":{"pi":{"root":"x","categories":{"x":{"paths":[1],"mode":"merge"}}}}}`, false},
		{"root not string", `{"version":1,"adapters":{"pi":{"root":1,"categories":{}}}}`, false},
		{"allow escape star", `{"version":1,"adapters":{"pi":{"root":"x","allowEscape":["**/"],"categories":{}}}}`, false},
		{"allow escape specific", `{"version":1,"adapters":{"pi":{"root":"x","allowEscape":["skills/browser/"],"categories":{}}}}`, true},
		{"backup zero", `{"version":1,"adapters":{},"backup":{"keep":0}}`, false},
		{"backup fraction", `{"version":1,"adapters":{},"backup":{"keep":1.5}}`, false},
		{"backup string", `{"version":1,"adapters":{},"backup":{"keep":"7"}}`, false},
		{"ignore paths wrong", `{"version":1,"adapters":{},"secrets":{"ignorePaths":["ok",2]}}`, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			config, validationErrors := ValidateConfig([]byte(test.raw))
			if (config != nil) != test.ok {
				t.Fatalf("config = %#v, errors = %v, want ok=%v", config, validationErrors, test.ok)
			}
			if test.ok && len(validationErrors) != 0 {
				t.Fatalf("valid config errors = %v", validationErrors)
			}
			if !test.ok && len(validationErrors) == 0 {
				t.Fatal("invalid config returned no errors")
			}
		})
	}
}

func TestConfigRoundTripAndMissingSentinel(t *testing.T) {
	paths := GetHomerPaths(func(string) string { return t.TempDir() })
	config := testConfig()
	config.Backup = &BackupConfig{Keep: intPtr(4)}
	config.Secrets = &SecretsConfig{IgnorePaths: []string{"pi/"}, Recipients: []string{}, Files: map[string]string{}}
	if err := SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, &config) {
		t.Fatalf("config roundtrip = %#v, want %#v", got, config)
	}
	if _, err := LoadConfig(GetHomerPaths(func(string) string { return t.TempDir() })); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing config error = %v, want os.ErrNotExist", err)
	}
}

func TestStateRoundTripAndCorruptionFallback(t *testing.T) {
	home := t.TempDir()
	paths := GetHomerPaths(func(string) string { return home })
	state := HomerState{Version: 1, LastSyncCommit: "abc", LastSyncAt: "2026-01-02T03:04:05Z", LastSyncCommand: "push"}
	if err := SaveState(paths, state); err != nil {
		t.Fatal(err)
	}
	if got := LoadState(paths); !reflect.DeepEqual(got, state) {
		t.Fatalf("state roundtrip = %#v, want %#v", got, state)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(home, "*.tmp-*")); len(leftovers) != 0 {
		t.Fatalf("temporary state files remain: %v", leftovers)
	}
	if err := os.WriteFile(paths.StateFile, []byte(`{"version":1,"lastSyncCommit"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadState(paths); !reflect.DeepEqual(got, HomerState{Version: 1}) {
		t.Fatalf("corrupt state = %#v", got)
	}
	if err := os.WriteFile(paths.StateFile, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadState(paths); !reflect.DeepEqual(got, HomerState{Version: 1}) {
		t.Fatalf("non-object state = %#v", got)
	}
}

func TestStoreRoundTripClearsAdapterAndDerivesKind(t *testing.T) {
	paths := GetHomerPaths(func(string) string { return t.TempDir() })
	config := testConfig()
	first := AdapterSnapshot{
		AdapterID: "pi",
		Categories: []CategorySnapshot{
			{AdapterID: "pi", Category: "settings", Mode: SyncModeMerge, Files: SnapshotFiles{
				"settings.json": {Kind: "json", Content: `{"model":"sonnet"}`},
			}},
			{AdapterID: "pi", Category: "skills", Mode: SyncModeMirror, Files: SnapshotFiles{
				"old.md": {Kind: "file", Content: "old"},
			}},
		},
	}
	if err := WriteSnapshotToStore(paths, first); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(paths.StoreDir, "pi", "skills", "old.md")
	if _, err := os.Stat(stale); err != nil {
		t.Fatal(err)
	}
	second := AdapterSnapshot{AdapterID: "pi", Categories: []CategorySnapshot{{
		AdapterID: "pi", Category: "settings", Mode: SyncModeMerge, Files: SnapshotFiles{
			"settings.json": {Kind: "file", Content: `{"next":true}`},
		},
	}}}
	if err := WriteSnapshotToStore(paths, second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file stat = %v", err)
	}
	got, err := ReadSnapshotFromStore(paths, config)
	if err != nil {
		t.Fatal(err)
	}
	settings := got[0].Categories[0]
	if settings.Files["settings.json"].Kind != "json" {
		t.Fatalf("kind = %q, want json", settings.Files["settings.json"].Kind)
	}
	if got[0].Categories[1].Files == nil || len(got[0].Categories[1].Files) != 0 {
		t.Fatalf("missing category should be empty: %#v", got[0].Categories[1].Files)
	}

	if err := WriteSnapshotToStore(paths, AdapterSnapshot{AdapterID: "pi", Categories: []CategorySnapshot{{
		AdapterID: "pi", Category: "settings", Mode: SyncModeMerge, Files: SnapshotFiles{"../escape": {Content: "x"}},
	}}}); err == nil {
		t.Fatal("expected path escape rejection")
	}
}

func TestEntryKindAndRootUnreadableGuard(t *testing.T) {
	if EntryKindFor(SyncModeMerge, `{"a":1}`) != "json" || EntryKindFor(SyncModeMirror, `{"a":1}`) != "file" || EntryKindFor(SyncModeMerge, "broken") != "file" {
		t.Fatal("entry kind semantics changed")
	}
	outcome := ScanOutcomeLike{Snapshot: AdapterSnapshot{}, Errors: []ScanProblem{{Path: "/root", Message: "missing"}}}
	if !IsRootUnreadable(outcome) {
		t.Fatal("empty snapshot with an error must be root-unreadable")
	}
	outcome.Snapshot.Categories = []CategorySnapshot{{}}
	if IsRootUnreadable(outcome) {
		t.Fatal("non-empty snapshot must not be root-unreadable")
	}
}

func intPtr(value int) *int { return &value }
