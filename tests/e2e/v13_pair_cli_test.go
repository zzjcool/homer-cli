package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestV13PairMissingTailcatShowsInstallAndGitFallback(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer v1.3 Pair E2E\n\temail = homer-v13-pair@example.invalid\n")
	result := runProcess(t, repoRoot(), global, []string{
		"PATH=" + t.TempDir(),
		"HOME=" + filepath.Join(root, "home"),
		"HOMER_HOME=" + filepath.Join(root, "homer"),
	}, binary, "pair")
	if result.code != 1 {
		t.Fatalf("homer pair exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", result.code, result.stdout, result.stderr)
	}
	for _, expected := range []string{
		"https://github.com/tailscale/tailcat",
		"homer secret push",
		"homer secret pull",
	} {
		if !strings.Contains(result.stderr, expected) {
			t.Fatalf("homer pair stderr missing %q:\n%s", expected, result.stderr)
		}
	}
}

func TestV13PairHelpAndJSONSmoke(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer v1.3 Pair Help E2E\n\temail = homer-v13-pair-help@example.invalid\n")
	env := []string{
		"PATH=" + t.TempDir(),
		"HOME=" + filepath.Join(root, "home"),
		"HOMER_HOME=" + filepath.Join(root, "homer"),
	}

	help := runProcess(t, repoRoot(), global, env, binary, "pair", "--help")
	if help.code != 0 || help.stderr != "" {
		t.Fatalf("homer pair --help = %#v", help)
	}
	if !strings.Contains(help.stdout, "用法: homer pair [options]") || !strings.Contains(help.stdout, "homer pair <tc-addr>") || !strings.Contains(help.stdout, "勿粘贴到 git") {
		t.Fatalf("homer pair --help missing frozen usage:\n%s", help.stdout)
	}

	jsonResult := runProcess(t, repoRoot(), global, env, binary, "pair", "--json")
	if jsonResult.code != 1 {
		t.Fatalf("homer pair --json exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", jsonResult.code, jsonResult.stdout, jsonResult.stderr)
	}
	report := map[string]any{}
	if err := json.Unmarshal([]byte(jsonResult.stdout), &report); err != nil {
		t.Fatalf("homer pair --json output is not JSON: %v\n%s", err, jsonResult.stdout)
	}
	if report["ok"] != false || report["status"] != "no-tailcat" {
		t.Fatalf("homer pair --json report = %#v", report)
	}
	if !strings.Contains(jsonResult.stderr, "homer secret push") {
		t.Fatalf("homer pair --json stderr missing git fallback: %s", jsonResult.stderr)
	}
}

// Keep the process-boundary fixture in this wave even though the complete
// serve handshake is owned by the P1 command orchestrator. It is used by the
// adapter unit tests through the same executable shape: an address file is
// written immediately, then the process remains attached to stdin/stdout.
func TestV13PairFakeTailcatFixtureIsExecutable(t *testing.T) {
	dir := t.TempDir()
	tailcat := filepath.Join(dir, "tailcat")
	if err := os.WriteFile(tailcat, []byte("#!/bin/sh\nprintf 'fixture-address\\n' > \"$TAILCAT_ADDR_FILE\"\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(tailcat); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("fake tailcat fixture = %v, err=%v", info, err)
	}
}
