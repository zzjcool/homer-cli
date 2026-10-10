package web

import (
	"path/filepath"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/keyring"
	"github.com/zzjcool/homer-cli/internal/pluginregistry"
)

// Credential presence on a machine, as the collect dialog needs it.
const (
	credPresent = "present"
	credAbsent  = "absent"
	credUnknown = "unknown"
)

// CredentialRule is a credential file one adapter's tool keeps outside the
// synced tree. The adapter never collects it in plaintext (it sits on the
// ignore list), so without an encrypted keyring entry it silently stays on
// the machine. The console uses these rules to say so at collect time.
type CredentialRule struct {
	Adapter     string `json:"adapter"`
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Note        string `json:"note,omitempty"`
	// Exists is filled per machine when choices are built.
	Exists string `json:"exists,omitempty"`
}

// credentialRules lists credential files declared by the official adapter
// plugins. Plugins without known credential files do not invent any.
func credentialRules() []CredentialRule {
	return credentialRulesFromPlugins(pluginregistry.Builtins())
}

func credentialRulesFromPlugins(plugins []pluginregistry.Plugin) []CredentialRule {
	rules := make([]CredentialRule, 0)
	for _, plugin := range plugins {
		if plugin.Role != pluginregistry.RoleAdapter {
			continue
		}
		for _, file := range pluginregistry.PluginCredentialFiles(plugin) {
			rules = append(rules, CredentialRule{
				Adapter: plugin.ID, Name: file.Name,
				Destination: file.Destination, Note: file.Note,
			})
		}
	}
	return rules
}

func credentialRulesFor(adapterID string) []CredentialRule {
	out := []CredentialRule{}
	for _, rule := range credentialRules() {
		if rule.Adapter == adapterID {
			out = append(out, rule)
		}
	}
	return out
}

// sameDestination compares two key destinations after ~ expansion.
func sameDestination(a, b string) bool {
	return filepath.Clean(core.ExpandHome(a)) == filepath.Clean(core.ExpandHome(b))
}

// applyCredentialProbe adds each adapter's credential files to a collect
// choice list. found is the machine's answer (nil when it could not be
// asked, which leaves every file "unknown" rather than guessing).
func applyCredentialProbe(choices []AdapterChoice, found map[string]bool) {
	for i := range choices {
		choices[i].Credentials = credentialRulesFor(choices[i].ID)
		for j := range choices[i].Credentials {
			rule := &choices[i].Credentials[j]
			rule.Exists = credUnknown
			if present, ok := found[rule.Destination]; ok {
				if present {
					rule.Exists = credPresent
				} else {
					rule.Exists = credAbsent
				}
			}
		}
	}
}

// SecretHit is a file the machine's secret scanner stopped on. Destination
// is where the file lives on that machine, which is what the key form needs.
type SecretHit struct {
	Path        string `json:"path"`
	Destination string `json:"destination"`
	Reason      string `json:"reason,omitempty"`
	Line        int    `json:"line,omitempty"`
}

// secretDestination turns a store path ("pi/files/web-search.json") into the
// adapter id and the tool path on the machine ("~/.pi/agent/web-search.json").
func secretDestination(config *core.HomerConfig, storePath string) (string, string) {
	parts := strings.SplitN(strings.TrimLeft(filepath.ToSlash(storePath), "/"), "/", 3)
	if len(parts) != 3 {
		return "", ""
	}
	adapterID, category, rel := parts[0], parts[1], parts[2]
	if config == nil {
		return adapterID, ""
	}
	adapter, ok := config.Adapters[adapterID]
	if !ok {
		return adapterID, ""
	}
	categoryConfig, ok := adapter.Categories[category]
	if !ok {
		return adapterID, ""
	}
	return adapterID, categoryDestination(adapter.Root, categoryConfig, rel)
}

// credentialRulesView is the storage drawer's "默认加密" list: every known
// credential file, and whether a keyring entry already covers it.
func credentialRulesView(homerHome string) []map[string]any {
	listed := keyring.Apply(homerHome, keyring.Command{Action: "list"})
	rules := credentialRules()
	out := make([]map[string]any, 0, len(rules))
	for _, rule := range rules {
		encrypted := false
		for _, key := range listed.Keys {
			for _, file := range key.Files {
				if sameDestination(file.Destination, rule.Destination) {
					encrypted = true
				}
			}
		}
		out = append(out, map[string]any{
			"adapter":     rule.Adapter,
			"name":        rule.Name,
			"destination": rule.Destination,
			"encrypted":   encrypted,
		})
	}
	return out
}
