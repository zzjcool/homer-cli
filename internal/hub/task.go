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
	TaskKindStatus TaskKind = "status"
	TaskKindDiff   TaskKind = "diff"
	TaskKindPush   TaskKind = "push"
	TaskKindPull   TaskKind = "pull"
	TaskKindSSHKey TaskKind = "ssh-key"
	TaskKindSecret TaskKind = "secret"
)

type Task struct {
	TaskID    string      `json:"taskId"`
	Kind      TaskKind    `json:"kind"`
	Options   TaskOptions `json:"options"`
	CreatedAt time.Time   `json:"createdAt"`
}

type TaskOptions struct {
	Adapter   string   `json:"adapter,omitempty"`
	Category  string   `json:"category,omitempty"`
	Path      string   `json:"path,omitempty"`
	Confirm   bool     `json:"confirm,omitempty"`
	Adapters  []string `json:"adapters,omitempty"`
	Overwrite bool     `json:"overwrite,omitempty"`
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
	TaskTTL           = 120 * time.Second
	AgentStaleAfter   = 90 * time.Second
)
