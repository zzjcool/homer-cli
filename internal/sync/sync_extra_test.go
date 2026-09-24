package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

func TestExcludedKeysMatrixAndPlanting(t *testing.T) {
	config := core.HomerConfig{Adapters: map[string]core.AdapterConfig{
		"pi": {Categories: map[string]core.CategoryConfig{
			"settings": {Mode: core.SyncModeMerge, ExcludeKeys: []string{"apiKeys", "token"}},
		}},
	}}
	local := core.AdapterSnapshot{AdapterID: "pi", Categories: []core.CategorySnapshot{{
		AdapterID: "pi", Category: "settings", Mode: core.SyncModeMerge,
		Files: core.SnapshotFiles{
			"settings.json": {Kind: "json", Content: `{"apiKeys":"secret","nested":{"apiKeys":"keep"},"token":"t","model":"x"}`},
			"broken.json":   {Kind: "json", Content: "broken"},
			"plain.txt":     {Kind: "file", Content: "apiKeys=secret"},
			"array.json":    {Kind: "json", Content: `["apiKeys",{"apiKeys":1}]`},
		},
	}}}
	prepared := ApplyExcludeKeyPlaceholders(local, config)
	settings := prepared.Categories[0].Files["settings.json"].Content
	if strings.Contains(settings, `"secret"`) || !strings.Contains(settings, `"__REQUIRED__"`) || !strings.Contains(settings, `"nested"`) {
		t.Fatalf("placeholder replacement = %s", settings)
	}
	if got := prepared.Categories[0].Files["broken.json"].Content; got != "broken" {
		t.Fatalf("broken entry changed: %q", got)
	}
	if got := prepared.Categories[0].Files["plain.txt"].Content; got != "apiKeys=secret" {
		t.Fatalf("file entry changed: %q", got)
	}
	if got := prepared.Categories[0].Files["array.json"].Content; got != `["apiKeys",{"apiKeys":1}]` {
		t.Fatalf("array entry changed: %q", got)
	}

	stripped := StripSnapshotExcludeKeys(local, config)
	strippedJSON := stripped.Categories[0].Files["settings.json"].Content
	if strings.Contains(strippedJSON, `"apiKeys":"secret"`) || strings.Contains(strippedJSON, `"token"`) || !strings.Contains(strippedJSON, `"nested":{"apiKeys":"keep"}`) {
		t.Fatalf("stripped content = %s", strippedJSON)
	}
	if local.Categories[0].Files["settings.json"].Content == strippedJSON {
		t.Fatal("strip mutated or failed to copy local snapshot")
	}

	merged, err := orderedjson.Parse([]byte(`{"model":"remote"}`))
	if err != nil {
		t.Fatal(err)
	}
	localValue, err := orderedjson.Parse([]byte(`{"apiKeys":"secret","model":"local"}`))
	if err != nil {
		t.Fatal(err)
	}
	planted := PlantExcludedKeys(merged, localValue, []string{"apiKeys"})
	if got := string(orderedjson.Serialize(planted)); !strings.Contains(got, `"apiKeys": "secret"`) {
		t.Fatalf("planted = %s", got)
	}
	missing, err := orderedjson.Parse([]byte(`{"model":"local"}`))
	if err != nil {
		t.Fatal(err)
	}
	withoutLocalKey := PlantExcludedKeys(merged, missing, []string{"apiKeys"})
	if strings.Contains(string(orderedjson.Serialize(withoutLocalKey)), "apiKeys") {
		t.Fatal("missing local excluded key was invented")
	}
}

