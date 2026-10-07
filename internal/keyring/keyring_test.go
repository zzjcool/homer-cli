package keyring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter/pi"
	"github.com/zzjcool/homer-cli/internal/core"
)

func TestEnvelopeRoundTripAndSyncShape(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	homer := filepath.Join(root, ".homer")
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return homer
		}
		return ""
	})
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			pi.PIAdapterID: pi.DefaultPIAdapter,
		},
	}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
	const password = "correct horse battery"
	const plaintext = "tencent-api-key-value\n"
	destination := filepath.Join(root, "providers.json")
	if err := os.WriteFile(destination, []byte(plaintext), 0o600); err != nil {
		t.Fatal(err)
	}

	fast := Command{WorkFactor: 14, Password: password}
	created := Apply(homer, Command{Action: "create", ID: "codebuddy", Name: "CodeBuddy", Password: password, WorkFactor: fast.WorkFactor})
	if !created.OK || created.Status != "created" {
		t.Fatalf("create = %#v", created)
	}
	again := Apply(homer, Command{Action: "create", ID: "codebuddy", Name: "CodeBuddy", Password: password, WorkFactor: fast.WorkFactor})
	if again.OK || again.Status != "exists" {
		t.Fatalf("second create = %#v", again)
	}

	encrypted := Apply(homer, Command{
		Action: "encrypt", ID: "codebuddy", File: "providers", Path: destination,
		Password: "wrong password", WorkFactor: fast.WorkFactor,
	})
	if encrypted.Status != "bad-password" {
		t.Fatalf("bad password = %#v", encrypted)
	}
	encrypted = Apply(homer, Command{
		Action: "encrypt", ID: "codebuddy", File: "providers", Path: destination,
		Password: password, WorkFactor: fast.WorkFactor,
	})
	if !encrypted.OK {
		t.Fatalf("encrypt = %#v", encrypted)
	}

	loaded, err := core.LoadConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Adapters["keyring"]; !ok {
		t.Fatal("create did not register the keyring adapter")
	}
	envelope, err := os.ReadFile(filepath.Join(root, ".homer", "keyring", "items", "codebuddy", "envelope.age"))
	if err != nil {
		t.Fatal(err)
	}
	fileBlob, err := os.ReadFile(filepath.Join(root, ".homer", "keyring", "items", "codebuddy", "files", "providers.age"))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(envelope) + string(fileBlob) + string(mustRead(t, filepath.Join(root, ".homer", "keyring", "items", "codebuddy", "manifest.json")))
	if strings.Contains(joined, plaintext) || strings.Contains(joined, "AGE-SECRET-KEY-") || strings.Contains(joined, password) {
		t.Fatal("keyring stored plaintext, the data key, or the password")
	}

	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	unlocked := Apply(homer, Command{Action: "unlock", ID: "codebuddy", Password: password, WorkFactor: fast.WorkFactor})
	if !unlocked.OK {
		t.Fatalf("unlock = %#v", unlocked)
	}
	restored, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != plaintext {
		t.Fatalf("restored = %q", restored)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v err=%v", info, err)
	}

	rotated := Apply(homer, Command{Action: "passwd", ID: "codebuddy", Password: password, NewPassword: "a-new-password", WorkFactor: fast.WorkFactor})
	if !rotated.OK {
		t.Fatalf("passwd = %#v", rotated)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	old := Apply(homer, Command{Action: "unlock", ID: "codebuddy", Password: password, WorkFactor: fast.WorkFactor})
	if old.Status != "bad-password" {
		t.Fatalf("old password = %#v", old)
	}
	fresh := Apply(homer, Command{Action: "unlock", ID: "codebuddy", Password: "a-new-password", WorkFactor: fast.WorkFactor})
	if !fresh.OK {
		t.Fatalf("new password unlock = %#v", fresh)
	}
	if string(mustRead(t, destination)) != plaintext {
		t.Fatal("password change rewrote the file body")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
