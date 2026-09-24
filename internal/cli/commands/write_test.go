package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
)

func writeTestPaths(t *testing.T) core.HomerPaths {
	t.Helper()
	home := t.TempDir()
	return core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return home
		}
		return os.Getenv(name)
	})
}

func writeTestConfig(paths core.HomerPaths, root string, mode core.SyncMode) core.HomerConfig {
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"pi": {
				Root: root,
				Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: mode},
				},
			},
		},
	}
	if err := core.SaveConfig(paths, config); err != nil {
		panic(err)
	}
	return config
}

func writeTestSnapshot(content string, mode core.SyncMode) []core.AdapterSnapshot {
	return []core.AdapterSnapshot{{
		AdapterID: "pi",
		Categories: []core.CategorySnapshot{{
			AdapterID: "pi", Category: "settings", Mode: mode,
			Files: core.SnapshotFiles{"settings.json": {Kind: "json", Content: content}},
		}},
	}}
}

func fakeWriteGit(heads ...string) GitPort {
	calls := 0
	return GitPort{
		IsGitRepo:              func(string) bool { return true },
		HasUpstream:            func(string) bool { return true },
		HasPushTarget:          func(string) bool { return true },
		Fetch:                  func(string) gitx.ExecResult { return gitx.ExecResult{OK: true} },
		GitFetch:               func(string) gitx.ExecResult { return gitx.ExecResult{OK: true} },
		RequireCleanStore:      func(core.HomerPaths) error { return nil },
		RequireFastForwardable: func(core.HomerPaths) error { return nil },
		MergeFFUpstream:        func(string) gitx.ExecResult { return gitx.ExecResult{OK: true} },
		HeadCommit: func(string) string {
			index := calls
			calls++
			if index < len(heads) {
				return heads[index]
			}
			return heads[len(heads)-1]
		},
		IsStoreClean:        func(string) bool { return true },
		CommitStoreIfNeeded: func(core.HomerPaths, string) string { return "commit" },
		CommitAllStore:      func(string, string) string { return "commit" },
		Push:                func(string) gitx.ExecResult { return gitx.ExecResult{OK: true} },
		GitPush:             func(string) gitx.ExecResult { return gitx.ExecResult{OK: true} },
	}
}

func TestPushCreatesInitialBaselineOnEmptyRemote(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	home := filepath.Join(root, "homer")
	tool := filepath.Join(root, "tool")
	if result := gitx.Exec(root, []string{"init", "--bare", "-b", "main", origin}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if result := gitx.Exec(root, []string{"init", "-b", "main", home}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	for _, args := range [][]string{{"config", "user.email", "w10@example.invalid"}, {"config", "user.name", "W10"}, {"remote", "add", "origin", origin}, {"config", "branch.main.remote", "origin"}, {"config", "branch.main.merge", "refs/heads/main"}} {
		if result := gitx.Exec(home, args, 0); !result.OK {
			t.Fatal(result.Stderr)
		}
	}
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return home
		}
		return os.Getenv(name)
	})
	config := writeTestConfig(paths, tool, core.SyncModeMirror)
	if err := os.MkdirAll(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool, "settings.json"), []byte("baseline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := core.WriteSnapshotToStore(paths, writeTestSnapshot("baseline\n", core.SyncModeMirror)[0]); err != nil {
		t.Fatal(err)
	}
	report := RunPush(PushOptions{HomerHome: home, Yes: true}, nil)
	if report.Status != PushStatusPushed || report.ExitCode() != 0 || !report.PushedToRemote {
		t.Fatalf("initial baseline report = %#v", report)
	}
	if gitx.HeadCommit(home) == "" {
		t.Fatal("initial baseline did not create a local commit")
	}
	if got := gitx.Exec(origin, []string{"show", "main:store/pi/settings/settings.json"}, 0); !got.OK || strings.TrimSpace(got.Stdout) != "baseline" {
		t.Fatalf("origin missing baseline store: %#v", got)
	}
	if state := core.LoadState(paths); state.LastSyncCommit != gitx.HeadCommit(home) {
		t.Fatalf("state baseline = %q, head = %q", state.LastSyncCommit, gitx.HeadCommit(home))
	}
	_ = config
}

