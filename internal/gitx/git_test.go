package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustExec(t *testing.T, home string, args ...string) string {
	t.Helper()
	result := Exec(home, args, 5*time.Second)
	if !result.OK {
		t.Fatalf("git %s failed: %s", strings.Join(args, " "), result.Stderr)
	}
	return result.Stdout
}

func testRepo(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := EnsureGitRepo(home); err != nil {
		t.Fatalf("EnsureGitRepo: %v", err)
	}
	mustExec(t, home, "branch", "-M", "main")
	mustExec(t, home, "config", "--local", "user.email", "homer-test@example.invalid")
	mustExec(t, home, "config", "--local", "user.name", "Homer Test")
	return home
}

func writeTestFile(t *testing.T, filename, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filename, err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
}

func commitTestStore(t *testing.T, home, content, message string) string {
	t.Helper()
	writeTestFile(t, filepath.Join(home, "store", "pi", "settings.json"), content)
	sha := CommitAllStore(home, message)
	if sha == "" {
		t.Fatalf("CommitAllStore(%q) returned empty", message)
	}
	return sha
}

func bareOrigin(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	origin := filepath.Join(parent, "origin.git")
	result := Exec(parent, []string{"init", "--bare", "-b", "main", origin}, 5*time.Second)
	if !result.OK {
		t.Fatalf("git init --bare: %s", result.Stderr)
	}
	return origin
}

func pushOrigin(t *testing.T, home, origin string) {
	t.Helper()
	mustExec(t, home, "remote", "add", "origin", origin)
	if result := Exec(home, []string{"push", "-u", "origin", "main"}, 5*time.Second); !result.OK {
		t.Fatalf("git push -u: %s", result.Stderr)
	}
}

func TestExecDefaultTimeoutAndFailureSummary(t *testing.T) {
	repo := testRepo(t)
	if GitDefaultTimeout != 15*time.Second {
		t.Fatalf("default timeout = %s, want 15s", GitDefaultTimeout)
	}

	result := Exec(repo, []string{"definitely-not-a-git-command"}, time.Second)
	if result.OK || result.Stdout != "" || !strings.Contains(strings.ToLower(result.Stderr), "not a git command") {
		t.Fatalf("unexpected failed command result: %#v", result)
	}

	started := time.Now()
	result = Exec(repo, []string{"-c", "alias.homerhang=!sleep 30", "homerhang"}, 250*time.Millisecond)
	if result.OK {
		t.Fatal("timed out command unexpectedly succeeded")
	}
	if !strings.Contains(strings.ToLower(result.Stderr), "timed out") && !strings.Contains(strings.ToLower(result.Stderr), "deadline") {
		t.Fatalf("timeout stderr lacks timeout summary: %q", result.Stderr)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("timeout took too long: %s", elapsed)
	}
}

