package web

import (
	"context"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/keyring"
)

type InspectParams struct {
	Adapters    []string `json:"adapters,omitempty"`
	Credentials []string `json:"credentials,omitempty"`
	WantKeys    bool     `json:"wantKeys,omitempty"`
}

type InspectEvent struct {
	Stage   string                        `json:"stage"`
	Done    int                           `json:"done,omitempty"`
	Total   int                           `json:"total,omitempty"`
	Adapter *commands.StatusAdapterReport `json:"adapter,omitempty"`
	Present map[string]bool               `json:"present,omitempty"`
	Secrets []InspectSecret               `json:"secrets,omitempty"`
	Keys    *keyring.Result               `json:"keys,omitempty"`
}

type InspectSecret struct {
	Path        string `json:"path"`
	Description string `json:"description"`
	Line        int    `json:"line"`
}

type InspectResult struct {
	Status  commands.StatusReport `json:"status"`
	Present map[string]bool       `json:"present,omitempty"`
	Secrets []InspectSecret       `json:"secrets,omitempty"`
	Keys    *keyring.Result       `json:"keys,omitempty"`
	Errors  []string              `json:"errors,omitempty"`
}

type InspectSource interface {
	AgentInspect(ctx context.Context, agentID string, p InspectParams, onEvent func(InspectEvent)) (InspectResult, error)
}