func TestCheckPushSafetyBranchesAndChangedFiles(t *testing.T) {
	config := syncTestConfig(core.SyncModeMerge, "settings.json")
	base := syncSnapshot("pi", "settings", core.SyncModeMerge, map[string]core.SnapshotEntry{
		"settings.json": syncJSONEntry(`{"a":1,"b":1}`),
		"a:b.json":      syncJSONEntry(`{"x":1}`),
	})
	localChanged := syncSnapshot("pi", "settings", core.SyncModeMerge, map[string]core.SnapshotEntry{
		"settings.json": syncJSONEntry(`{"a":2,"b":1}`),
		"a:b.json":      syncJSONEntry(`{"x":2}`),
	})
	remoteSame := base
	check := CheckPushSafety(config, []core.AdapterSnapshot{base}, []core.AdapterSnapshot{localChanged}, []core.AdapterSnapshot{remoteSame})
	if check.Status != PushStatusOK || len(check.ChangedFiles) != 2 {
		t.Fatalf("ok push check = %#v", check)
	}
	if check.ChangedFiles[1].RelPath != "a:b.json" && check.ChangedFiles[0].RelPath != "a:b.json" {
		t.Fatalf("colon path was not recovered: %#v", check.ChangedFiles)
	}

	remoteChanged := syncSnapshot("pi", "settings", core.SyncModeMerge, map[string]core.SnapshotEntry{
		"settings.json": syncJSONEntry(`{"a":1,"b":2}`),
		"a:b.json":      syncJSONEntry(`{"x":1}`),
	})
	check = CheckPushSafety(config, []core.AdapterSnapshot{base}, []core.AdapterSnapshot{base}, []core.AdapterSnapshot{remoteChanged})
	if check.Status != PushStatusRemoteAhead || len(check.RemoteAheadFiles) != 1 || check.RemoteAheadFiles[0].RelPath != "settings.json" {
		t.Fatalf("remote-ahead check = %#v", check)
	}

	conflictingRemote := syncSnapshot("pi", "settings", core.SyncModeMerge, map[string]core.SnapshotEntry{
		"settings.json": syncJSONEntry(`{"a":3,"b":1}`),
		"a:b.json":      syncJSONEntry(`{"x":1}`),
	})
	check = CheckPushSafety(config, []core.AdapterSnapshot{base}, []core.AdapterSnapshot{localChanged}, []core.AdapterSnapshot{conflictingRemote})
	if check.Status != PushStatusConflicts || len(check.ConflictItems) != 1 || check.ConflictItems[0].RelPath != "settings.json" {
		t.Fatalf("conflict check = %#v", check)
	}
}

func TestApplyPullActionsPhasesBackupDeleteAndConflict(t *testing.T) {
	home := t.TempDir()
	toolRoot := filepath.Join(home, "tool")
	homerRoot := filepath.Join(home, "homer")
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return homerRoot
		}
		return ""
	})
	config := core.HomerConfig{Adapters: map[string]core.AdapterConfig{"pi": {
		Root: toolRoot,
		Categories: map[string]core.CategoryConfig{
			"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMerge},
			"skills":   {Paths: []string{"skills/"}, Mode: core.SyncModeMirror},
		},
	}}}
	writeSyncFile(t, filepath.Join(toolRoot, "settings.json"), "old-settings")
	writeSyncFile(t, filepath.Join(toolRoot, "skills", "foo", "SKILL.md"), "old-skill")
	plan := PullPlan{Actions: []PullAction{
		{Type: PullActionWrite, AdapterID: "pi", Category: "settings", RelPath: "settings.json", Content: "new-settings"},
		{Type: PullActionDelete, AdapterID: "pi", Category: "skills", RelPath: "foo/SKILL.md"},
		{Type: PullActionConflict, AdapterID: "pi", Category: "skills", RelPath: "keep/SKILL.md", Reason: "modify-vs-modify"},
	}}
	writeSyncFile(t, filepath.Join(toolRoot, "skills", "keep", "SKILL.md"), "keep-local")
	got, err := ApplyPullActions(paths, config, plan, ApplyPullActionsOptions{Command: "merge"})
	if err != nil {
		t.Fatal(err)
	}
	if got.BackupDir == "" || !strings.Contains(filepath.Base(got.BackupDir), "-merge") {
		t.Fatalf("backup dir = %q", got.BackupDir)
	}
	if content, err := os.ReadFile(filepath.Join(toolRoot, "settings.json")); err != nil || string(content) != "new-settings" {
		t.Fatalf("written settings = %q, err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(toolRoot, "skills", "foo")); !os.IsNotExist(err) {
		t.Fatalf("empty parent was not pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(toolRoot, "skills")); err != nil {
		t.Fatalf("category root was pruned: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(toolRoot, "skills", "keep", "SKILL.md")); err != nil || string(content) != "keep-local" {
		t.Fatalf("conflict changed local content = %q, err=%v", content, err)
	}
	backupFiles := make([]string, 0, 2)
	if err := filepath.Walk(got.BackupDir, func(filename string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			backupFiles = append(backupFiles, filename)
		}
		return nil
	}); err != nil || len(backupFiles) != 2 {
		t.Fatalf("backup files = %v, err=%v", backupFiles, err)
	}
	for _, backupFile := range backupFiles {
		content, err := os.ReadFile(backupFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "old-settings" && string(content) != "old-skill" {
			t.Fatalf("unexpected backup %s = %q", backupFile, content)
		}
	}
}

func TestApplyPullActionsValidatesWholeBatchBeforeWrite(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "tool")
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return filepath.Join(home, "homer")
		}
		return ""
	})
	config := syncTestConfig(core.SyncModeMirror, "notes/")
	config.Adapters["pi"] = core.AdapterConfig{Root: root, Categories: config.Adapters["pi"].Categories}
	plan := PullPlan{Actions: []PullAction{
		{Type: PullActionWrite, AdapterID: "pi", Category: "notes", RelPath: "new.txt", Content: "must-not-write"},
		{Type: PullActionWrite, AdapterID: "pi", Category: "missing", RelPath: "x", Content: "bad"},
	}}
	if _, err := ApplyPullActions(paths, config, plan, ApplyPullActionsOptions{Backup: false}); err == nil {
		t.Fatal("invalid batch unexpectedly applied")
	}
	if _, err := os.Stat(filepath.Join(root, "notes", "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("valid action was applied before batch error: %v", err)
	}
}

