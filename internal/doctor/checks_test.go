package doctor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/agecrypto"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
)

func testPaths(t *testing.T) core.HomerPaths {
	t.Helper()
	return core.GetHomerPaths(func(string) string { return t.TempDir() })
}

func writeConfig(t *testing.T, paths core.HomerPaths, config core.HomerConfig) {
	t.Helper()
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
}

func minimalConfig(root string) core.HomerConfig {
	return core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"pi": {
				Root: root,
				Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMerge},
				},
			},
		},
	}
}

func TestAllDoctorChecksCoverOKWarnFail(t *testing.T) {
	paths := testPaths(t)
	if got := CheckConfig(paths); got.Status != CheckFail {
		t.Fatalf("missing config status = %s", got.Status)
	}
	if got := CheckRepoAndStore(paths); got[0].Status != CheckFail || got[1].Status != CheckWarn {
		t.Fatalf("non-repo checks = %#v", got)
	}
	if got := CheckRemote(paths, RemoteCheckOptions{Offline: true}); got.Status != CheckOK || !strings.Contains(got.Message, "跳过") {
		t.Fatalf("offline remote = %#v", got)
	}

	root := filepath.Join(paths.Home, "adapter")
	config := minimalConfig(root)
	writeConfig(t, paths, config)
	if got := CheckConfig(paths); got.Status != CheckOK {
		t.Fatalf("valid config status = %#v", got)
	}
	if got := CheckAdapters(config); got.Status != CheckWarn {
		t.Fatalf("missing adapter root = %#v", got)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := CheckAdapters(config); got.Status != CheckOK {
		t.Fatalf("existing adapter root = %#v", got)
	}

	identity := agecrypto.GenerateIdentity()
	if got := CheckAge(paths, core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{}, Secrets: &core.SecretsConfig{Files: map[string]string{"token": filepath.Join(paths.Home, "token")}}}, nil); got.Status != CheckFail {
		t.Fatalf("missing identity age = %#v", got)
	}
	if err := agecrypto.WriteIdentityFile(paths, identity); err != nil {
		t.Fatal(err)
	}
	ageConfig := core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{}, Secrets: &core.SecretsConfig{Recipients: []string{identity.Recipient}, Files: map[string]string{"token": filepath.Join(paths.Home, "token")}}}
	if got := CheckAge(paths, ageConfig, nil); got.Status != CheckWarn {
		t.Fatalf("missing vault age = %#v", got)
	}

	if got := CheckMachine(paths); got.Status != CheckWarn {
		t.Fatalf("missing state machine = %#v", got)
	}
	if err := gitx.EnsureGitRepo(paths.Home); err != nil {
		t.Fatal(err)
	}
	if got := CheckMachine(paths); got.Status != CheckWarn {
		t.Fatalf("unborn machine = %#v", got)
	}

	storeConfig := minimalConfig(root)
	if err := core.WriteSnapshotToStore(paths, core.AdapterSnapshot{
		AdapterID: "pi",
		Categories: []core.CategorySnapshot{{
			AdapterID: "pi", Category: "settings", Mode: core.SyncModeMerge,
			Files: core.SnapshotFiles{"settings.json": {Kind: "json", Content: "{\"token\":\"__REQUIRED__\",\"z\":\"__REQUIRED__\",\"nested\":{\"x\":\"__REQUIRED__\"}}\n"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	required := CheckRequiredPlaceholders(paths, storeConfig)
	if required.Status != CheckWarn || !bytes.Equal([]byte(required.Details[0]), []byte("pi/settings/settings.json: token, z")) {
		t.Fatalf("required = %#v", required)
	}
}

func TestDoctorHealthyChecks(t *testing.T) {
	paths := testPaths(t)
	root := filepath.Join(paths.Home, "adapter")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	identity := agecrypto.GenerateIdentity()
	secretConfig := minimalConfig(root)
	secretConfig.Secrets = &core.SecretsConfig{
		Recipients: []string{identity.Recipient},
		Files:      map[string]string{"token": filepath.Join(paths.Home, "token")},
	}
	writeConfig(t, paths, secretConfig)
	if err := core.WriteSnapshotToStore(paths, core.AdapterSnapshot{
		AdapterID: "pi",
		Categories: []core.CategorySnapshot{{
			AdapterID: "pi", Category: "settings", Mode: core.SyncModeMerge,
			Files: core.SnapshotFiles{"settings.json": {Kind: "json", Content: "{\"theme\":\"dark\"}\n"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := gitx.EnsureGitRepo(paths.Home); err != nil {
		t.Fatal(err)
	}
	if result := gitx.Exec(paths.Home, []string{"branch", "-M", "main"}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if result := gitx.Exec(paths.Home, []string{"config", "user.email", "doctor@example.invalid"}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if result := gitx.Exec(paths.Home, []string{"config", "user.name", "Doctor Test"}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if commit := gitx.CommitAllStore(paths.Home, "baseline"); commit == "" {
		t.Fatal("baseline commit missing")
	}
	origin := filepath.Join(filepath.Dir(paths.Home), "origin.git")
	if result := gitx.Exec(filepath.Dir(origin), []string{"init", "--bare", "-b", "main", origin}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if result := gitx.Exec(paths.Home, []string{"remote", "add", "origin", origin}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if result := gitx.Exec(paths.Home, []string{"push", "-u", "origin", "main"}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if err := agecrypto.WriteIdentityFile(paths, identity); err != nil {
		t.Fatal(err)
	}
	if err := agecrypto.EncryptSecretToFile(nil, paths, "token", []byte("doctor-secret-content\n"), []string{identity.Recipient}); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveState(paths, core.HomerState{Version: 1, LastSyncCommit: gitx.HeadCommit(paths.Home)}); err != nil {
		t.Fatal(err)
	}

	if repo := CheckRepoAndStore(paths); repo[0].Status != CheckOK || repo[1].Status != CheckOK {
		t.Fatalf("healthy repo/store = %#v", repo)
	}
	if remote := CheckRemote(paths, RemoteCheckOptions{}); remote.Status != CheckOK {
		t.Fatalf("healthy remote = %#v", remote)
	}
	if age := CheckAge(paths, secretConfig, nil); age.Status != CheckOK {
		t.Fatalf("healthy age = %#v", age)
	}
	if machine := CheckMachine(paths); machine.Status != CheckOK {
		t.Fatalf("healthy machine = %#v", machine)
	}
	if required := CheckRequiredPlaceholders(paths, secretConfig); required.Status != CheckOK {
		t.Fatalf("healthy required = %#v", required)
	}
}

func TestDoctorRemoteReachabilityAndStoreDirty(t *testing.T) {
	paths := testPaths(t)
	if err := gitx.EnsureGitRepo(paths.Home); err != nil {
		t.Fatal(err)
	}
	if got := CheckRemote(paths, RemoteCheckOptions{}); got.Status != CheckWarn {
		t.Fatalf("no-upstream remote = %#v", got)
	}
	if err := os.MkdirAll(filepath.Join(paths.Home, "store", "pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Home, "store", "pi", "dirty"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := CheckRepoAndStore(paths)
	if got[1].Status != CheckWarn || !strings.Contains(strings.Join(got[1].Details, "\n"), "store/pi/dirty") {
		t.Fatalf("dirty store = %#v", got[1])
	}
}
