package pluginregistry

import (
	"reflect"
	"runtime"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/adapter/herdr"
	"github.com/zzjcool/homer-cli/internal/adapter/keys"
	"github.com/zzjcool/homer-cli/internal/adapter/opencode"
	"github.com/zzjcool/homer-cli/internal/adapter/pi"
	"github.com/zzjcool/homer-cli/internal/adapter/vscode"
	"github.com/zzjcool/homer-cli/internal/core"
)

func TestBuiltinsOrderIDsAndRoles(t *testing.T) {
	plugins := Builtins()
	wantIDs := []string{"pi", "herdr", "opencode", "vscode", "keyring", "ssh-key"}
	if len(plugins) != len(wantIDs) {
		t.Fatalf("Builtins() has %d entries, want %d", len(plugins), len(wantIDs))
	}
	seen := make(map[string]bool, len(plugins))
	roles := map[Role]int{}
	for index, plugin := range plugins {
		if plugin.ID != wantIDs[index] {
			t.Errorf("Builtins()[%d].ID = %q, want %q", index, plugin.ID, wantIDs[index])
		}
		if seen[plugin.ID] {
			t.Errorf("duplicate plugin id %q", plugin.ID)
		}
		seen[plugin.ID] = true
		roles[plugin.Role]++
	}
	if roles[RoleAdapter] != 4 || roles[RoleCarrier] != 1 || roles[RoleAction] != 1 {
		t.Fatalf("role counts = %#v, want adapter=4 carrier=1 action=1", roles)
	}
}

func TestBuiltinsCarryFrozenDefaultsAndActionSpec(t *testing.T) {
	plugins := Builtins()
	wantAdapters := []struct {
		id     string
		config any
	}{
		{"pi", &pi.DefaultPIAdapter},
		{"herdr", &herdr.DefaultHerdrAdapter},
		{"opencode", &opencode.DefaultOpencodeAdapter},
		{"vscode", &vscode.DefaultVSCodeAdapter},
		{"keyring", &keys.DefaultAdapter},
	}
	for index, want := range wantAdapters {
		plugin := plugins[index]
		if plugin.ID != want.id || plugin.Adapter == nil || !reflect.DeepEqual(plugin.Adapter, want.config) {
			t.Errorf("Builtins()[%d] adapter = (%q, %#v), want (%q, %#v)", index, plugin.ID, plugin.Adapter, want.id, want.config)
		}
	}
	keyring, ok := Builtin("keyring")
	if !ok || keyring.Role != RoleCarrier || keyring.Adapter == nil {
		t.Fatalf("Builtin(\"keyring\") = (%#v, %v)", keyring, ok)
	}
	sshKey, ok := Builtin("ssh-key")
	if !ok || sshKey.Role != RoleAction || sshKey.Action == nil {
		t.Fatalf("Builtin(\"ssh-key\") = (%#v, %v)", sshKey, ok)
	}
	if sshKey.Action.Method != "ssh-key" || len(sshKey.Action.Form) != 1 {
		t.Fatalf("ssh-key action = %#v", sshKey.Action)
	}
	field := sshKey.Action.Form[0]
	if field.Field != "githubUser" || field.Label != "GitHub 用户名" || !field.Required || field.Placeholder != "例如 octocat" {
		t.Fatalf("ssh-key form field = %#v", field)
	}
}

func TestBuiltinsAndBuiltinReturnDetachedValues(t *testing.T) {
	plugins := Builtins()
	plugins[0].Name = "changed"
	plugins[0].Adapter.Categories["settings"] = core.CategoryConfig{}
	plugins[5].Action.Form[0].Field = "changed"

	again, ok := Builtin("pi")
	if !ok || again.Name != "pi" || again.Adapter.Categories["settings"].Paths[0] != "settings.json" {
		t.Fatalf("mutating Builtins() changed the registry: %#v", again)
	}
	sshKey, ok := Builtin("ssh-key")
	if !ok || sshKey.Action.Form[0].Field != "githubUser" {
		t.Fatalf("mutating Builtins() changed the action registry: %#v", sshKey)
	}
}

