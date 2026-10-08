package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pi's agent directory is collected in full. Credential files from the pi
// spec stay out of the plaintext store and round-trip through the keyring.
func TestPiCollectsTheTreeAndEncryptsCredentialFiles(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	fakeHome := filepath.Join(root, "home")
	homerHome := filepath.Join(root, "homer")
	agent := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(filepath.Join(agent, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(agent, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(agent, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("settings.json", "{\"theme\":\"dark\"}\n")
	write("custom-note.md", "kept in the center\n")
	write("notes/todo.md", "also kept\n")
	write("auth.json", "{\"token\":\"pi-rule-secret\"}\n")
	write("mcp-auth.json", "{\"access\":\"oauth\"}\n")
	write(filepath.Join("sessions", "chat.jsonl"), "session data\n")

	machine := machine{name: "a", fakeHome: fakeHome, homerHome: homerHome, gitGlobal: filepath.Join(root, "gitconfig")}
	initResult := runHomer(t, binary, machine, "init", "--adapters", "pi", "--json")
	if initResult.code != 0 {
		t.Fatalf("init exit %d\nstdout:\n%s\nstderr:\n%s", initResult.code, initResult.stdout, initResult.stderr)
	}

	store := filepath.Join(homerHome, "store", "pi")
	mustRead := func(rel, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(store, rel))
		if err != nil {
			t.Fatalf("store %s: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("store %s = %q", rel, got)
		}
	}
	mustRead("settings/settings.json", "{\"theme\":\"dark\"}\n")
	mustRead("files/custom-note.md", "kept in the center\n")
	mustRead("files/notes/todo.md", "also kept\n")
	for _, banned := range []string{
		"files/auth.json",
		"settings/auth.json",
		"files/mcp-auth.json",
		"files/sessions/chat.jsonl",
	} {
		if _, err := os.Stat(filepath.Join(store, banned)); !os.IsNotExist(err) {
			t.Fatalf("plaintext store contains %s", banned)
		}
	}

	password := "pi-rule-pass"
	created := runHomer(t, binary, machine, "key", "create", "--name", "pi", "--id", "pi", "--password", password, "--json")
	createdJSON := mustJSON(t, created, "key create")
	if createdJSON["ok"] != true {
		t.Fatalf("create = %#v", createdJSON)
	}
	encrypted := runHomer(t, binary, machine, "key", "encrypt", "--id", "pi", "--path", "~/.pi/agent/auth.json", "--adapter", "pi", "--password", password, "--json")
	encJSON := mustJSON(t, encrypted, "key encrypt")
	if encJSON["ok"] != true {
		t.Fatalf("encrypt = %#v", encJSON)
	}
	blob, err := os.ReadFile(filepath.Join(fakeHome, ".homer", "keyring", "items", "pi", "files", "auth.json.age"))
	if err != nil {
		t.Fatalf("ciphertext missing: %v", err)
	}
	if strings.Contains(string(blob), "pi-rule-secret") {
		t.Fatal("ciphertext still contains the credential")
	}

	// Remove the tool file and prove unlock restores it.
	if err := os.Remove(filepath.Join(agent, "auth.json")); err != nil {
		t.Fatal(err)
	}
	unlocked := runHomer(t, binary, machine, "key", "unlock", "--id", "pi", "--password", password, "--json")
	if unlocked.code != 0 {
		t.Fatalf("unlock exit %d\n%s\n%s", unlocked.code, unlocked.stdout, unlocked.stderr)
	}
	restored, err := os.ReadFile(filepath.Join(agent, "auth.json"))
	if err != nil || string(restored) != "{\"token\":\"pi-rule-secret\"}\n" {
		t.Fatalf("restored auth.json = %q err=%v", restored, err)
	}
}
