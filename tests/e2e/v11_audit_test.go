package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

// TestV11AuditMainline freezes the audit reproduction rather than relying on
// a pre-existing clone: init writes the snapshot, git remote add wires the
// bare origin, the first no-drift push creates the repository baseline, and a
// clean clone can immediately run home.
func TestV11AuditMainline(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer v1.1 E2E\n\temail = homer-v11@example.invalid\n")
	origin := filepath.Join(root, "origin.git")
	runGit(t, global, root, "init", "--bare", "-b", "master", origin)

	a := makeMachine(t, root, "audit-A", global, true)
	init := mustJSONHomer(t, binary, a, "init", "--json")
	if len(init["adapters"].([]any)) != 3 {
		t.Fatalf("init adapters = %#v", init["adapters"])
	}
	// This is intentionally outside Homer to match the advisor's exact
	// reproduction of a user manually connecting the repository.
	runGit(t, global, a.homerHome, "init", "-b", "master")
	runGit(t, global, a.homerHome, "remote", "add", "origin", origin)
	push := mustJSONHomer(t, binary, a, "push", "--yes", "--json")
	if push["status"] != "pushed" || push["pushedToRemote"] != true {
		t.Fatalf("first audit push = %#v", push)
	}
	for _, path := range []string{"homer.json", ".gitignore", "store/pi/settings/settings.json"} {
		if result := runProcess(t, root, global, nil, "git", "--git-dir", origin, "show", "master:"+path); result.code != 0 {
			t.Fatalf("bare origin missing %s: %#v", path, result)
		}
	}

	b := makeMachine(t, root, "audit-B", global, false)
	home := mustJSONHomer(t, binary, b, "home", origin, "--yes", "--json")
	if home["status"] != "homed" || home["ok"] != true {
		t.Fatalf("clone home report = %#v", home)
	}
	if _, err := os.Stat(filepath.Join(b.homerHome, "homer.json")); err != nil {
		t.Fatalf("clone did not contain homer.json: %v", err)
	}
	config, err := core.LoadConfig(core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return b.homerHome
		}
		return ""
	}))
	if err != nil || len(config.Adapters) != 3 {
		t.Fatalf("cloned config = %#v, err=%v", config, err)
	}
	if result := runHomer(t, binary, b, "version"); result.code != 0 || !strings.Contains(result.stdout, "homer version: dev") {
		t.Fatalf("development version = %#v", result)
	}
	if result := runHomer(t, binary, b, "--version"); result.code != 0 || result.stdout != "homer version: dev\n" {
		t.Fatalf("--version = %#v", result)
	}
}
