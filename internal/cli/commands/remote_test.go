package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
)

func TestRunRemoteEnsuresRepoSetsOriginAndPrintsFirstPush(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	if result := gitx.Exec(root, []string{"init", "--bare", "-b", "master", origin}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	home := filepath.Join(root, "homer")
	report := RunRemote(RemoteOptions{HomerHome: home, URL: origin})
	if report.Status != RemoteStatusConfigured || report.ExitCode() != 0 {
		t.Fatalf("remote report = %#v", report)
	}
	if !gitx.IsGitRepo(home) || !gitx.HasRemote(home) {
		t.Fatal("remote command did not create a git repository and origin")
	}
	if got := strings.TrimSpace(gitx.Exec(home, []string{"remote", "get-url", "origin"}, 0).Stdout); got != origin {
		t.Fatalf("origin URL = %q, want %q", got, origin)
	}
	wantHint := "git -C " + home + " push -u origin master"
	if report.PushCommand != wantHint || !strings.Contains(strings.Join(report.Warnings, "\n"), wantHint) {
		t.Fatalf("push hint = %#v, want %q", report, wantHint)
	}

	other := filepath.Join(root, "other.git")
	if result := gitx.Exec(root, []string{"init", "--bare", "-b", "master", other}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	updated := RunRemote(RemoteOptions{HomerHome: home, URL: other})
	if updated.Status != RemoteStatusConfigured {
		t.Fatalf("remote set-url report = %#v", updated)
	}
	if got := strings.TrimSpace(gitx.Exec(home, []string{"remote", "get-url", "origin"}, 0).Stdout); got != other {
		t.Fatalf("updated origin URL = %q, want %q", got, other)
	}
}

func TestRunRemoteRejectsOptionURL(t *testing.T) {
	report := RunRemote(RemoteOptions{HomerHome: t.TempDir(), URL: "--upload-pack=evil"})
	if report.Status != RemoteStatusError || report.ExitCode() != 1 {
		t.Fatalf("unsafe remote report = %#v", report)
	}
	if !strings.Contains(strings.Join(report.Errors, "\n"), "不得以") {
		t.Fatalf("unsafe remote errors = %#v", report.Errors)
	}
}

func TestInitRemoteCommitsAndPushesConfigurationCenter(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(global, []byte("[user]\n\tname = Homer Init Test\n\temail = init@example.invalid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	origin := filepath.Join(root, "origin.git")
	if result := gitx.Exec(root, []string{"init", "--bare", "-b", "master", origin}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	home := filepath.Join(root, "homer")
	snapshot := core.AdapterSnapshot{AdapterID: "pi", Categories: []core.CategorySnapshot{{
		AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror,
		Files: core.SnapshotFiles{"settings.json": {Kind: "file", Content: "baseline\n"}},
	}}}
	report, err := RunInitWithDeps(InitOptions{HomerHome: home, Adapters: []string{"pi"}, Remote: origin}, InitDeps{
		Scan: func(string, core.AdapterConfig) adapter.ScanOutcome { return adapter.ScanOutcome{Snapshot: snapshot} },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Adapters) != 1 || !gitx.HasUpstream(home) {
		t.Fatalf("init remote report/repo = %#v", report)
	}
	for _, path := range []string{"homer.json", ".gitignore", "store/pi/settings/settings.json"} {
		if result := gitx.Exec(origin, []string{"show", "master:" + path}, 0); !result.OK {
			t.Fatalf("origin missing %s: %s", path, result.Stderr)
		}
	}
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return ""
	})
	if state := core.LoadState(paths); state.LastSyncCommit == "" || state.LastSyncCommand != "push" {
		t.Fatalf("init state = %#v", state)
	}
}
