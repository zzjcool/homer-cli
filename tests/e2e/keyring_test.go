package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyringSyncsAndUnlocksOnAnotherMachine(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer Keyring\n\temail = homer-keyring@example.invalid\n")
	origin := filepath.Join(root, "origin.git")
	runGit(t, global, root, "init", "--bare", "-b", "main", origin)

	a := makeMachine(t, root, "A", global, true)
	runGit(t, global, root, "clone", origin, a.homerHome)
	mustJSONHomer(t, binary, a, "init", "--json")

	const password = "correct-horse"
	const plaintext = "KEYRING-E2E-PLAINTEXT-91c7\n"
	writeFile(t, filepath.Join(a.fakeHome, "providers.json"), plaintext)
	mustJSONHomer(t, binary, a, "key", "create", "--id", "codebuddy", "--name", "CodeBuddy", "--password", password, "--json")
	mustJSONHomer(t, binary, a, "key", "encrypt", "--id", "codebuddy", "--file", "providers", "--path", "~/providers.json", "--password", password, "--json")
	mustJSONHomer(t, binary, a, "push", "--yes", "--json")

	b := makeMachine(t, root, "B", global, false)
	home := mustJSONHomer(t, binary, b, "home", origin, "--yes", "--json")
	if home["ok"] != true {
		t.Fatalf("home = %#v", home)
	}
	if _, err := os.Stat(filepath.Join(b.fakeHome, "providers.json")); err == nil {
		t.Fatal("plaintext arrived before unlock")
	}
	mustJSONHomer(t, binary, b, "key", "unlock", "--id", "codebuddy", "--password", password, "--json")
	restored, err := os.ReadFile(filepath.Join(b.fakeHome, "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != plaintext {
		t.Fatalf("restored = %q", restored)
	}
	world := &e2eWorld{origin: origin, root: root, gitGlobal: global}
	assertOriginDoesNotContain(t, world, "KEYRING-E2E-PLAINTEXT-91c7")
	assertOriginDoesNotContain(t, world, "AGE-SECRET-KEY-")
	assertOriginDoesNotContain(t, world, password)
}

func TestKeyringRejectsShortPassword(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer Keyring\n\temail = homer-keyring@example.invalid\n")
	m := makeMachine(t, root, "A", global, false)
	if err := os.MkdirAll(m.homerHome, 0o755); err != nil {
		t.Fatal(err)
	}
	mustJSONHomer(t, binary, m, "init", "--json")
	result := runHomer(t, binary, m, "key", "create", "--id", "codebuddy", "--password", "short", "--json")
	if result.code == 0 || !strings.Contains(result.stderr, "8") && !strings.Contains(result.stdout, "8") {
		t.Fatalf("short password = %#v", result)
	}
}
