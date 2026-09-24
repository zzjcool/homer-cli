package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
)

type homeTestFixture struct {
	config     core.HomerConfig
	snapshot   core.AdapterSnapshot
	toolRoot   string
	secretName string
}

func newHomeTestFixture(t *testing.T, root string) homeTestFixture {
	t.Helper()
	toolRoot := filepath.Join(root, "tool")
	if err := os.MkdirAll(toolRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return homeTestFixture{
		config: core.HomerConfig{
			Version: 1,
			Adapters: map[string]core.AdapterConfig{
				"pi": {
					Root: toolRoot,
					Categories: map[string]core.CategoryConfig{
						"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
					},
				},
			},
			Secrets: &core.SecretsConfig{
				Files: map[string]string{"one": filepath.Join(root, "secret.txt")},
			},
		},
		snapshot: core.AdapterSnapshot{
			AdapterID: "pi",
			Categories: []core.CategorySnapshot{{
				AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror,
				Files: core.SnapshotFiles{"settings.json": {Kind: "file", Content: "remote\n"}},
			}},
		},
		toolRoot:   toolRoot,
		secretName: "one",
	}
}

func populateHomeClone(t *testing.T, fixture homeTestFixture, dest string) {
	t.Helper()
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return dest
		}
		return ""
	})
	if err := core.SaveConfig(paths, fixture.config); err != nil {
		t.Fatal(err)
	}
	if err := core.WriteSnapshotToStore(paths, fixture.snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestRunHomeSetupFailuresWithInjectedClone(t *testing.T) {
	tests := []struct {
		name           string
		setup          func(t *testing.T, target string)
		want           string
		wantCloneCalls int
		assert         func(t *testing.T, report HomeReport, target string)
	}{
		{
			name: "non-empty target rejects before clone",
			setup: func(t *testing.T, target string) {
				t.Helper()
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "keep"), []byte("local"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want:           "目标目录非空",
			wantCloneCalls: 0,
		},
		{
			name: "clone failure has no follow-up write",
			setup: func(t *testing.T, target string) {
				t.Helper()
				_ = target
			},
			want:           "clone failed",
			wantCloneCalls: 1,
			assert: func(t *testing.T, report HomeReport, target string) {
				t.Helper()
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatalf("clone failure wrote target: %v", err)
				}
				if report.Cloned {
					t.Fatal("failed clone marked cloned")
				}
			},
		},
		{
			name: "clone without homer config is rejected",
			setup: func(t *testing.T, target string) {
				t.Helper()
				_ = target
			},
			want:           "不是 homer 配置中心",
			wantCloneCalls: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "homer")
			test.setup(t, target)
			cloneCalls := 0
			report := RunHome(HomeOptions{HomerHome: target, RepoURL: "fixture", Yes: true}, &HomeDeps{
				Clone: func(_, dest string) error {
					cloneCalls++
					if test.name == "clone failure has no follow-up write" {
						return errors.New("clone failed: fixture unavailable")
					}
					if test.name == "clone without homer config is rejected" {
						if err := os.MkdirAll(dest, 0o755); err != nil {
							return err
						}
						return os.WriteFile(filepath.Join(dest, "README.md"), []byte("not homer"), 0o644)
					}
					return nil
				},
			})
			if cloneCalls != test.wantCloneCalls {
				t.Fatalf("clone calls = %d, want %d; report=%#v", cloneCalls, test.wantCloneCalls, report)
			}
			if !strings.Contains(strings.Join(report.Errors, "\n"), test.want) {
				t.Fatalf("report errors = %#v, want %q", report.Errors, test.want)
			}
			if test.assert != nil {
				test.assert(t, report, target)
			}
		})
	}
}

