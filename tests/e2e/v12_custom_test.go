package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
)

func v12HandwrittenFakeCLIConfig(listCmd, applyCmd string) string {
	return fmt.Sprintf(`{
  "version": 1,
  "adapters": {
    "fakecli": {
      "root": "~/.fakecli",
      "categories": {
        "conf": {
          "paths": ["config.toml"],
          "mode": "mirror"
        },
        "plugins": {
          "kind": "manifest",
          "mode": "mirror",
          "listCmd": %s,
          "applyCmd": %s
        },
        "disabled": {
          "paths": ["disabled.txt"],
          "mode": "mirror",
          "enabled": false
        }
      }
    }
  }
}
`, strconv.Quote(listCmd), strconv.Quote(applyCmd))
}

func v12FindDisabled(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestV12CustomAdapterEndToEnd(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer v1.2 Custom E2E\n\temail = homer-v12-custom@example.invalid\n")
	origin := filepath.Join(root, "origin.git")
	runGit(t, global, root, "init", "--bare", "-b", "main", origin)

	listScript := filepath.Join(root, "world", "fakecli-list.sh")
	applyScript := filepath.Join(root, "world", "fakecli-apply.sh")
	v12WriteFakeCLIScripts(t, listScript, applyScript)

	a := makeMachine(t, root, "custom-A", global, false)
	writeFile(t, filepath.Join(a.fakeHome, ".fakecli", "config.toml"), "# exact custom config\nvalue = \"A\"\nbytes = [1, 2, 3]\n")
	writeFile(t, filepath.Join(a.fakeHome, ".fakecli", "disabled.txt"), "must never enter store\n")
	v12WriteFakeCLIState(t, a, []string{v12FakeOne, v12FakeTwo})
	if err := os.MkdirAll(a.homerHome, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(a.homerHome, "homer.json"), v12HandwrittenFakeCLIConfig(v12CommandPath(listScript), v12CommandPath(applyScript)))
	runGit(t, global, a.homerHome, "init", "-b", "main")
	runGit(t, global, a.homerHome, "remote", "add", "origin", origin)

	status := v12MustJSON(t, v12RunHomer(t, binary, a, filepath.Dir(applyScript), "status", "--json"), "status --json")
	fakeStatus := v12AdapterReport(t, status, "fakecli")
	confStatus := v12CategoryReport(t, fakeStatus, "conf")
	pluginsStatus := v12CategoryReport(t, fakeStatus, "plugins")
	if confStatus["push"] != float64(1) || confStatus["pull"] != float64(0) || confStatus["conflicts"] != float64(0) {
		t.Fatalf("custom conf status = %#v, want one push", confStatus)
	}
	if pluginsStatus["push"] != float64(1) || pluginsStatus["pull"] != float64(0) || pluginsStatus["conflicts"] != float64(0) {
		t.Fatalf("custom plugins status = %#v, want one virtual-file push", pluginsStatus)
	}
	if !v12FindDisabled(stringSlice(status["disabled"]), "fakecli/disabled") {
		t.Fatalf("status disabled = %#v, want fakecli/disabled", status["disabled"])
	}

	push := v12MustJSON(t, v12RunHomer(t, binary, a, filepath.Dir(applyScript), "push", "--yes", "--json"), "push --yes --json")
	if push["status"] != "pushed" || push["pushedToRemote"] != true {
		t.Fatalf("custom push = %#v", push)
	}
	v12AssertOriginHas(t, global, root, origin, "store/fakecli/conf/config.toml")
	v12AssertOriginHas(t, global, root, origin, "store/fakecli/plugins/plugins.manifest.txt")
	v12AssertOriginLacks(t, global, root, origin, "store/fakecli/disabled/disabled.txt")

	b := makeMachine(t, root, "custom-B", global, false)
	v12WriteFakeCLIState(t, b, []string{v12FakeOne})
	homeReport := v12MustJSON(t, v12RunHomer(t, binary, b, filepath.Dir(applyScript), "home", origin, "--mode", "pull", "--yes", "--json"), "home origin --mode pull --yes --json")
	if homeReport["status"] != "homed" || homeReport["ok"] != true {
		t.Fatalf("custom home = %#v", homeReport)
	}
	manifestReport := homeReport["manifest"].(map[string]any)
	installed := stringSlice(manifestReport["installed"])
	if len(installed) != 1 || !v12HasString(installed, "fakecli/plugins:"+v12FakeTwo) {
		t.Fatalf("custom home manifest = %#v", manifestReport)
	}
	wantConfig := "# exact custom config\nvalue = \"A\"\nbytes = [1, 2, 3]\n"
	gotConfig, err := os.ReadFile(filepath.Join(b.fakeHome, ".fakecli", "config.toml"))
	if err != nil || string(gotConfig) != wantConfig {
		t.Fatalf("custom config after home = %q, err=%v", gotConfig, err)
	}
	applyLog, err := os.ReadFile(filepath.Join(b.fakeHome, ".fakecli", "plugins.apply.log"))
	if err != nil || !strings.Contains(string(applyLog), v12FakeTwo) {
		t.Fatalf("custom apply log = %q, err=%v", applyLog, err)
	}
	if _, err := os.Stat(filepath.Join(b.fakeHome, ".fakecli", "disabled.txt")); !os.IsNotExist(err) {
		t.Fatalf("disabled category appeared on new machine: err=%v", err)
	}
	if got, err := os.ReadFile(filepath.Join(b.homerHome, "store", "fakecli", "plugins", "plugins.manifest.txt")); err != nil || string(got) != v12FakeOne+"\n"+v12FakeTwo+"\n" {
		t.Fatalf("custom manifest store = %q, err=%v", got, err)
	}
}

type v12WizardPort struct {
	calls int
}

func (port *v12WizardPort) MultiSelect(_ string, _ []commands.WizardOption, _ []string) ([]string, error) {
	defer func() { port.calls++ }()
	switch port.calls {
	case 0:
		return []string{"pi"}, nil
	case 1:
		return []string{"settings"}, nil
	default:
		return nil, fmt.Errorf("unexpected wizard prompt %d", port.calls)
	}
}

func TestV12InitWizardSelectionWritesOnlySelectedSnapshot(t *testing.T) {
	home := t.TempDir()
	wizard := &v12WizardPort{}
	_, err := commands.RunInitWithDeps(commands.InitOptions{HomerHome: home}, commands.InitDeps{
		Wizard: wizard,
		Scan: func(adapterID string, config core.AdapterConfig) adapter.ScanOutcome {
			categoryNames := make([]string, 0, len(config.Categories))
			for name := range config.Categories {
				categoryNames = append(categoryNames, name)
			}
			sort.Strings(categoryNames)
			categories := make([]core.CategorySnapshot, 0, len(categoryNames))
			for _, name := range categoryNames {
				key := name + ".json"
				if name == "settings" {
					key = "settings.json"
				}
				categories = append(categories, core.CategorySnapshot{
					AdapterID: adapterID,
					Category:  name,
					Mode:      config.Categories[name].Mode,
					Files:     core.SnapshotFiles{key: {Kind: "file", Content: adapterID + "/" + name}},
				})
			}
			return adapter.ScanOutcome{Snapshot: core.AdapterSnapshot{AdapterID: adapterID, Categories: categories}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wizard.calls != 2 {
		t.Fatalf("wizard calls = %d, want adapter + selected pi category", wizard.calls)
	}
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return ""
	})
	config, err := core.LoadConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	piConfig := config.Adapters["pi"]
	if piConfig.Enabled != nil && !*piConfig.Enabled {
		t.Fatal("selected pi adapter was disabled")
	}
	settings := piConfig.Categories["settings"]
	if settings.Enabled != nil && !*settings.Enabled {
		t.Fatal("selected settings category was disabled")
	}
	for _, id := range []string{"herdr", "opencode", "vscode"} {
		if enabled := config.Adapters[id].Enabled; enabled == nil || *enabled {
			t.Fatalf("unselected adapter %s enabled = %v", id, enabled)
		}
	}
	snapshots, err := core.ReadSnapshotFromStore(paths, *config)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].AdapterID != "pi" || len(snapshots[0].Categories) != 1 || snapshots[0].Categories[0].Category != "settings" {
		t.Fatalf("wizard store snapshots = %#v, want only pi/settings", snapshots)
	}
	if len(snapshots[0].Categories[0].Files) != 1 || snapshots[0].Categories[0].Files["settings.json"].Content != "pi/settings" {
		t.Fatalf("wizard selected store files = %#v", snapshots[0].Categories[0].Files)
	}
}
