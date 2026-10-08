package keyring

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExistsReportsRegularFilesOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := Apply(t.TempDir(), Command{Action: "exists", Paths: []string{"~/auth.json", "~/missing.json", "~/dir"}})
	if !got.OK || !got.Exists["~/auth.json"] || got.Exists["~/missing.json"] || got.Exists["~/dir"] {
		t.Fatalf("exists = %+v", got)
	}
	if bad := Apply(t.TempDir(), Command{Action: "exists", Paths: []string{"relative/x"}}); bad.OK {
		t.Fatal("relative paths must be rejected")
	}
	if bad := Apply(t.TempDir(), Command{Action: "exists", Paths: []string{"~/../etc/passwd"}}); bad.OK {
		t.Fatal(".. must be rejected")
	}
}
