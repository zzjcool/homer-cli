// Package pluginregistry is the single source of truth for built-in plugins
// and the CLI tools that Homer tracks.
package pluginregistry

import (
	"runtime"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/adapter/herdr"
	"github.com/zzjcool/homer-cli/internal/adapter/keys"
	"github.com/zzjcool/homer-cli/internal/adapter/opencode"
	"github.com/zzjcool/homer-cli/internal/adapter/pi"
	"github.com/zzjcool/homer-cli/internal/adapter/vscode"
	"github.com/zzjcool/homer-cli/internal/core"
)

// Role identifies the kind of capability a plugin provides.
type Role string

const (
	RoleAdapter Role = "adapter"
	RoleCarrier Role = "carrier"
	RoleAction  Role = "action"
)

// Plugin is one installed capability in Homer's plugin directory.
type Plugin struct {
	ID      string              `json:"id"`
	Role    Role                `json:"role"`
	Name    string              `json:"name"`
	Desc    string              `json:"description,omitempty"`
	Adapter *core.AdapterConfig `json:"adapter,omitempty"`
	Action  *ActionSpec         `json:"action,omitempty"`
}

// ActionSpec describes the form used to render a one-shot plugin action. It
// does not select or execute a machine method.
type ActionSpec struct {
	Method string      `json:"method"`
	Form   []FormField `json:"form"`
}

// FormField is one value rendered for an action plugin.
type FormField struct {
	Field       string `json:"field"`
	Label       string `json:"label"`
	Required    bool   `json:"required"`
	Placeholder string `json:"placeholder,omitempty"`
}

// CredentialFile describes a credential file associated with an adapter.
type CredentialFile struct {
	Name        string
	Destination string
	Note        string
}

// Builtins returns the official plugins in their stable display order. Every
// call returns independent adapter configuration and action form values.
func Builtins() []Plugin {
	return []Plugin{
		{
			ID: pi.PIAdapterID, Role: RoleAdapter, Name: "pi",
			Desc:    "pi 的配置与扩展同步",
			Adapter: adapterConfigCopy(pi.DefaultPIAdapter),
		},
		{
			ID: herdr.HerdrAdapterID, Role: RoleAdapter, Name: "herdr",
			Desc:    "herdr 终端复用器配置",
			Adapter: adapterConfigCopy(herdr.DefaultHerdrAdapter),
		},
		{
			ID: opencode.OpencodeAdapterID, Role: RoleAdapter, Name: "opencode",
			Desc:    "opencode 配置",
			Adapter: adapterConfigCopy(opencode.DefaultOpencodeAdapter),
		},
		{
			ID: vscode.VSCodeAdapterID, Role: RoleAdapter, Name: "VS Code",
			Desc:    "VS Code 设置与扩展",
			Adapter: adapterConfigCopy(vscode.DefaultVSCodeAdapter),
		},
		{
			ID: keys.AdapterID, Role: RoleCarrier, Name: "密钥环",
			Desc:    "跟随同步的加密密钥载体",
			Adapter: adapterConfigCopy(keys.DefaultAdapter),
		},
		{
			ID: "ssh-key", Role: RoleAction, Name: "登录公钥",
			Desc: "把 GitHub 用户公钥写入机器 authorized_keys",
			Action: &ActionSpec{
				Method: "ssh-key",
				Form: []FormField{{
					Field: "githubUser", Label: "GitHub 用户名", Required: true,
					Placeholder: "例如 octocat",
				}},
			},
		},
	}
}

// Builtin finds an official plugin by id. The returned plugin is detached from
// the registry and may be modified by the caller.
func Builtin(id string) (Plugin, bool) {
	for _, plugin := range Builtins() {
		if plugin.ID == id {
			return plugin, true
		}
	}
	return Plugin{}, false
}

// Tools returns the programs tracked by Homer, in the order the console shows
// them. Each call creates an independent list, including its argument slices.
func Tools() []adapter.Tool {
	return []adapter.Tool{
		{
			ID: "pi", Adapter: "pi", Label: "pi", Binary: "pi",
			VersionArgs: []string{"--version"},
			Install:     piInstallCommand(),
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
}

// ToolByID finds a registered program.
func ToolByID(id string) (adapter.Tool, bool) {
	for _, tool := range Tools() {
		if tool.ID == id {
			return tool, true
		}
	}
	return adapter.Tool{}, false
}

// OfficialInstall returns the official copy-paste installer for a CLI that a
// built-in adapter invokes from listCmd or applyCmd. Programs without a
// one-line installer have no entry.
func OfficialInstall(binary string) (string, bool) {
	for _, tool := range Tools() {
		if tool.Binary == binary && tool.Install != "" {
			return tool.Install, true
		}
	}
	return "", false
}

// PluginCredentialFiles returns known credential files for pi and opencode.
// The returned slice is newly allocated on every call.
func PluginCredentialFiles(plugin Plugin) []CredentialFile {
	switch plugin.ID {
	case pi.PIAdapterID:
		notes := map[string]string{
			"auth.json":     "登录凭证和 API key",
			"mcp-auth.json": "MCP 服务的登录凭证",
		}
		files := make([]CredentialFile, 0, len(pi.SecretFiles))
		for _, name := range pi.SecretFiles {
			files = append(files, CredentialFile{
				Name: name, Destination: pi.SecretDestination(name), Note: notes[name],
			})
		}
		return files
	case opencode.OpencodeAdapterID:
		return []CredentialFile{{
			Name: "auth.json", Destination: "~/.local/share/opencode/auth.json", Note: "provider 登录凭证",
		}}
	default:
		return nil
	}
}

func piInstallCommand() string {
	if runtime.GOOS == "windows" {
		return `powershell -c "irm https://pi.dev/install.ps1 | iex"`
	}
	return "curl -fsSL https://pi.dev/install.sh | sh"
}

func adapterConfigCopy(config core.AdapterConfig) *core.AdapterConfig {
	copy := config
	copy.Enabled = boolCopy(config.Enabled)
	copy.Ignore = stringsCopy(config.Ignore)
	copy.AllowEscape = stringsCopy(config.AllowEscape)
	if config.Categories != nil {
		copy.Categories = make(map[string]core.CategoryConfig, len(config.Categories))
		for name, category := range config.Categories {
			category.Paths = stringsCopy(category.Paths)
			category.Kind = categoryKindCopy(category.Kind)
			category.Enabled = boolCopy(category.Enabled)
			category.Exclude = stringsCopy(category.Exclude)
			category.ExcludeKeys = stringsCopy(category.ExcludeKeys)
			copy.Categories[name] = category
		}
	}
	return &copy
}

func boolCopy(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func categoryKindCopy(value *core.CategoryKind) *core.CategoryKind {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func stringsCopy(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}
