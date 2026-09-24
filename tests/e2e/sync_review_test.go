package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cloneReviewMachine(t *testing.T, root, name, origin, global string) machine {
	t.Helper()
	m := makeMachine(t, root, name, global, false)
	runGit(t, global, root, "clone", origin, m.homerHome)
	runGit(t, global, m.homerHome, "config", "user.email", "sync-review@example.invalid")
	runGit(t, global, m.homerHome, "config", "user.name", "Sync Review")
	return m
}

func parseE2EJSON(t *testing.T, result processResult, command string) map[string]any {
	t.Helper()
	if result.stderr != "" {
		t.Fatalf("homer %s wrote stderr:\n%s", command, result.stderr)
	}
	report := map[string]any{}
	if err := json.Unmarshal([]byte(result.stdout), &report); err != nil {
		t.Fatalf("homer %s did not produce JSON: %v\n%s", command, err, result.stdout)
	}
	return report
}

func backupContentsFor(t *testing.T, root, suffix string) []string {
	t.Helper()
	contents := []string{}
	err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, filename)
		if relErr != nil {
			return relErr
		}
		if strings.HasSuffix(filepath.ToSlash(rel), suffix) {
			data, readErr := os.ReadFile(filename)
			if readErr != nil {
				return readErr
			}
			contents = append(contents, string(data))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestMVPSyncReviewGroups(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer Sync Review\n\temail = homer-sync-review@example.invalid\n")
	origin := filepath.Join(root, "origin.git")
	runGit(t, global, root, "init", "--bare", "-b", "main", origin)

	a := makeMachine(t, root, "sync-A", global, false)
	if err := os.MkdirAll(a.homerHome, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, global, a.homerHome, "init", "-b", "main")
	runGit(t, global, a.homerHome, "remote", "add", "origin", origin)
	mustJSONHomer(t, binary, a, "init", "--json")
	runGit(t, global, a.homerHome, "add", "-A")
	runGit(t, global, a.homerHome, "commit", "-m", "fixture: initial config")
	runGit(t, global, a.homerHome, "push", "-u", "origin", "main")

	alpha := filepath.Join(piRoot(a), "skills", "alpha", "SKILL.md")
	writeFile(t, alpha, "v1\n")
	firstPush := mustJSONHomer(t, binary, a, "push", "--yes", "--json")
	if firstPush["status"] != "pushed" {
		t.Fatalf("initial v1 push = %#v", firstPush)
	}

	b := cloneReviewMachine(t, root, "sync-B", origin, global)
	c := cloneReviewMachine(t, root, "sync-C", origin, global)
	writeFile(t, filepath.Join(piRoot(b), "skills", "alpha", "SKILL.md"), "v1\n")
	writeFile(t, filepath.Join(piRoot(c), "skills", "alpha", "SKILL.md"), "c-local\n")

	t.Run("03-pull-apply-and-backup-old-version", func(t *testing.T) {
		writeFile(t, alpha, "v2\n")
		push := mustJSONHomer(t, binary, a, "push", "--yes", "--json")
		if push["status"] != "pushed" {
			t.Fatalf("A v2 push = %#v", push)
		}
		pull := mustJSONHomer(t, binary, b, "pull", "--yes", "--json")
		if pull["status"] != "applied" || pull["ok"] != true {
			t.Fatalf("B pull v2 = %#v", pull)
		}
		if got, err := os.ReadFile(filepath.Join(piRoot(b), "skills", "alpha", "SKILL.md")); err != nil || string(got) != "v2\n" {
			t.Fatalf("B file after pull = %q, err=%v", got, err)
		}
		backups := backupContentsFor(t, filepath.Join(b.homerHome, "backups"), "pi/skills/alpha/SKILL.md")
		if !hasString(backups, "v1\n") {
			t.Fatalf("pull backup did not retain v1: %#v", backups)
		}
	})

	t.Run("04-remote-ahead-same-file-pull-rejects-conflict", func(t *testing.T) {
		pull := runHomer(t, binary, c, "pull", "--yes", "--json")
		if pull.code != 1 {
			t.Fatalf("conflicting pull exit = %d, stdout=%s", pull.code, pull.stdout)
		}
		report := parseE2EJSON(t, pull, "pull --yes --json")
		status, _ := report["status"].(string)
		if status != "conflicts-remain" && status != "error" {
			t.Fatalf("conflicting pull status = %#v", report)
		}
		if got, err := os.ReadFile(filepath.Join(piRoot(c), "skills", "alpha", "SKILL.md")); err != nil || string(got) != "c-local\n" {
			t.Fatalf("C local file was overwritten: %q, err=%v", got, err)
		}
	})

	t.Run("06-pull-delete-propagates-and-backs-up-deleted-content", func(t *testing.T) {
		if err := os.Remove(alpha); err != nil {
			t.Fatal(err)
		}
		push := mustJSONHomer(t, binary, a, "push", "--yes", "--json")
		if push["status"] != "pushed" {
			t.Fatalf("A delete push = %#v", push)
		}
		pull := mustJSONHomer(t, binary, b, "pull", "--yes", "--json")
		if pull["status"] != "applied" || pull["ok"] != true {
			t.Fatalf("B delete pull = %#v", pull)
		}
		if _, err := os.Stat(filepath.Join(piRoot(b), "skills", "alpha", "SKILL.md")); !os.IsNotExist(err) {
			t.Fatalf("B deleted file still exists: %v", err)
		}
		backups := backupContentsFor(t, filepath.Join(b.homerHome, "backups"), "pi/skills/alpha/SKILL.md")
		if !hasString(backups, "v2\n") {
			t.Fatalf("delete pull backup did not retain v2: %#v", backups)
		}
	})
}
