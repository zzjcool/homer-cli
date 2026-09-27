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
)

type Task struct {
	TaskID    string      `json:"taskId"`
	Kind      TaskKind    `json:"kind"`
	Options   TaskOptions `json:"options"`
	CreatedAt time.Time   `json:"createdAt"`
}

type TaskOptions struct {
	Adapter  string `json:"adapter,omitempty"`
	Category string `json:"category,omitempty"`
	Confirm  bool   `json:"confirm,omitempty"`
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
