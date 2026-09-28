package gens

import (
	"os"
	"path/filepath"
	"testing"
)

// The hub's current-state layout (advisor ruling): immutable
// generations/<n>/{store,homer.json} plus one HEAD file naming the live
// generation. Readers only trust HEAD; writers publish a complete tree then
// flip HEAD atomically.
func TestPublishAndRead(t *testing.T) {
	home := t.TempDir()
	layout := New(home)

	if _, ok := layout.Read(); ok {
		t.Fatal("empty hub must report no generation")
	}

	store := map[string]map[string]string{
		"pi": {"settings.json": "one", "packages.manifest.txt": "npm:a\n"},
	}
	meta := []byte(`{"version":1}`)
	gen1, err := layout.Publish(store, meta)
	if err != nil || gen1 != 1 {
		t.Fatalf("publish = %v gen=%d", err, gen1)
	}

	// Read back: contents and generation.
	head, ok := layout.Read()
	if !ok || head.Generation != 1 {
		t.Fatalf("read = %v %v", head, ok)
	}
	if got := readStoreFile(t, head.StoreDir, "pi", "settings.json"); got != "one" {
		t.Fatalf("settings = %q", got)
	}
	if string(head.Meta) != string(meta) {
		t.Fatalf("meta = %s", head.Meta)
	}

	// Publish again: generation increments, old tree stays (immutability),
	// HEAD flips.
	store["pi"]["settings.json"] = "two"
	gen2, err := layout.Publish(store, meta)
	if err != nil || gen2 != 2 {
		t.Fatalf("second publish = %v gen=%d", err, gen2)
	}
	head2, _ := layout.Read()
	if head2.Generation != 2 {
		t.Fatalf("head generation = %d", head2.Generation)
	}
	if got := readStoreFile(t, head2.StoreDir, "pi", "settings.json"); got != "two" {
		t.Fatalf("new settings = %q", got)
	}
	if got := readStoreFile(t, head1Store(t, layout), "pi", "settings.json"); got != "one" {
		t.Fatalf("old generation mutated: %q", got)
	}
}

// A partially written generation (crash before HEAD flip) is invisible:
// HEAD still names the last complete generation.
func TestCrashBeforeFlipInvisible(t *testing.T) {
	home := t.TempDir()
	layout := New(home)
	_, err := layout.Publish(map[string]map[string]string{"pi": {"a": "1"}}, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: a generations/3 tree exists but HEAD still says 1.
	genDir := filepath.Join(home, "generations", "3")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "store-marker"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	head, ok := layout.Read()
	if !ok || head.Generation != 1 {
		t.Fatalf("head = %v %v (must ignore the half-written 3)", head, ok)
	}
}

func readStoreFile(t *testing.T, storeDir, adapter, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(storeDir, adapter, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func head1Store(t *testing.T, layout *Layout) string {
	t.Helper()
	return filepath.Join(layout.home, "generations", "1", "store")
}
