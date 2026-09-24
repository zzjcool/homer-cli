package backup

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

func TestBackupLayoutPruneAndPermissions(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(string) string { return home })
	source := filepath.Join(home, "source", "secret.txt")
	if err := os.MkdirAll(filepath.Dir(source), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("secret-v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := BackupFiles(paths, "pull", []BackupTarget{{SourceAbs: source, Label: "pi/settings/secret.txt"}}, BackupFilesOptions{Mode: BackupFileModes{Dir: 0o700, File: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(result.BackupDir)[:6] == "" || filepath.Base(result.BackupDir)[6:] != "-pull" {
		t.Fatalf("backup dir = %q", result.BackupDir)
	}
	backupFile := filepath.Join(result.BackupDir, "pi", "settings", "secret.txt")
	if data, err := os.ReadFile(backupFile); err != nil || string(data) != "secret-v1\n" {
		t.Fatalf("backup content = %q, err=%v", data, err)
	}
	for _, name := range []string{paths.BackupsDir, filepath.Dir(result.BackupDir), result.BackupDir, filepath.Dir(backupFile)} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %s mode = %o", name, info.Mode().Perm())
		}
	}
	if info, err := os.Stat(backupFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup file mode = %v, err=%v", info.Mode().Perm(), err)
	}

	missing, err := BackupFiles(paths, "pull", []BackupTarget{{SourceAbs: filepath.Join(home, "nope"), Label: "pi/nope"}})
	if err != nil || len(missing.Skipped) != 1 || len(missing.BackedUp) != 0 {
		t.Fatalf("missing target result = %#v, err=%v", missing, err)
	}
	if _, err := BackupFiles(paths, "pull", []BackupTarget{{SourceAbs: source, Label: "../escape"}}); err == nil {
		t.Fatal("expected unsafe label error")
	}

	for _, date := range []string{"20000101", "20000102", "20000103"} {
		if err := os.MkdirAll(filepath.Join(paths.BackupsDir, date, "120000-pull"), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	pruned, err := PruneBackups(paths, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned.Removed) != 2 || len(pruned.Kept) != 2 || pruned.Removed[0] != "20000102" || pruned.Removed[1] != "20000101" {
		t.Fatalf("prune result = %#v", pruned)
	}
}

func TestOrdinaryBackupDoesNotForceSecurityMode(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(string) string { return home })
	source := filepath.Join(home, "source.txt")
	if err := os.WriteFile(source, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := BackupFiles(paths, "pull", []BackupTarget{{SourceAbs: source, Label: "x.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(result.BackupDir, "x.txt"))
	if err != nil {
		t.Fatal(err)
	}
	oldUmask := syscall.Umask(0)
	syscall.Umask(oldUmask)
	expected := os.FileMode(0o666 &^ oldUmask)
	if info.Mode().Perm() != expected {
		t.Fatalf("ordinary backup mode = %o, want default %o", info.Mode().Perm(), expected)
	}
}
