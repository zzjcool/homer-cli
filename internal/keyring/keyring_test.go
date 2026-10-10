package keyring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter/pi"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
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
	if _, ok := loaded.Adapters["keyring"]; ok {
		t.Fatal("keyring operations unexpectedly registered the keyring adapter")
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

func TestListIncludesKeyCollectedIntoCenter(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	homer := filepath.Join(root, ".homer")
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return homer
		}
		return ""
	})
	if err := core.SaveConfig(paths, core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{}}); err != nil {
		t.Fatal(err)
	}
	manifest := "{\n  \"id\": \"codebuddy\",\n  \"name\": \"CodeBuddy\",\n  \"files\": [{\"id\": \"providers\", \"destination\": \"~/providers.json\"}]\n}\n"
	if _, err := gens.New(homer).Publish(map[string]map[string]string{
		"keyring": {"items/codebuddy/manifest.json": manifest},
	}, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	listed := Apply(homer, Command{Action: "list"})
	if !listed.OK || len(listed.Keys) != 1 || listed.Keys[0].ID != "codebuddy" {
		t.Fatalf("center list = %#v", listed)
	}
	if len(listed.Keys[0].Files) != 1 || listed.Keys[0].Files[0].Destination != "~/providers.json" {
		t.Fatalf("files = %#v", listed.Keys[0].Files)
	}
}

func TestCheckPasswordDoesNotWritePlaintext(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	homer := filepath.Join(root, ".homer")
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return homer
		}
		return ""
	})
	if err := core.SaveConfig(paths, core.HomerConfig{Version: 1}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "models.json")
	if err := os.WriteFile(destination, []byte("secret-body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := Apply(homer, Command{Action: "create", ID: "pi", Name: "pi", Password: "long-password", WorkFactor: 14})
	if !created.OK {
		t.Fatalf("create = %#v", created)
	}
	encrypted := Apply(homer, Command{Action: "encrypt", ID: "pi", Path: destination, Adapter: "pi", Password: "long-password", WorkFactor: 14})
	if !encrypted.OK {
		t.Fatalf("encrypt = %#v", encrypted)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	wrong := Apply(homer, Command{Action: "check", ID: "pi", Password: "wrong-password"})
	if wrong.OK || wrong.Status != "bad-password" {
		t.Fatalf("wrong = %#v", wrong)
	}
	checked := Apply(homer, Command{Action: "check", ID: "pi", Password: "long-password"})
	if !checked.OK {
		t.Fatalf("check = %#v", checked)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("check wrote the plaintext")
	}
}

func TestListKeepsLocalFileTheCenterHasNotCollected(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	homer := filepath.Join(root, ".homer")
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return homer
		}
		return ""
	})
	if err := core.SaveConfig(paths, core.HomerConfig{Version: 1}); err != nil {
		t.Fatal(err)
	}
	models := filepath.Join(root, "models.json")
	providers := filepath.Join(root, "providers.json")
	if err := os.WriteFile(models, []byte("models\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(providers, []byte("providers\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := Apply(homer, Command{Action: "create", ID: "pi", Name: "pi", Password: "long-password", WorkFactor: 14})
	if !created.OK {
		t.Fatalf("create = %#v", created)
	}
	for _, path := range []string{models, providers} {
		encrypted := Apply(homer, Command{Action: "encrypt", ID: "pi", Path: path, Adapter: "pi", Password: "long-password", WorkFactor: 14})
		if !encrypted.OK {
			t.Fatalf("encrypt %s = %#v", path, encrypted)
		}
	}
	center := "{\n  \"id\": \"pi\",\n  \"name\": \"pi\",\n  \"files\": [{\"id\": \"models.json\", \"destination\": \"" + models + "\", \"adapter\": \"pi\"}]\n}\n"
	if _, err := gens.New(homer).Publish(map[string]map[string]string{
		"keyring": {"items/pi/manifest.json": center},
	}, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	listed := Apply(homer, Command{Action: "list"})
	if !listed.OK || len(listed.Keys) != 1 {
		t.Fatalf("list = %#v", listed)
	}
	got := map[string]bool{}
	for _, file := range listed.Keys[0].Files {
		got[file.Destination] = true
	}
	if !got[models] || !got[providers] {
		t.Fatalf("files = %#v", listed.Keys[0].Files)
	}
}

func TestCreateAndEncryptAssignIDsFromNameAndPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	homer := filepath.Join(root, ".homer")
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return homer
		}
		return ""
	})
	if err := core.SaveConfig(paths, core.HomerConfig{Version: 1}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "models.json")
	if err := os.WriteFile(destination, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := Apply(homer, Command{Action: "create", Name: "提供商", Password: "long-password", WorkFactor: 14})
	if !created.OK || created.Keys[0].Name != "提供商" || !strings.HasPrefix(created.Keys[0].ID, "k-") {
		t.Fatalf("create = %#v", created)
	}
	id := created.Keys[0].ID
	encrypted := Apply(homer, Command{Action: "encrypt", ID: id, Path: destination, Adapter: "pi", Password: "long-password", WorkFactor: 14})
	if !encrypted.OK || len(encrypted.Keys[0].Files) != 1 || encrypted.Keys[0].Files[0].Destination != destination || encrypted.Keys[0].Files[0].Adapter != "pi" {
		t.Fatalf("encrypt = %#v", encrypted)
	}
	if rejected := Apply(homer, Command{Action: "encrypt", ID: id, Path: destination, Adapter: "keyring", Password: "long-password", WorkFactor: 14}); rejected.OK {
		t.Fatalf("keyring bind = %#v", rejected)
	}
	if encrypted.Keys[0].Files[0].ID != "models.json" {
		t.Fatalf("file id = %q", encrypted.Keys[0].Files[0].ID)
	}
	again := Apply(homer, Command{Action: "encrypt", ID: id, Path: destination, Password: "long-password", WorkFactor: 14})
	if !again.OK || len(again.Keys[0].Files) != 1 {
		t.Fatalf("second encrypt = %#v", again)
	}
}

func TestBrowseExpandsDirectoryAndFiltersPrefix(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	agent := filepath.Join(root, ".pi", "agent")
	if err := os.MkdirAll(agent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agent, "models.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agent, "auth.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	homer := filepath.Join(root, ".homer")
	opened := Apply(homer, Command{Action: "browse", Path: "~/.pi/agent"})
	if !opened.OK || len(opened.Entries) != 2 {
		t.Fatalf("dir = %#v", opened)
	}
	if opened.Entries[0].Name != "auth.json" || opened.Entries[0].Dir || opened.Entries[0].Path != "~/.pi/agent/auth.json" {
		t.Fatalf("first = %#v", opened.Entries[0])
	}
	filtered := Apply(homer, Command{Action: "browse", Path: "~/.pi/ag"})
	if !filtered.OK || len(filtered.Entries) != 1 || !filtered.Entries[0].Dir || filtered.Entries[0].Path != "~/.pi/agent/" {
		t.Fatalf("prefix = %#v", filtered)
	}
	if rejected := Apply(homer, Command{Action: "browse", Path: "~/../etc"}); rejected.OK {
		t.Fatalf("parent = %#v", rejected)
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
