package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kballard/go-shellquote"
)

const (
	v12VSCodeOne = "publisher.vscode.one"
	v12VSCodeTwo = "publisher.vscode.two"
	v12FakeOne   = "publisher.fake.one"
	v12FakeTwo   = "publisher.fake.two"
)

func v12WriteExecutable(t *testing.T, filename, content string) {
	t.Helper()
	writeFile(t, filename, content)
	if err := os.Chmod(filename, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", filename, err)
	}
}

func v12PathEnv(binDir string) string {
	return binDir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func v12CommandPath(filename string) string {
	return shellquote.Join(filename)
}

func v12RunHomer(t *testing.T, binary string, m machine, binDir string, args ...string) processResult {
	t.Helper()
	return runProcess(t, repoRoot(), m.gitGlobal, []string{
		"HOME=" + m.fakeHome,
		"HOMER_HOME=" + m.homerHome,
		"PATH=" + v12PathEnv(binDir),
	}, binary, args...)
}

func v12MustJSON(t *testing.T, result processResult, command string) map[string]any {
	t.Helper()
	if result.code != 0 {
		t.Fatalf("homer %s failed (exit %d):\nstdout:\n%s\nstderr:\n%s", command, result.code, result.stdout, result.stderr)
	}
	return parseE2EJSON(t, result, command)
}

func v12WriteCodeScript(t *testing.T, filename string) {
	t.Helper()
	v12WriteExecutable(t, filename, `#!/bin/sh
set -eu
case "${1:-}" in
  --list-extensions)
    if [ -f "$HOME/.fake-vscode-extensions" ]; then
      cat "$HOME/.fake-vscode-extensions"
    fi
    ;;
  --install-extension)
    [ "$#" -eq 2 ]
    printf '%s\n' "$2" >> "$HOME/.fake-vscode-install.log"
    ;;
  *)
    echo "unsupported fake code invocation" >&2
    exit 2
    ;;
esac
`)
}

func v12WriteFakeCLIScripts(t *testing.T, listScript, applyScript string) {
	t.Helper()
	v12WriteExecutable(t, listScript, `#!/bin/sh
set -eu
if [ -f "$HOME/.fakecli/plugins.list" ]; then
  cat "$HOME/.fakecli/plugins.list"
fi
`)
	v12WriteExecutable(t, applyScript, `#!/bin/sh
set -eu
[ "$#" -eq 1 ]
printf '%s\n' "$1" >> "$HOME/.fakecli/plugins.apply.log"
`)
}

func v12WriteVSCodeState(t *testing.T, m machine, ids []string) {
	t.Helper()
	writeFile(t, filepath.Join(m.fakeHome, ".config", "Code", "settings.json"), "{\n  \"editor.fontSize\": 14,\n  \"window.zoomLevel\": 1\n}\n")
	writeFile(t, filepath.Join(m.fakeHome, ".config", "Code", "keybindings.json"), "[\n  {\"key\": \"ctrl+k\", \"command\": \"workbench.action.files.openFile\"}\n]\n")
	writeFile(t, filepath.Join(m.fakeHome, ".fake-vscode-extensions"), strings.Join(ids, "\n")+"\n")
}

func v12WriteFakeCLIState(t *testing.T, m machine, ids []string) {
	t.Helper()
	writeFile(t, filepath.Join(m.fakeHome, ".fakecli", "plugins.list"), strings.Join(ids, "\n")+"\n")
}

func v12FakeCLIConfig(listCmd, applyCmd string) map[string]any {
	return map[string]any{
		"root": "~/.fakecli",
		"categories": map[string]any{
			"conf": map[string]any{
				"paths": []any{"config.toml"},
				"mode":  "mirror",
			},
			"plugins": map[string]any{
				"kind":     "manifest",
				"mode":     "mirror",
				"listCmd":  listCmd,
				"applyCmd": applyCmd,
			},
		},
	}
}

func v12AddFakeCLIConfig(t *testing.T, m machine, listCmd, applyCmd string) {
	t.Helper()
	patchConfig(t, m, func(config map[string]any) {
		adapters, ok := config["adapters"].(map[string]any)
		if !ok {
			t.Fatalf("homer.json adapters has type %T", config["adapters"])
		}
		adapters["fakecli"] = v12FakeCLIConfig(listCmd, applyCmd)
	})
}

func v12AdapterReport(t *testing.T, report map[string]any, id string) map[string]any {
	t.Helper()
	for _, value := range report["adapters"].([]any) {
		adapterReport := value.(map[string]any)
		if adapterReport["id"] == id {
			return adapterReport
		}
	}
	t.Fatalf("report has no adapter %q: %#v", id, report["adapters"])
	return nil
}

func v12CategoryReport(t *testing.T, adapterReport map[string]any, name string) map[string]any {
	t.Helper()
	for _, value := range adapterReport["categories"].([]any) {
		category := value.(map[string]any)
		if category["name"] == name {
			return category
		}
	}
	t.Fatalf("adapter %q has no category %q: %#v", adapterReport["id"], name, adapterReport["categories"])
	return nil
}

func v12HasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func v12OriginTree(t *testing.T, global, root, origin string) string {
	t.Helper()
	return runGit(t, global, root, "--git-dir", origin, "ls-tree", "-r", "--name-only", "HEAD")
}

func v12AssertOriginHas(t *testing.T, global, root, origin, path string) {
	t.Helper()
	if !strings.Contains("\n"+v12OriginTree(t, global, root, origin), "\n"+path+"\n") && !strings.HasSuffix(v12OriginTree(t, global, root, origin), "\n"+path) {
		t.Fatalf("origin tree does not contain %s:\n%s", path, v12OriginTree(t, global, root, origin))
	}
}

func v12AssertOriginLacks(t *testing.T, global, root, origin, path string) {
	t.Helper()
	for _, item := range strings.Fields(v12OriginTree(t, global, root, origin)) {
		if item == path {
			t.Fatalf("origin tree unexpectedly contains %s", path)
		}
	}
}

func TestV12ManifestHomeMainline(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer v1.2 Manifest E2E\n\temail = homer-v12-manifest@example.invalid\n")
	origin := filepath.Join(root, "origin.git")
	runGit(t, global, root, "init", "--bare", "-b", "main", origin)

	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	v12WriteCodeScript(t, filepath.Join(binDir, "code"))
	listScript := filepath.Join(root, "world", "fakecli-list.sh")
	applyScript := filepath.Join(root, "world", "fakecli-apply.sh")
	v12WriteFakeCLIScripts(t, listScript, applyScript)

	a := makeMachine(t, root, "manifest-A", global, false)
	v12WriteVSCodeState(t, a, []string{v12VSCodeOne, v12VSCodeTwo})
	runGit(t, global, root, "clone", origin, a.homerHome)
	initReport := v12MustJSON(t, v12RunHomer(t, binary, a, binDir, "init", "--json"), "init --json")
	initStoreManifest := filepath.Join(a.homerHome, "store", "vscode", "extensions", "extensions.manifest.txt")
	if got, err := os.ReadFile(initStoreManifest); err != nil {
		t.Fatalf("init did not write vscode manifest virtual file: %v", err)
	} else if string(got) != v12VSCodeOne+"\n"+v12VSCodeTwo+"\n" {
		t.Fatalf("init vscode manifest = %q", got)
	}
	vscodeInit := v12AdapterReport(t, initReport, "vscode")
	extensionsInit := v12CategoryReport(t, vscodeInit, "extensions")
	if extensionsInit["fileCount"] != float64(1) {
		t.Fatalf("init vscode extensions report = %#v, want one virtual file", extensionsInit)
	}

	v12AddFakeCLIConfig(t, a, v12CommandPath(listScript), v12CommandPath(applyScript))
	writeFile(t, filepath.Join(a.fakeHome, ".fakecli", "config.toml"), "name = \"machine-a\"\nmode = \"safe\"\n")
	v12WriteFakeCLIState(t, a, []string{v12FakeOne, v12FakeTwo})
	push := v12MustJSON(t, v12RunHomer(t, binary, a, binDir, "push", "--yes", "--json"), "push --yes --json")
	if push["status"] != "pushed" || push["pushedToRemote"] != true {
		t.Fatalf("machine A push = %#v", push)
	}

	b := makeMachine(t, root, "manifest-B", global, false)
	v12WriteVSCodeState(t, b, []string{v12VSCodeOne})
	v12WriteFakeCLIState(t, b, []string{v12FakeOne})
	homeReport := v12MustJSON(t, v12RunHomer(t, binary, b, binDir, "home", origin, "--yes", "--json"), "home origin --yes --json")
	if homeReport["status"] != "homed" || homeReport["ok"] != true {
		t.Fatalf("machine B home = %#v", homeReport)
	}
	manifestReport, ok := homeReport["manifest"].(map[string]any)
	if !ok {
		t.Fatalf("home report has no manifest report: %#v", homeReport)
	}
	installed := stringSlice(manifestReport["installed"])
	for _, want := range []string{"vscode/extensions:" + v12VSCodeTwo, "fakecli/plugins:" + v12FakeTwo} {
		if !v12HasString(installed, want) {
			t.Fatalf("home installed = %#v, missing %q", installed, want)
		}
	}
	if len(installed) != 2 {
		t.Fatalf("home installed = %#v, want exactly the two missing IDs", installed)
	}
	warnings := strings.Join(stringSlice(homeReport["warnings"]), "\n")
	if !strings.Contains(warnings, "已按 --yes 确认执行远端声明的 manifest 命令") {
		t.Fatalf("home report lacks remote manifest gate warning: %#v", homeReport["warnings"])
	}
	fakeApplyLog, err := os.ReadFile(filepath.Join(b.fakeHome, ".fakecli", "plugins.apply.log"))
	if err != nil || !strings.Contains(string(fakeApplyLog), v12FakeTwo) {
		t.Fatalf("fakecli apply log = %q, err=%v", fakeApplyLog, err)
	}
	codeInstallLog, err := os.ReadFile(filepath.Join(b.fakeHome, ".fake-vscode-install.log"))
	if err != nil || !strings.Contains(string(codeInstallLog), v12VSCodeTwo) {
		t.Fatalf("fake code install log = %q, err=%v", codeInstallLog, err)
	}
	bStoreManifest := filepath.Join(b.homerHome, "store", "vscode", "extensions", "extensions.manifest.txt")
	if got, err := os.ReadFile(bStoreManifest); err != nil {
		t.Fatalf("home store missing vscode manifest: %v", err)
	} else if string(got) != v12VSCodeOne+"\n"+v12VSCodeTwo+"\n" {
		t.Fatalf("home store vscode manifest = %q", got)
	}

	c := makeMachine(t, root, "manifest-C", global, false)
	v12WriteVSCodeState(t, c, []string{v12VSCodeOne})
	v12WriteFakeCLIState(t, c, []string{v12FakeOne})
	withoutYes := v12RunHomer(t, binary, c, binDir, "home", origin, "--mode", "merge", "--json")
	if withoutYes.code != 1 {
		t.Fatalf("machine C non-TTY home exit = %d, stdout=%s, stderr=%s", withoutYes.code, withoutYes.stdout, withoutYes.stderr)
	}
	cReport := parseE2EJSON(t, withoutYes, "home origin --mode merge --json")
	if cReport["status"] != "aborted" {
		t.Fatalf("machine C non-TTY home = %#v", cReport)
	}
	if !strings.Contains(strings.Join(stringSlice(cReport["errors"]), "\n"), "manifest") {
		t.Fatalf("machine C error lacks manifest prompt: %#v", cReport["errors"])
	}
}