func TestRunHomeIdentityMissingStillHomed(t *testing.T) {
	root := t.TempDir()
	fixture := newHomeTestFixture(t, root)
	target := filepath.Join(root, "homer")
	report := RunHome(HomeOptions{HomerHome: target, RepoURL: "fixture", Yes: true}, &HomeDeps{
		Clone: func(_, dest string) error {
			populateHomeClone(t, fixture, dest)
			return nil
		},
	})
	if report.Status != HomeStatusHomed || report.ExitCode() != 0 || !report.OK {
		t.Fatalf("home report = %#v", report)
	}
	if len(report.Secrets.Skipped) != 1 || report.Secrets.Skipped[0] != fixture.secretName {
		t.Fatalf("skipped secrets = %#v", report.Secrets.Skipped)
	}
	warnings := strings.Join(report.Warnings, "\n")
	if !strings.Contains(warnings, "未找到本机 age identity") || !strings.Contains(warnings, "homer secret keygen") {
		t.Fatalf("identity warning = %q", warnings)
	}
	if content, err := os.ReadFile(filepath.Join(fixture.toolRoot, "settings.json")); err != nil || string(content) != "remote\n" {
		t.Fatalf("home did not apply config: %q, %v", content, err)
	}
}

func TestRunHomeCreatesMissingEnabledAdapterRoots(t *testing.T) {
	root := t.TempDir()
	osHome := filepath.Join(root, "os-home")
	if err := os.MkdirAll(osHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", osHome)

	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"pi": {
				Root: "~/.pi/agent",
				Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
				},
			},
			"herdr": {
				Root: "~/.config/herdr",
				Categories: map[string]core.CategoryConfig{
					"config": {Paths: []string{"config.toml"}, Mode: core.SyncModeMirror},
				},
			},
			"opencode": {
				Root: "~/.config/opencode",
				Categories: map[string]core.CategoryConfig{
					"config": {Paths: []string{"opencode.json"}, Mode: core.SyncModeMirror},
				},
			},
		},
	}
	snapshots := []core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{{AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{
			"settings.json": {Kind: "file", Content: "pi\n"},
		}}}},
		{AdapterID: "herdr", Categories: []core.CategorySnapshot{{AdapterID: "herdr", Category: "config", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{
			"config.toml": {Kind: "file", Content: "herdr\n"},
		}}}},
		{AdapterID: "opencode", Categories: []core.CategorySnapshot{{AdapterID: "opencode", Category: "config", Mode: core.SyncModeMirror, Files: core.SnapshotFiles{
			"opencode.json": {Kind: "file", Content: "opencode\n"},
		}}}},
	}
	target := filepath.Join(root, "homer")
	report := RunHome(HomeOptions{HomerHome: target, RepoURL: "fixture", Yes: true}, &HomeDeps{
		Clone: func(_, dest string) error {
			paths := core.GetHomerPaths(func(name string) string {
				if name == "HOMER_HOME" {
					return dest
				}
				return ""
			})
			if err := core.SaveConfig(paths, config); err != nil {
				return err
			}
			for _, snapshot := range snapshots {
				if err := core.WriteSnapshotToStore(paths, snapshot); err != nil {
					return err
				}
			}
			return nil
		},
	})
	if report.Status != HomeStatusHomed || report.FirstContact == nil {
		t.Fatalf("home report = %#v", report)
	}
	if written := len(report.FirstContact.Applied.Written); written != 3 {
		t.Fatalf("home writes = %d, want 3; report=%#v", written, report)
	}
	for _, adapterConfig := range config.Adapters {
		root := core.ExpandHome(adapterConfig.Root)
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			t.Fatalf("adapter root %s was not created: %v", root, err)
		}
	}
	for _, expected := range []struct {
		path    string
		content string
	}{
		{filepath.Join(osHome, ".pi", "agent", "settings.json"), "pi\n"},
		{filepath.Join(osHome, ".config", "herdr", "config.toml"), "herdr\n"},
		{filepath.Join(osHome, ".config", "opencode", "opencode.json"), "opencode\n"},
	} {
		if content, err := os.ReadFile(expected.path); err != nil || string(content) != expected.content {
			t.Fatalf("home write %s = %q, err=%v", expected.path, content, err)
		}
	}
}

