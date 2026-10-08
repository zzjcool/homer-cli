package keyring

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

// A file the user already encrypted must be reported relative to its
// adapter root, so the scanner can stop collecting the plaintext copy.
func TestEncryptedIgnoresRelativeToAdapterRoot(t *testing.T) {
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
	for _, name := range []string{"web-search.json", "auth.json", "other.json"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(`{"k":"v"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if r := Apply(homerHome, Command{Action: "create", ID: "pi", Name: "pi", Password: "password-1234", WorkFactor: 10}); !r.OK {
		t.Fatalf("create: %+v", r)
	}
	for _, name := range []string{"web-search.json", "auth.json"} {
		r := Apply(homerHome, Command{Action: "encrypt", ID: "pi", Path: "~/.pi/agent/" + name, Adapter: "pi", Password: "password-1234"})
		if !r.OK {
			t.Fatalf("encrypt %s: %+v", name, r)
		}
	}
	// An unbound file and a file bound to another adapter must not leak in.
	if r := Apply(homerHome, Command{Action: "encrypt", ID: "pi", Path: "~/.pi/agent/other.json", Password: "password-1234"}); !r.OK {
		t.Fatalf("encrypt other: %+v", r)
	}
	got := EncryptedIgnores(homerHome, "pi", "~/.pi/agent")
	want := []string{"auth.json", "web-search.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pi ignores = %v, want %v", got, want)
	}
	if other := EncryptedIgnores(homerHome, "opencode", "~/.config/opencode"); len(other) != 0 {
		t.Fatalf("opencode must get nothing, got %v", other)
	}
	// A destination outside the adapter root has no relative path.
	if outside := EncryptedIgnores(homerHome, "pi", "~/elsewhere"); len(outside) != 0 {
		t.Fatalf("outside root = %v", outside)
	}
	if none := EncryptedIgnores(homerHome, "", "~/.pi/agent"); none != nil {
		t.Fatalf("empty adapter = %v", none)
	}
}
