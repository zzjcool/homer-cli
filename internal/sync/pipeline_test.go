package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
)

func TestRequireFastForwardableDivergenceIncludesRecoveryGuidance(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	if result := gitx.Exec(root, []string{"init", "--bare", "-b", "master", origin}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	local := filepath.Join(root, "local")
	if err := gitx.EnsureGitRepo(local); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "Divergence Test"},
		{"config", "user.email", "divergence@example.invalid"},
		{"remote", "add", "origin", origin},
	} {
		if result := gitx.Exec(local, args, 0); !result.OK {
			t.Fatal(result.Stderr)
		}
	}
	if result := gitx.Exec(local, []string{"branch", "-M", "master"}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	write := filepath.Join(local, "store", "pi", "settings.json")
	if err := writeFileForPipeline(write, "base\n"); err != nil {
		t.Fatal(err)
	}
	if result := gitx.Exec(local, []string{"add", "-A"}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if result := gitx.Exec(local, []string{"commit", "-m", "base"}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if result := gitx.Exec(local, []string{"push", "-u", "origin", "master"}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}

	other := filepath.Join(root, "other")
	if err := gitx.CloneRepo(origin, other, 0); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "user.name", "Divergence Test"}, {"config", "user.email", "divergence@example.invalid"}} {
		if result := gitx.Exec(other, args, 0); !result.OK {
			t.Fatal(result.Stderr)
		}
	}
	if err := writeFileForPipeline(filepath.Join(other, "store", "pi", "settings.json"), "remote\n"); err != nil {
		t.Fatal(err)
	}
	if result := gitx.Exec(other, []string{"add", "-A"}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if result := gitx.Exec(other, []string{"commit", "-m", "remote"}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if result := gitx.Exec(other, []string{"push"}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if err := writeFileForPipeline(write, "local\n"); err != nil {
		t.Fatal(err)
	}
	if result := gitx.Exec(local, []string{"add", "-A"}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if result := gitx.Exec(local, []string{"commit", "-m", "local"}, 0); !result.OK {
		t.Fatal(result.Stderr)
	}
	if result := gitx.Fetch(local); !result.OK {
		t.Fatal(result.Stderr)
	}

	paths := core.HomerPaths{Home: local}
	err := RequireFastForwardable(paths)
	if err == nil {
		t.Fatal("divergent repository unexpectedly fast-forwardable")
	}
	message := err.Error()
	for _, want := range []string{
		"两台机器都推送过",
		"① 在本机 homer push",
		"git -C " + local + " pull --rebase 后 homer merge",
		"保留两边",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("divergence error missing %q: %s", want, message)
		}
	}
}

func writeFileForPipeline(filename, content string) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filename, []byte(content), 0o644)
}