func TestEnsureGitRepoIsIdempotentAndMaintainsPrivateLines(t *testing.T) {
	home := t.TempDir()
	if err := EnsureGitRepo(home); err != nil {
		t.Fatalf("first EnsureGitRepo: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(home, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"state.json", "backups/", "keys/"} {
		if strings.Count(string(first), "\n"+required+"\n") != 1 && !strings.HasPrefix(string(first), required+"\n") {
			t.Fatalf("required line %q missing from %q", required, first)
		}
	}
	if err := EnsureGitRepo(home); err != nil {
		t.Fatalf("second EnsureGitRepo: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(home, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("second ensure changed .gitignore:\nfirst=%q\nsecond=%q", first, second)
	}

	custom := t.TempDir()
	if err := os.WriteFile(filepath.Join(custom, ".gitignore"), []byte("state.json\n*.log"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureGitRepo(custom); err != nil {
		t.Fatalf("custom EnsureGitRepo: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(custom, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	want := "state.json\n*.log\nbackups/\nkeys/\n"
	if string(got) != want {
		t.Fatalf("custom .gitignore = %q, want %q", got, want)
	}
}

func TestCommitAllStoreScopesConfigurationCenterAndNoopIsEmpty(t *testing.T) {
	repo := testRepo(t)
	// EnsureGitRepo creates .gitignore before the first baseline. It is part of
	// the configuration center, so this is intentionally a real (non-empty)
	// initial commit even before store content exists.
	initial := CommitAllStore(repo, "nothing")
	if initial == "" {
		t.Fatal("initial .gitignore commit = empty")
	}
	if got := strings.TrimSpace(mustExec(t, repo, "show", "--name-only", "--format=", "HEAD")); got != ".gitignore" {
		t.Fatalf("initial paths = %q", got)
	}
	writeTestFile(t, filepath.Join(repo, "homer.json"), "{\"version\":1}\n")
	first := commitTestStore(t, repo, "{\"model\":\"sonnet\"}\n", "store one")
	if HeadCommit(repo) != first || !IsStoreClean(repo) || !IsPushClean(repo) {
		t.Fatalf("configuration-center commit did not produce a clean HEAD")
	}
	firstPaths := strings.TrimSpace(mustExec(t, repo, "show", "--name-only", "--format=", "HEAD"))
	if firstPaths != "homer.json\nstore/pi/settings.json" {
		t.Fatalf("initial configuration-center paths = %q", firstPaths)
	}

	writeTestFile(t, filepath.Join(repo, "homer.json"), "{\"version\":2}\n")
	writeTestFile(t, filepath.Join(repo, "store", "pi", "settings.json"), "{\"model\":\"opus\"}\n")
	second := CommitAllStore(repo, "store two")
	if second == "" || second == first {
		t.Fatalf("second store commit = %q, first = %q", second, first)
	}
	changed := strings.TrimSpace(mustExec(t, repo, "show", "--name-only", "--format=", "HEAD"))
	if changed != "homer.json\nstore/pi/settings.json" {
		t.Fatalf("configuration-center commit paths = %q", changed)
	}
	if !IsPushClean(repo) {
		t.Fatal("configuration-center paths remained dirty after commit")
	}
	if got := CommitAllStore(repo, "again"); got != "" {
		t.Fatalf("noop store commit = %q, want empty", got)
	}
}

func TestCommitPathsDoesNotCoupleSecretsAndStore(t *testing.T) {
	repo := testRepo(t)
	writeTestFile(t, filepath.Join(repo, "secrets", "a.age"), "cipher\n")
	writeTestFile(t, filepath.Join(repo, "store", "pi", "x.txt"), "store\n")
	if got := CommitPaths(repo, []string{"secrets/"}, "secret"); got == "" {
		t.Fatal("CommitPaths returned empty for a secret change")
	}
	if got := strings.TrimSpace(mustExec(t, repo, "show", "--name-only", "--format=", "HEAD")); got != "secrets/a.age" {
		t.Fatalf("secret commit paths = %q", got)
	}
	if IsStoreClean(repo) {
		t.Fatal("store change was coupled to secret commit")
	}
	if got := CommitPaths(repo, []string{"secrets/"}, "again"); got != "" {
		t.Fatalf("noop secret commit = %q", got)
	}
}

func TestUpstreamAndPushTarget(t *testing.T) {
	repo := testRepo(t)
	if HasUpstream(repo) || HasPushTarget(repo) || UpstreamRef(repo) != "" {
		t.Fatal("new repository unexpectedly has a push target")
	}
	// branch.<name>.remote is enough for S3 even if the tracking ref is gone.
	mustExec(t, repo, "config", "--local", "branch.main.remote", "origin")
	if !HasPushTarget(repo) {
		t.Fatal("branch.remote configuration was not recognized as push target")
	}
	if HasUpstream(repo) {
		t.Fatal("unresolvable branch config unexpectedly resolved upstream")
	}
	if got := strings.TrimSpace(mustExec(t, repo, "config", "--local", "--get", "user.email")); got != "homer-test@example.invalid" {
		t.Fatalf("repository user.email = %q", got)
	}
	if got := strings.TrimSpace(mustExec(t, repo, "config", "--local", "--get", "user.name")); got != "Homer Test" {
		t.Fatalf("repository user.name = %q", got)
	}
}

func TestAncestorAndFastForward(t *testing.T) {
	origin := bareOrigin(t)
	writer := testRepo(t)
	commitTestStore(t, writer, "{\"version\":1}\n", "writer one")
	pushOrigin(t, writer, origin)
	readerDir := filepath.Join(t.TempDir(), "reader")
	if err := CloneRepo(origin, readerDir, 5*time.Second); err != nil {
		t.Fatalf("clone reader: %v", err)
	}
	mustExec(t, readerDir, "config", "--local", "user.email", "homer-test@example.invalid")
	mustExec(t, readerDir, "config", "--local", "user.name", "Homer Test")
	base := HeadCommit(readerDir)
	if base == "" || !HasUpstream(readerDir) {
		t.Fatal("reader clone has no initial upstream")
	}

	commitTestStore(t, writer, "{\"version\":2}\n", "writer two")
	if result := Push(writer); !result.OK {
		t.Fatalf("writer push: %s", result.Stderr)
	}
	if result := Fetch(readerDir); !result.OK {
		t.Fatalf("reader fetch: %s", result.Stderr)
	}
	if !IsAncestorOf(readerDir, base, UpstreamRef(readerDir)) {
		t.Fatal("base is not an ancestor of fetched upstream")
	}
	if result := MergeFFUpstream(readerDir); !result.OK {
		t.Fatalf("fast-forward merge: %s", result.Stderr)
	}
	if HeadCommit(readerDir) != HeadCommit(writer) {
		t.Fatal("fast-forward did not reach writer HEAD")
	}

	// Make one local-only commit and one remote-only commit.  The second
	// fetch leaves the local branch divergent, so --ff-only must fail without
	// moving HEAD.
	writeTestFile(t, filepath.Join(readerDir, "store", "pi", "local.txt"), "local\n")
	localOnly := CommitAllStore(readerDir, "local only")
	if localOnly == "" {
		t.Fatal("local-only commit failed")
	}
	otherDir := filepath.Join(t.TempDir(), "other")
	if err := CloneRepo(origin, otherDir, 5*time.Second); err != nil {
		t.Fatalf("clone other: %v", err)
	}
	mustExec(t, otherDir, "config", "--local", "user.email", "homer-test@example.invalid")
	mustExec(t, otherDir, "config", "--local", "user.name", "Homer Test")
	writeTestFile(t, filepath.Join(otherDir, "store", "pi", "remote.txt"), "remote\n")
	if got := CommitAllStore(otherDir, "remote only"); got == "" {
		t.Fatal("remote-only commit failed")
	}
	if result := Push(otherDir); !result.OK {
		t.Fatalf("other push: %s", result.Stderr)
	}
	if result := Fetch(readerDir); !result.OK {
		t.Fatalf("divergent fetch: %s", result.Stderr)
	}
	before := HeadCommit(readerDir)
	result := MergeFFUpstream(readerDir)
	if result.OK || !strings.Contains(strings.ToLower(result.Stderr), "fast") && !strings.Contains(strings.ToLower(result.Stderr), "not possible") {
		t.Fatalf("divergent ff result: %#v", result)
	}
	if HeadCommit(readerDir) != before || before != localOnly {
		t.Fatal("failed fast-forward changed local HEAD")
	}
	if IsAncestorOf(readerDir, before, UpstreamRef(readerDir)) {
		t.Fatal("divergent local HEAD reported as upstream ancestor")
	}
}

func TestCloneRepoUsesSeparatorAndRejectsOptionURLs(t *testing.T) {
	shimDir := t.TempDir()
	argsFile := filepath.Join(shimDir, "args")
	shim := filepath.Join(shimDir, "git")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GITX_ARGS_FILE\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	oldArgs := os.Getenv("GITX_ARGS_FILE")
	if err := os.Setenv("PATH", shimDir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("GITX_ARGS_FILE", argsFile); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Setenv("PATH", oldPath)
		_ = os.Setenv("GITX_ARGS_FILE", oldArgs)
	}()

	if err := CloneRepo("https://example.invalid/homer.git", filepath.Join(t.TempDir(), "dest"), time.Second); err != nil {
		t.Fatalf("shim clone: %v", err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(lines) != 4 || lines[0] != "clone" || lines[1] != "--" || lines[2] != "https://example.invalid/homer.git" {
		t.Fatalf("clone argv = %#v", lines)
	}
	if lines[3] == "" {
		t.Fatal("clone destination argument missing")
	}
	for _, url := range []string{"-ofoo", "--upload-pack=touch /tmp/pwned"} {
		if err := AssertCloneableRepoURL(url); err == nil {
			t.Fatalf("option URL %q was accepted", url)
		}
	}
}
