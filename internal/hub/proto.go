package hub

import (
	"time"

	"github.com/zzjcool/homer-cli/internal/toolctl"
)

// Stream methods shared by the hub and agents. Method names are the wire
// representation of TaskKind where a task kind already exists.
const (
	MethodHello     = "hello"
	MethodHeartbeat = "hb"
	MethodInspect   = "collect.inspect"
)

// HelloParams is the agent's first stream request.
type HelloParams struct {
	Proto    int               `json:"proto"`
	AgentID  string            `json:"agentId"`
	Hostname string            `json:"hostname"`
	Version  string            `json:"version"`
	Caps     []string          `json:"caps"`
	Drift    *AgentDrift       `json:"drift,omitempty"`
	Host     *HostSnapshot     `json:"host,omitempty"`
	Tools    *[]toolctl.Status `json:"tools,omitempty"`
}

// WelcomeResult is sent after the hub accepts an agent hello request.
type WelcomeResult struct {
	Proto          int    `json:"proto"`
	HubVersion     string `json:"hubVersion"`
	InstanceID     string `json:"instanceId"`
	AgentSecret    string `json:"agentSecret,omitempty"`
	PingIntervalMs int    `json:"pingIntervalMs"`
	PingTimeoutMs  int    `json:"pingTimeoutMs"`
	MaxFrame       int    `json:"maxFrame"`
}

// HeartbeatParams is the agent's best-effort event update.
type HeartbeatParams struct {
	Version string            `json:"version,omitempty"`
	Drift   *AgentDrift       `json:"drift,omitempty"`
	Host    *HostSnapshot     `json:"host,omitempty"`
	Tools   *[]toolctl.Status `json:"tools,omitempty"`
}

const (
	CallBudgetDefault = 60 * time.Second
	ReqDeadlineSlack  = 5 * time.Second
)

func CallBudget(kind TaskKind) time.Duration {
	switch kind {
	case TaskKindPull:
		return PullWait
	case TaskKindUpgrade:
		return UpgradeWait
	case TaskKindToolUpgrade:
		return ToolUpgradeWait
	default:
		return CallBudgetDefault
	}
}