func TestToolsMatchTheFourBuiltInToolDefinitions(t *testing.T) {
	piInstall := "curl -fsSL https://pi.dev/install.sh | sh"
	if runtime.GOOS == "windows" {
		piInstall = `powershell -c "irm https://pi.dev/install.ps1 | iex"`
	}
	want := []adapter.Tool{
		{
			ID: "pi", Adapter: "pi", Label: "pi", Binary: "pi",
			VersionArgs: []string{"--version"}, Install: piInstall,
			UpgradeArgs: []string{"update", "--self"},
		},
		{
			ID: "herdr", Adapter: "herdr", Label: "herdr", Binary: "herdr",
			VersionArgs: []string{"--version"},
			Install:     "curl -fsSL https://herdr.dev/install.sh | sh",
			UpgradeArgs: []string{"update"},
		},
		{
			ID: "opencode", Adapter: "opencode", Label: "opencode", Binary: "opencode",
			VersionArgs: []string{"--version"},
			Install:     "curl -fsSL https://opencode.ai/install | bash",
			UpgradeArgs: []string{"upgrade"},
		},
		{
			ID: "vscode", Adapter: "vscode", Label: "VS Code", Binary: "code",
			VersionArgs: []string{"--version"},
		},
	}
	if got := Tools(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Tools() = %#v, want %#v", got, want)
	}

	tools := Tools()
	tools[0].VersionArgs[0] = "changed"
	if got := Tools()[0].VersionArgs[0]; got != "--version" {
		t.Fatalf("mutating Tools() changed the registry: VersionArgs[0] = %q", got)
	}
}

func TestToolLookupAndOfficialInstall(t *testing.T) {
	tool, ok := ToolByID("pi")
	if !ok || tool.ID != "pi" {
		t.Fatalf("ToolByID(\"pi\") = (%#v, %v)", tool, ok)
	}
	if _, ok := ToolByID("unknown"); ok {
		t.Fatal("ToolByID accepted an unknown id")
	}
	install, ok := OfficialInstall("pi")
	if !ok || install == "" {
		t.Fatalf("OfficialInstall(\"pi\") = (%q, %v)", install, ok)
	}
	if install, ok := OfficialInstall("code"); ok || install != "" {
		t.Fatalf("OfficialInstall(\"code\") = (%q, %v), want no installer", install, ok)
	}
}

func TestPluginCredentialFiles(t *testing.T) {
	piPlugin, ok := Builtin("pi")
	if !ok {
		t.Fatal("Builtin(\"pi\") not found")
	}
	wantPi := []CredentialFile{
		{Name: "auth.json", Destination: "~/.pi/agent/auth.json", Note: "登录凭证和 API key"},
		{Name: "mcp-auth.json", Destination: "~/.pi/agent/mcp-auth.json", Note: "MCP 服务的登录凭证"},
	}
	if got := PluginCredentialFiles(piPlugin); !reflect.DeepEqual(got, wantPi) {
		t.Fatalf("PluginCredentialFiles(pi) = %#v, want %#v", got, wantPi)
	}

	opencodePlugin, ok := Builtin("opencode")
	if !ok {
		t.Fatal("Builtin(\"opencode\") not found")
	}
	wantOpencode := []CredentialFile{{
		Name: "auth.json", Destination: "~/.local/share/opencode/auth.json", Note: "provider 登录凭证",
	}}
	if got := PluginCredentialFiles(opencodePlugin); !reflect.DeepEqual(got, wantOpencode) {
		t.Fatalf("PluginCredentialFiles(opencode) = %#v, want %#v", got, wantOpencode)
	}
	if got := PluginCredentialFiles(pluginsByID(t, "herdr")); len(got) != 0 {
		t.Fatalf("PluginCredentialFiles(herdr) = %#v, want no files", got)
	}
}

func pluginsByID(t *testing.T, id string) Plugin {
	t.Helper()
	plugin, ok := Builtin(id)
	if !ok {
		t.Fatalf("Builtin(%q) not found", id)
	}
	return plugin
}
