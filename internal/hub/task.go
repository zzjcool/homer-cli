package hub

import (
	"encoding/json"
	"time"
)

type AgentMode string

const (
	AgentModeListen  AgentMode = "listen"
	AgentModeConnect AgentMode = "connect"
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

type Task struct {
	TaskID    string      `json:"taskId"`
	Kind      TaskKind    `json:"kind"`
	Options   TaskOptions `json:"options"`
	CreatedAt time.Time   `json:"createdAt"`
}

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

type TaskResult struct {
	TaskID  string          `json:"taskId"`
	AgentID string          `json:"agentId"`
	OK      bool            `json:"ok"`
	Kind    TaskKind        `json:"kind"`
	Report  json.RawMessage `json:"report,omitempty"`
	Error   string          `json:"error,omitempty"`
}

const (
	TaskQueueCapacity = 8
	// TaskTTL is how long a task may wait for its machine to take it, and how
	// long an ordinary task may take in all, from being queued to its result
	// coming back. See TaskLifetime for the kinds that run for minutes.
	TaskTTL         = 120 * time.Second
	AgentStaleAfter = 90 * time.Second
)

// How long the dispatcher waits for the kinds of task that legitimately run
// for minutes: a pull installs plugins one by one, an upgrade downloads a
// binary or runs a vendor installer.
const (
	PullWait        = 12 * time.Minute
	UpgradeWait     = 3 * time.Minute
	ToolUpgradeWait = 7 * time.Minute
)

// TaskLifetime is how long a machine may spend on a task of this kind, from
// the moment it takes the task until its result is submitted. A task that
// outlives it is dropped.
//
// The dispatcher has always been willing to wait minutes for a pull or an
// upgrade, so the registry must not cut those off at the ordinary limit and
// then discard the result the machine finally reports. A task that no machine
// has taken is a different matter: it only lives TaskTTL, whatever its kind,
// so that a machine that has gone away is noticed in two minutes rather than
// twelve.
func TaskLifetime(kind TaskKind) time.Duration {
	switch kind {
	case TaskKindPull:
		return PullWait
	case TaskKindUpgrade:
		return UpgradeWait
	case TaskKindToolUpgrade:
		return ToolUpgradeWait
	default:
		return TaskTTL
	}
}