func TestPullS4KeepsPreFastForwardBaseAndJSONCounts(t *testing.T) {
	paths := writeTestPaths(t)
	config := writeTestConfig(paths, filepath.Join(paths.Home, "tool"), core.SyncModeMirror)
	tool := filepath.Join(paths.Home, "tool", "settings.json")
	if err := os.MkdirAll(filepath.Dir(tool), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tool, []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sources := syncx.SyncSources{
		Base:   writeTestSnapshot("base\n", core.SyncModeMirror),
		Local:  writeTestSnapshot("local\n", core.SyncModeMirror),
		Remote: writeTestSnapshot("remote\n", core.SyncModeMirror),
	}
	git := fakeWriteGit("base-commit", "remote-commit")
	report := RunPull(PullOptions{HomerHome: paths.Home, Yes: true}, &PullDeps{Sources: sources, NoFetch: true, Git: git})
	if report.Status != PullStatusConflictsRemain || report.ExitCode() != 1 {
		t.Fatalf("pull report = %#v", report)
	}
	state := core.LoadState(paths)
	if state.LastSyncCommit != "base-commit" {
		t.Fatalf("S4 advanced base to %q", state.LastSyncCommit)
	}
	if got, err := os.ReadFile(tool); err != nil || string(got) != "local\n" {
		t.Fatalf("conflict changed local tool: %q, %v", got, err)
	}
	if got := RenderPullReport(report); !strings.Contains(got, "写入: 0  删除: 0  冲突: 1") {
		t.Fatalf("pull text count = %q", got)
	}
	if !json.Valid([]byte(RenderPullJSON(report))) {
		t.Fatal("pull JSON is invalid")
	}
	_ = config
}

func TestMergeSecretGateDoesNotWriteStore(t *testing.T) {
	paths := writeTestPaths(t)
	writeTestConfig(paths, filepath.Join(paths.Home, "tool"), core.SyncModeMerge)
	tool := filepath.Join(paths.Home, "tool", "settings.json")
	if err := os.MkdirAll(filepath.Dir(tool), 0o755); err != nil {
		t.Fatal(err)
	}
	local := `{"name":"local","token":"sk-ant-abcdefghijklmnopqrstuvwxyz1234567890"}`
	if err := os.WriteFile(tool, []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	sources := syncx.SyncSources{
		Base:   writeTestSnapshot(`{"name":"base"}`, core.SyncModeMerge),
		Local:  writeTestSnapshot(local, core.SyncModeMerge),
		Remote: writeTestSnapshot(`{"name":"remote"}`, core.SyncModeMerge),
	}
	git := fakeWriteGit("base-commit", "remote-commit")
	report := RunMerge(MergeOptions{HomerHome: paths.Home, AcceptLocal: true}, &MergeDeps{Sources: sources, NoFetch: true, Git: git})
	if report.Status != MergeStatusError || report.ExitCode() != 1 {
		t.Fatalf("merge report = %#v", report)
	}
	if len(report.Errors) == 0 || !strings.Contains(strings.Join(report.Errors, "\n"), "拒绝写入 store") {
		t.Fatalf("missing secret gate error: %#v", report.Errors)
	}
	if _, err := os.Stat(paths.StoreDir); !os.IsNotExist(err) {
		t.Fatalf("secret-gated merge wrote store: %v", err)
	}
	if !json.Valid([]byte(RenderMergeJSON(report))) {
		t.Fatal("merge JSON is invalid")
	}
}

func TestHomeRejectsUnsafeURLBeforeClone(t *testing.T) {
	paths := writeTestPaths(t)
	called := false
	report := RunHome(HomeOptions{HomerHome: paths.Home, RepoURL: "--upload-pack=evil", Yes: true}, &HomeDeps{
		Clone: func(_, _ string) error { called = true; return nil },
	})
	if report.Status != HomeStatusError || report.ExitCode() != 1 || called {
		t.Fatalf("unsafe home report = %#v, clone called=%v", report, called)
	}
	if !strings.Contains(strings.Join(report.Errors, "\n"), "不得以") {
		t.Fatalf("unsafe URL message = %#v", report.Errors)
	}
	if !json.Valid([]byte(RenderHomeJSON(report))) {
		t.Fatal("home JSON is invalid")
	}
}
