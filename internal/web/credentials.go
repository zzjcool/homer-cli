package web

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzjcool/homer-cli/internal/adapter/opencode"
	"github.com/zzjcool/homer-cli/internal/adapter/pi"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/keyring"
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

// credentialRules lists only files a tool is known to keep credentials in.
// herdr and vscode have none: VS Code keeps secrets in the OS keychain and
// herdr stores none, so nothing is invented for them.
func credentialRules() []CredentialRule {
	rules := make([]CredentialRule, 0, len(pi.SecretFiles)+1)
	notes := map[string]string{
		"auth.json":     "登录凭证和 API key",
		"mcp-auth.json": "MCP 服务的登录凭证",
	}
	for _, name := range pi.SecretFiles {
		rules = append(rules, CredentialRule{
			Adapter: pi.PIAdapterID, Name: name,
			Destination: pi.SecretDestination(name), Note: notes[name],
		})
	}
	rules = append(rules, CredentialRule{
		Adapter: opencode.OpencodeAdapterID, Name: "auth.json",
		Destination: "~/.local/share/opencode/auth.json", Note: "provider 登录凭证",
	})
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

// probeCredentials asks the machine which of the paths are regular files.
// A missing entry in the result means the machine could not be asked.
func probeCredentials(ctx context.Context, actor KeyAgentSource, agentID string, paths []string) map[string]bool {
	if actor == nil || len(paths) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := actor.AgentKey(ctx, agentID, keyring.Command{Action: "exists", Paths: paths})
	if err != nil {
		return nil
	}
	var result keyring.Result
	if json.Unmarshal(raw, &result) != nil || !result.OK {
		return nil
	}
	return result.Exists
}

// SecretHit is a file the machine's secret scanner stopped on. Destination
// is where the file lives on that machine, which is what the key form needs.
type SecretHit struct {
	Path        string `json:"path"`
	Destination string `json:"destination"`
	Reason      string `json:"reason,omitempty"`
	Line        int    `json:"line,omitempty"`
}

// preflightSecrets asks a machine to run a collect without confirming. The
// machine scans, then stops before writing anything (confirm=false is the
// same gate "确认" opens), so the files that would be refused are known
// before the user commits to 收取.
func (s *Server) preflightSecrets(ctx context.Context, agentID string, ids []string) map[string][]SecretHit {
	if s.opts.Agents == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// No adapter list means "everything this machine has": the preflight now
	// starts before the status reply, so it cannot know the list yet.
	scope := SyncScope{}
	if len(ids) > 0 {
		scope = SyncScope{Explicit: true, Adapters: ids}
	}
	raw, err := s.opts.Agents.AgentPush(ctx, agentID, false, scope)
	if err != nil {
		return nil
	}
	var report struct {
		Status  string `json:"status"`
		Secrets []struct {
			Path        string `json:"path"`
			Description string `json:"description"`
			Line        int    `json:"line"`
		} `json:"secrets"`
	}
	if json.Unmarshal(raw, &report) != nil || report.Status != "secrets-rejected" {
		return nil
	}
	config, _ := core.LoadConfig(s.paths())
	hits := map[string][]SecretHit{}
	seen := map[string]bool{}
	for _, found := range report.Secrets {
		if seen[found.Path] {
			continue
		}
		seen[found.Path] = true
		adapterID, destination := secretDestination(config, found.Path)
		hits[adapterID] = append(hits[adapterID], SecretHit{
			Path: found.Path, Destination: destination, Reason: found.Description, Line: found.Line,
		})
	}
	return hits
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
