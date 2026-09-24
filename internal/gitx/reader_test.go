package gitx

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

func readerConfig() *core.HomerConfig {
	return &core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"pi": {
				Root:    "/tmp/pi",
				Enabled: boolPtr(true),
				Categories: map[string]core.CategoryConfig{
					"empty":    {Paths: []string{"empty/"}, Mode: core.SyncModeMirror},
					"notes":    {Paths: []string{"notes/"}, Mode: core.SyncModeMirror},
					"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMerge},
					"disabled": {Paths: []string{"disabled/"}, Mode: core.SyncModeMirror, Enabled: boolPtr(false)},
				},
			},
			"other": {
				Root:    "/tmp/other",
				Enabled: boolPtr(true),
				Categories: map[string]core.CategoryConfig{
					"data": {Paths: []string{"data/"}, Mode: core.SyncModeMirror},
				},
			},
			"off": {
				Root:    "/tmp/off",
				Enabled: boolPtr(false),
				Categories: map[string]core.CategoryConfig{
					"ignored": {Paths: []string{"ignored/"}, Mode: core.SyncModeMirror},
				},
			},
		},
	}
}

func boolPtr(value bool) *bool { return &value }

func TestReadStoreSnapshotAtCommitRoundTrip(t *testing.T) {
	home := testRepo(t)
	writeTestFile(t, filepath.Join(home, "store", "pi", ".homer-complete"), "")
	writeTestFile(t, filepath.Join(home, "store", "pi", "settings", "settings.json"), "{\n  \"model\": \"sonnet\"\n}\n")
	writeTestFile(t, filepath.Join(home, "store", "pi", "settings", "broken.json"), "not json\n")
	writeTestFile(t, filepath.Join(home, "store", "pi", "notes", "foo", "SKILL.md"), "# foo\n")
	writeTestFile(t, filepath.Join(home, "store", "pi", "notes", "foo", "nested", "note.md"), "nested\n")
	// The empty category is represented by its directory in a real store
	// writer, but no blob is needed for the git reader to return an empty map.
	if err := os.MkdirAll(filepath.Join(home, "store", "pi", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	first := CommitAllStore(home, "snapshot one")
	if first == "" {
		t.Fatal("snapshot commit failed")
	}

	cfg := readerConfig()
	got := ReadStoreSnapshotAtCommit(core.HomerPaths{Home: home}, cfg, first)
	want := []core.AdapterSnapshot{
		{
			AdapterID: "other",
			Categories: []core.CategorySnapshot{
				{AdapterID: "other", Category: "data", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{}},
			},
		},
		{
			AdapterID: "pi",
			Categories: []core.CategorySnapshot{
				{AdapterID: "pi", Category: "empty", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{}},
				{AdapterID: "pi", Category: "notes", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{
					"foo/SKILL.md":       {Kind: "file", Content: "# foo\n"},
					"foo/nested/note.md": {Kind: "file", Content: "nested\n"},
				}},
				{AdapterID: "pi", Category: "settings", Mode: core.SyncModeMerge, Files: core.SnapshotFiles{
					"broken.json":   {Kind: "file", Content: "not json\n"},
					"settings.json": {Kind: "json", Content: "{\n  \"model\": \"sonnet\"\n}\n"},
				}},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot mismatch:\n got %#v\nwant %#v", got, want)
	}
	for _, adapter := range got {
		for _, category := range adapter.Categories {
			for relPath := range category.Files {
				if relPath == ".homer-complete" {
					t.Fatal(".homer-complete leaked into snapshot")
				}
			}
		}
	}

	invalid := ReadStoreSnapshotAtCommit(core.HomerPaths{Home: home}, cfg, "not-a-real-ref")
	if len(invalid) != 2 {
		t.Fatalf("invalid ref adapter count = %d, want 2", len(invalid))
	}
	for _, adapter := range invalid {
		for _, category := range adapter.Categories {
			if len(category.Files) != 0 {
				t.Fatalf("invalid ref returned files: %#v", category.Files)
			}
		}
	}
}

func TestReadStoreSnapshotAtCommitHistoricalContentAndFiltering(t *testing.T) {
	home := testRepo(t)
	cfg := readerConfig()
	writeTestFile(t, filepath.Join(home, "store", "pi", "settings", "settings.json"), "{\"version\":1}\n")
	v1 := CommitAllStore(home, "v1")
	writeTestFile(t, filepath.Join(home, "store", "pi", "settings", "settings.json"), "{\"version\":2}\n")
	v2 := CommitAllStore(home, "v2")
	if v1 == "" || v2 == "" || v1 == v2 {
		t.Fatalf("historical commits = %q, %q", v1, v2)
	}
	old := ReadStoreSnapshotAtCommit(core.HomerPaths{Home: home}, cfg, v1)
	newer := ReadStoreSnapshotAtCommit(core.HomerPaths{Home: home}, cfg, v2)
	oldContent := old[1].Categories[2].Files["settings.json"].Content
	newContent := newer[1].Categories[2].Files["settings.json"].Content
	if oldContent != "{\"version\":1}\n" || newContent != "{\"version\":2}\n" {
		t.Fatalf("historical content = %q / %q", oldContent, newContent)
	}
	if len(old[1].Categories) != 3 || len(old[0].Categories) != 1 {
		t.Fatalf("enabled filtering/categories mismatch: %#v", old)
	}
}

func TestReadVaultFileAtCommitPreservesBinaryAndEmptyBlob(t *testing.T) {
	home := testRepo(t)
	binary := []byte{0x00, 0xff, 0x80, 0x41, 0x0a, 0xc3, 0x28, 0x00, 0xfe}
	filename := filepath.Join(home, "secrets", "binary.age")
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, binary, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := CommitPaths(home, []string{"secrets/"}, "binary"); got == "" {
		t.Fatal("binary secret commit failed")
	}
	got, err := ReadVaultFileAtCommit(home, "secrets/binary.age", "HEAD")
	if err != nil {
		t.Fatalf("read binary blob: %v", err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("binary blob changed: %v != %v", got, binary)
	}

	empty := filepath.Join(home, "secrets", "empty.age")
	if err := os.WriteFile(empty, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := CommitPaths(home, []string{"secrets/"}, "empty"); got == "" {
		t.Fatal("empty blob commit failed")
	}
	got, err = ReadVaultFileAtCommit(home, "secrets/empty.age", "HEAD")
	if err != nil {
		t.Fatalf("read empty blob: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("empty blob = %#v, want nonnil empty bytes", got)
	}
	if _, err := ReadVaultFileAtCommit(home, "secrets/missing.age", "HEAD"); err == nil {
		t.Fatal("missing blob unexpectedly succeeded")
	}
}
