package agentd

import (
	"os"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

// TestEnsureMachineKeyringAdapter locks the reviewer-F1 fix: a remote key
// create must declare the keyring adapter on the machine it lands on, or the
// key silently never enters the collect choices (the old silent-stranding
// regression).
func TestEnsureMachineKeyringAdapter(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return home
		}
		return os.Getenv(name)
	})

	// No homer.json: the write path must not invent a machine configuration.
	if err := ensureMachineKeyringAdapter(home); err == nil {
		t.Fatal("missing homer.json must be an error, not a silent recreate")
	}

	// Seed a config without keyring; the ensure must add it.
	if err := os.WriteFile(paths.ConfigFile, []byte("{\"version\":1,\"adapters\":{\"pi\":{\"root\":\"~/.pi/agent\",\"categories\":{\"settings\":{\"paths\":[\"settings.json\"],\"mode\":\"merge\"}}}}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureMachineKeyringAdapter(home); err != nil {
		t.Fatal(err)
	}
	config, err := core.LoadConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := config.Adapters["keyring"]; !ok {
		t.Fatalf("keyring adapter not declared after ensure: %v", config.Adapters)
	}
	// The pre-existing adapter must survive.
	if _, ok := config.Adapters["pi"]; !ok {
		t.Fatalf("pi adapter lost after ensure: %v", config.Adapters)
	}

	// Idempotent: a second ensure is a no-op (and must not error).
	if err := ensureMachineKeyringAdapter(home); err != nil {
		t.Fatal(err)
	}

	// The declared root is the official plugin's frozen keyring root, so a
	// key created afterwards lands inside the synced tree.
	if root := config.Adapters["keyring"].Root; root == "" {
		t.Fatalf("unexpected keyring root: %q", root)
	}
}
