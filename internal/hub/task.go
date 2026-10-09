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
	// TaskKindResolveRecord stages or unstages a conflict decision on the
	// machine (staged-resolution plan): record a choice, clear it, or list
	// pending decisions. It never touches any file; a later dispatch
	// consumes the recorded entries.
	TaskKindResolveRecord TaskKind = "resolve-record"
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
	// Resolution fields (staged-resolution plan). ResolutionAction /
	// ResolutionChoice drive TaskKindResolveRecord; the existing Resolve
	// field is NOT reused because runTaskCommand dispatches Resolve=local|center
	// regardless of method.
	ResolutionAction string `json:"resolutionAction,omitempty"` // record|clear|list
	ResolutionChoice string `json:"resolutionChoice,omitempty"` // center|local
	// CenterGeneration is the hub's generation when the task was built;
	// 0 means unknown (never written today).
	CenterGeneration int `json:"centerGeneration,omitempty"`
	// ApplyResolutions lets a pull consume the recorded decisions for its
	// explicitly selected adapters (D3).
	ApplyResolutions bool `json:"applyResolutions,omitempty"`
	// ClearResolutions asks a successful push/pull to clear the recorded
	// decisions for its adapters (the record-and-execute-now fallback).
	ClearResolutions bool `json:"clearResolutions,omitempty"`
}

const (
	AgentStaleAfter = 90 * time.Second

	// Pull, self-upgrade and tool upgrade can legitimately take minutes.
	PullWait        = 12 * time.Minute
	UpgradeWait     = 3 * time.Minute
	ToolUpgradeWait = 7 * time.Minute
)
