package hub

import (
	"encoding/json"
	"time"
)

type TaskKind string

const (
	TaskKindStatus  TaskKind = "status"
	TaskKindDiff    TaskKind = "diff"
	TaskKindPush    TaskKind = "push"
	TaskKindPull    TaskKind = "pull"
	TaskKindSSHKey  TaskKind = "ssh-key"
	TaskKindSecret  TaskKind = "secret"
	TaskKindUpgrade TaskKind = "upgrade"
	// TaskKindToolUpgrade upgrades one program an adapter drives (pi, herdr,
	// opencode...). Options.Tool names it; the agent maps the name to a
	// command it already knows.
	TaskKindToolUpgrade TaskKind = "tool-upgrade"
)

type TaskOptions struct {
	Adapter      string   `json:"adapter,omitempty"`
	Category     string   `json:"category,omitempty"`
	Path         string   `json:"path,omitempty"`
	Confirm      bool     `json:"confirm,omitempty"`
	Adapters     []string `json:"adapters,omitempty"`
	Overwrite    bool     `json:"overwrite,omitempty"`
	AllowSecrets bool     `json:"allowSecrets,omitempty"`
	// Resolve is set by the console's conflict buttons: "local" keeps this
	// machine, "center" applies the hub generation. Empty for ordinary
	// push/pull.
	Resolve string `json:"resolve,omitempty"`
	// GitHubUser and SSHKeys carry a login-key install. The hub fills
	// SSHKeys after reading the user's public keys; the agent only writes
	// them into authorized_keys.
	GitHubUser string   `json:"githubUser,omitempty"`
	SSHKeys    []string `json:"sshKeys,omitempty"`
	// SecretAction is status, keygen, save, push, or pull. SecretPayload is
	// the save body (recipients + files). The private key is never carried.
	SecretAction  string          `json:"secretAction,omitempty"`
	SecretPayload json.RawMessage `json:"secretPayload,omitempty"`
	// Tool is the adapter.Tool ID a tool-upgrade task acts on. It is a name,
	// never a command: an unknown name is refused by the agent.
	Tool string `json:"tool,omitempty"`
}

const (
	AgentStaleAfter = 90 * time.Second

	// Pull, self-upgrade and tool upgrade can legitimately take minutes.
	PullWait        = 12 * time.Minute
	UpgradeWait     = 3 * time.Minute
	ToolUpgradeWait = 7 * time.Minute
)