func TestCollectSyncSourcesFallbackMatrix(t *testing.T) {
	// No state/no repository: base and remote use the store worktree.
	home := t.TempDir()
	tool := filepath.Join(home, "tool")
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return filepath.Join(home, "homer")
		}
		return ""
	})
	config := syncTestConfig(core.SyncModeMerge, "settings.json")
	config.Adapters["pi"] = core.AdapterConfig{Root: tool, Categories: config.Adapters["pi"].Categories}
	store := syncSnapshot("pi", "settings", core.SyncModeMerge, map[string]core.SnapshotEntry{"settings.json": syncJSONEntry(`{"store":1}`)})
	if err := core.WriteSnapshotToStore(paths, store); err != nil {
		t.Fatal(err)
	}
	writeSyncFile(t, filepath.Join(tool, "settings.json"), `{"local":1}`)
	sources := CollectSyncSources(paths, config, CollectSyncSourcesOptions{Fetch: false})
	if sources.Mode != SyncBaseModeStore || len(sources.Base) != 1 || len(sources.Remote) != 1 || sources.Remote[0].Categories[0].Files["settings.json"].Content != `{"store":1}` {
		t.Fatalf("store fallback sources = %#v", sources)
	}

	// Historical state commit is preferred over the mutable store worktree.
	if err := gitx.EnsureGitRepo(paths.Home); err != nil {
		t.Fatal(err)
	}
	mustSyncGit(t, paths.Home, "config", "--local", "user.email", "homer-test@example.invalid")
	mustSyncGit(t, paths.Home, "config", "--local", "user.name", "Homer Test")
	if commit := gitx.CommitAllStore(paths.Home, "baseline"); commit == "" {
		t.Fatal("baseline commit failed")
	} else {
		state := core.HomerState{Version: 1, LastSyncCommit: commit}
		if err := core.SaveState(paths, state); err != nil {
			t.Fatal(err)
		}
	}
	writeSyncFile(t, filepath.Join(paths.StoreDir, "pi", "settings", "settings.json"), `{"store":2}`)
	sources = CollectSyncSources(paths, config, CollectSyncSourcesOptions{Fetch: false})
	if sources.Mode != SyncBaseModeGit || sources.BaseCommit == "" || sources.Base[0].Categories[0].Files["settings.json"].Content != `{"store":1}` {
		t.Fatalf("git base fallback = %#v", sources)
	}

	// An unreadable adapter root is not interpreted as a full local deletion.
	config.Adapters["pi"] = core.AdapterConfig{Root: filepath.Join(home, "missing-root"), Categories: config.Adapters["pi"].Categories}
	sources = CollectSyncSources(paths, config, CollectSyncSourcesOptions{Fetch: false})
	if sources.Local[0].Categories[0].Files["settings.json"].Content != sources.Base[0].Categories[0].Files["settings.json"].Content || len(sources.Errors) == 0 {
		t.Fatalf("root unreadable guard failed: %#v", sources)
	}

	// An upstream commit with no store tree falls back to the base instead of
	// interpreting the absent tree as remote deletion (S2).
	remoteHome := t.TempDir()
	remotePaths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return filepath.Join(remoteHome, "homer")
		}
		return ""
	})
	remoteConfig := syncTestConfig(core.SyncModeMerge, "settings.json")
	remoteConfig.Adapters["pi"] = core.AdapterConfig{Root: filepath.Join(remoteHome, "tool"), Categories: remoteConfig.Adapters["pi"].Categories}
	if err := core.WriteSnapshotToStore(remotePaths, store); err != nil {
		t.Fatal(err)
	}
	if err := gitx.EnsureGitRepo(remotePaths.Home); err != nil {
		t.Fatal(err)
	}
	mustSyncGit(t, remotePaths.Home, "branch", "-M", "main")
	mustSyncGit(t, remotePaths.Home, "config", "--local", "user.email", "homer-test@example.invalid")
	mustSyncGit(t, remotePaths.Home, "config", "--local", "user.name", "Homer Test")
	writeSyncFile(t, filepath.Join(remotePaths.Home, "homer.json"), "{}\n")
	remoteCommit := gitx.CommitPaths(remotePaths.Home, []string{"homer.json"}, "configuration only")
	if remoteCommit == "" {
		t.Fatal("configuration commit failed")
	}
	mustSyncGit(t, remotePaths.Home, "update-ref", "refs/remotes/origin/main", remoteCommit)
	mustSyncGit(t, remotePaths.Home, "config", "--local", "branch.main.remote", "origin")
	mustSyncGit(t, remotePaths.Home, "config", "--local", "branch.main.merge", "refs/heads/main")
	sources = CollectSyncSources(remotePaths, remoteConfig, CollectSyncSourcesOptions{Fetch: false})
	if sources.Remote[0].Categories[0].Files["settings.json"].Content != `{"store":1}` || len(sources.Warnings) == 0 {
		t.Fatalf("missing remote store fallback = %#v", sources)
	}

	// A fetch failure has the same safe remote:=base fallback (four-row
	// degradation matrix), while retaining the upstream diagnostic.
	mustSyncGit(t, remotePaths.Home, "remote", "add", "origin", filepath.Join(remoteHome, "does-not-exist.git"))
	sources = CollectSyncSources(remotePaths, remoteConfig)
	if sources.Remote[0].Categories[0].Files["settings.json"].Content != `{"store":1}` || !containsSyncWarning(sources.Warnings, "git fetch 失败") {
		t.Fatalf("fetch failure fallback = %#v", sources)
	}
}

func containsSyncWarning(warnings []string, text string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, text) {
			return true
		}
	}
	return false
}

func syncTestConfig(mode core.SyncMode, path string) core.HomerConfig {
	return core.HomerConfig{Adapters: map[string]core.AdapterConfig{"pi": {
		Categories: map[string]core.CategoryConfig{"settings": {Paths: []string{path}, Mode: mode}},
	}}}
}

func syncSnapshot(adapter, category string, mode core.SyncMode, files map[string]core.SnapshotEntry) core.AdapterSnapshot {
	return core.AdapterSnapshot{AdapterID: adapter, Categories: []core.CategorySnapshot{{AdapterID: adapter, Category: category, Mode: mode, Files: files}}}
}

func syncJSONEntry(content string) core.SnapshotEntry {
	return core.SnapshotEntry{Kind: "json", Content: content}
}

func writeSyncFile(t *testing.T, filename, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSyncGit(t *testing.T, home, command string, args ...string) {
	t.Helper()
	all := append([]string{command}, args...)
	result := gitx.Exec(home, all, 0)
	if !result.OK {
		t.Fatalf("git %v: %s", all, result.Stderr)
	}
}