func TestRunHomeAdapterRootCreationFailureWarns(t *testing.T) {
	root := t.TempDir()
	osHome := filepath.Join(root, "os-home")
	if err := os.MkdirAll(osHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", osHome)
	blocked := filepath.Join(osHome, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"pi": {
				Root: filepath.Join("~", "blocked", "agent"),
				Categories: map[string]core.CategoryConfig{
					"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMirror},
				},
			},
		},
	}
	snapshot := core.AdapterSnapshot{AdapterID: "pi", Categories: []core.CategorySnapshot{{
		AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror,
		Files: core.SnapshotFiles{"settings.json": {Kind: "file", Content: "remote\n"}},
	}}}
	target := filepath.Join(root, "homer")
	report := RunHome(HomeOptions{HomerHome: target, RepoURL: "fixture", Yes: true}, &HomeDeps{
		Clone: func(_, dest string) error {
			paths := core.GetHomerPaths(func(name string) string {
				if name == "HOMER_HOME" {
					return dest
				}
				return ""
			})
			if err := core.SaveConfig(paths, config); err != nil {
				return err
			}
			return core.WriteSnapshotToStore(paths, snapshot)
		},
	})
	warnings := strings.Join(report.Warnings, "\n")
	if !strings.Contains(warnings, "adapter root 不可读") {
		t.Fatalf("root creation failure was silent: report=%#v", report)
	}
}

func TestRunHomeConfigFailureNamesTargetAndRemovalHint(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "homer")
	report := RunHome(HomeOptions{HomerHome: target, RepoURL: "fixture", Yes: true}, &HomeDeps{
		Clone: func(_, dest string) error {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			return nil
		},
	})
	message := strings.Join(report.Errors, "\n")
	if report.Status != HomeStatusError || !strings.Contains(message, "不是 homer 配置中心") {
		t.Fatalf("config failure report = %#v", report)
	}
	for _, want := range []string{"根目录缺少 homer.json", "旧版 homer 推送", "homer push --yes", "恢复步骤", target} {
		if !strings.Contains(message, want) {
			t.Fatalf("config failure guidance missing %q: %q", want, message)
		}
	}
}

func TestRunHomeRejectsCloneStrayFromRealGitClone(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if err := gitx.EnsureGitRepo(origin); err != nil {
		t.Fatal(err)
	}
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return origin
		}
		return ""
	})
	fixture := newHomeTestFixture(t, root)
	if err := core.SaveConfig(paths, fixture.config); err != nil {
		t.Fatal(err)
	}
	if err := core.WriteSnapshotToStore(paths, fixture.snapshot); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "user.email", "home-review@example.invalid"}, {"config", "user.name", "Home Review"}, {"add", "-A"}, {"commit", "-m", "fixture"}} {
		if result := gitx.Exec(origin, args, 0); !result.OK {
			t.Fatal(result.Stderr)
		}
	}

	template := filepath.Join(root, "template")
	if err := os.MkdirAll(filepath.Join(template, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(template, "hooks", "post-checkout")
	hookBody := "#!/bin/sh\nprintf '%s\\n' stray > \"$(dirname \"$GIT_DIR\")/stray.txt\"\n"
	if err := os.WriteFile(hook, []byte(hookBody), 0o755); err != nil {
		t.Fatal(err)
	}
	oldTemplate, hadTemplate := os.LookupEnv("GIT_TEMPLATE_DIR")
	if err := os.Setenv("GIT_TEMPLATE_DIR", template); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadTemplate {
			_ = os.Setenv("GIT_TEMPLATE_DIR", oldTemplate)
		} else {
			_ = os.Unsetenv("GIT_TEMPLATE_DIR")
		}
	})

	target := filepath.Join(root, "clone")
	report := RunHome(HomeOptions{HomerHome: target, RepoURL: origin, Yes: true}, nil)
	if report.Status != HomeStatusError || !strings.Contains(strings.Join(report.Errors, "\n"), "clone 后的工作区含仓库之外的条目") {
		t.Fatalf("stray clone report = %#v", report)
	}
	if _, err := os.Stat(filepath.Join(target, "stray.txt")); err != nil {
		t.Fatalf("fixture hook did not create stray file: %v", err)
	}
}
