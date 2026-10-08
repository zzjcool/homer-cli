package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/keyring"
)

// End to end: once a file is encrypted and bound to the adapter, the local
// scan no longer contains its plaintext, so neither the secret scanner nor
// the drift count can see it. The caller's config is left untouched.
func TestWithEncryptedIgnoresHidesPlaintextFromScan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	homerHome := t.TempDir()
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return homerHome
		}
		return os.Getenv(key)
	})
	if err := core.SaveConfig(paths, core.HomerConfig{Version: 1}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"web-search.json": `{"braveApiKey":"abc"}`, "keep.json": `{"a":1}`} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := core.AdapterConfig{
		Root: "~/.pi/agent",
		Categories: map[string]core.CategoryConfig{
			"files": {Paths: []string{"./"}, Mode: core.SyncModeMirror},
		},
	}
	files := func(c core.AdapterConfig) map[string]bool {
		out := map[string]bool{}
		for _, category := range adapter.ScanAdapter("pi", c).Snapshot.Categories {
			for path := range category.Files {
				out[path] = true
			}
		}
		return out
	}
	if before := files(config); !before["web-search.json"] || !before["keep.json"] {
		t.Fatalf("setup scan = %v", before)
	}
	if r := keyring.Apply(homerHome, keyring.Command{Action: "create", ID: "pi", Name: "pi", Password: "password-1234", WorkFactor: 10}); !r.OK {
		t.Fatalf("create: %+v", r)
	}
	if r := keyring.Apply(homerHome, keyring.Command{Action: "encrypt", ID: "pi", Path: "~/.pi/agent/web-search.json", Adapter: "pi", Password: "password-1234"}); !r.OK {
		t.Fatalf("encrypt: %+v", r)
	}
	after := files(WithEncryptedIgnores(homerHome, "pi", config))
	if after["web-search.json"] {
		t.Fatalf("encrypted file still scanned in plaintext: %v", after)
	}
	if !after["keep.json"] {
		t.Fatalf("unrelated file must stay: %v", after)
	}
	if len(config.Ignore) != 0 {
		t.Fatalf("caller config mutated: %v", config.Ignore)
	}
}
