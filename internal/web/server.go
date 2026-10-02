package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
)

// Version is injected by the command package when it knows the build version.
// Keeping a useful development default makes the package usable from tests and
// from embedders that do not have a version variable of their own.
var Version = "dev"

type AgentIdentity struct {
	AgentID  string `json:"agentId"`
	Hostname string `json:"hostname"`
	Version  string `json:"version,omitempty"`
}

type AgentInfo struct {
	AgentID  string    `json:"agentId"`
	Hostname string    `json:"hostname"`
	Mode     string    `json:"mode"`
	Addr     string    `json:"addr,omitempty"`
	LastSeen time.Time `json:"lastSeen"`
	Version  string    `json:"version,omitempty"`
	// Stale is true when the hub has not heard from the agent for longer
	// than hub.AgentStaleAfter; the UI renders it as an offline status dot.
	Stale bool `json:"stale"`
	// Drift is the machine's last self-reported status summary (uploaded
	// with each poll): counts relative to the storage it last synced with.
	Drift *AgentDrift `json:"drift,omitempty"`
	// Host is the machine's last self-reported resource snapshot.
	Host *HostSnapshot `json:"host,omitempty"`
}

// AgentDrift is the per-machine status summary the console renders.
type AgentDrift struct {
	Push      int    `json:"push"`
	Pull      int    `json:"pull"`
	Conflicts int    `json:"conflicts"`
	Error     string `json:"error,omitempty"`
}

// HostSnapshot is the resource report an agent uploads. Field names match
// hub.HostSnapshot so the dispatcher can pass the JSON straight through.
type HostSnapshot struct {
	OS        string      `json:"os,omitempty"`
	Arch      string      `json:"arch,omitempty"`
	Distro    string      `json:"distro,omitempty"`
	Kernel    string      `json:"kernel,omitempty"`
	UptimeSec int64       `json:"uptimeSec,omitempty"`
	CPU       *HostCPU    `json:"cpu,omitempty"`
	Memory    *HostMemory `json:"memory,omitempty"`
	Swap      *HostMemory `json:"swap,omitempty"`
	Load      *HostLoad   `json:"load,omitempty"`
	Disks     []HostDisk  `json:"disks,omitempty"`
	Nets      []HostNet   `json:"nets,omitempty"`
}

type HostCPU struct {
	Cores int      `json:"cores,omitempty"`
	Model string   `json:"model,omitempty"`
	Usage *float64 `json:"usage,omitempty"`
}

type HostMemory struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

type HostLoad struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

type HostDisk struct {
	Mount string `json:"mount"`
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

type HostNet struct {
	Name  string   `json:"name"`
	MAC   string   `json:"mac,omitempty"`
	Addrs []string `json:"addrs,omitempty"`
	Up    bool     `json:"up"`
}

type DiffParams struct {
	Adapter  string `json:"adapter,omitempty"`
	Category string `json:"category,omitempty"`
}

// AgentError is the structured failure seam between a hub dispatcher and the
// web package. The status is deliberately carried by the error: callers do not
// need to know how a dispatcher reached an agent in order to render it.
type AgentError struct {
	Code   string
	Status int
	Err    error
}

func (e *AgentError) Error() string {
	if e == nil {
		return "agent: <nil>"
	}
	if e.Err == nil {
		return "agent: " + e.Code + ": <nil>"
	}
	return "agent: " + e.Code + ": " + e.Err.Error()
}

func (e *AgentError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type AgentsSource interface {
	ListAgents() []AgentInfo
	AgentStatus(ctx context.Context, agentID string) (json.RawMessage, error)
	AgentDiff(ctx context.Context, agentID string, params DiffParams) (string, error)
	AgentPush(ctx context.Context, agentID string, confirm bool, scope SyncScope) (json.RawMessage, error)
	AgentPull(ctx context.Context, agentID string, confirm bool, scope SyncScope) (json.RawMessage, error)
	// RemoveAgent drops a machine from the list entirely (optional:
	// embedded sources without removal keep 501 semantics).
	RemoveAgent(agentID string) bool
}

type ServeOptions struct {
	Addr          string
	HomerHome     string
	Token         string
	Identity      *AgentIdentity
	Agents        AgentsSource
	AgentEndpoint http.Handler
	// AgentEndpointAuthorized mirrors the agent API's machine-credential
	// check (per-agent secrets) for the web layer's /agent/v1/* gate.
	// When nil the endpoint's own authorized() still applies inside.
	AgentEndpointAuthorized func(r *http.Request) bool
	// Enrollment is the shared Tailscale-style enrollment manager: the
	// console mints one-time codes and revokes per-agent secrets through
	// it. Declared as an interface to keep the web package free of the
	// hub dependency (hub imports web for AgentsSource).
	Enrollment EnrollmentService
	// SyncDeps supplies the no-git data-plane transport for this server's
	// /api/push and /api/pull (an agent's own web endpoint executes pulls
	// pushed by the hub's dispatcher). When nil the legacy git transport
	// runs.
	SyncDeps SyncDepsSource
}

// SyncDepsSource builds command deps per request. The snapshot/secret
// pair is the no-git transport; the sink uploads prepared snapshots.
type SyncDepsSource interface {
	// PullDeps returns deps for /api/pull on this server's home.
	PullDeps() *commands.PullDeps
	// PushDeps returns deps for /api/push on this server's home.
	// adapters is nil for an unrestricted push and the explicit selection
	// otherwise; the sink must publish that selection as a merge.
	PushDeps(adapters []string) *commands.PushDeps
}

// EnrollmentService is the subset of the enrollment manager the console
// needs: minting join codes and revoking machines.
type EnrollmentService interface {
	Mint(ttl time.Duration) (string, error)
	Revoke(agentID string) bool
	// ValidCode reports whether a one-time enrollment code is still
	// redeemable without burning it (binary download on fresh machines).
	ValidCode(code string) bool
}

// Server is the P1 HTTP server. Its handler is built once so Handler can be
// passed directly to httptest, another server, or an agent daemon.
type Server struct {
	opts    ServeOptions
	handler http.Handler
	auth    *authStore
}

// writeMutex is intentionally package-global. Multiple Server values in one
// process still operate on the same Homer workspace in the common embedding
// and test scenarios, so push and pull must share one serialization point.
var writeMutex sync.Mutex

func NewServer(opts ServeOptions) (*Server, error) {
	if strings.TrimSpace(opts.Addr) == "" {
		return nil, errors.New("web server address is required")
	}
	// An empty HomerHome must resolve to the machine default (~/.homer),
	// never the process working directory — the storage layout
	// (generations/, keys/) is rooted there.
	if strings.TrimSpace(opts.HomerHome) == "" {
		paths := core.GetHomerPaths(func(key string) string {
			// HOMER_HOME must flow through the real environment so
			// tests (and deployments) that set it keep working.
			return os.Getenv(key)
		})
		opts.HomerHome = paths.Home
	}
	// Advisor rule 4: a hub without an administrator password refuses to
	// bind beyond loopback — finish first-run setup on the machine itself
	// (or over LAN) before exposing the console publicly. The token-only
	// path stays available for scripted deployments.
	hasPassword := false
	if info, err := os.Stat(filepath.Join(opts.HomerHome, "keys", "hub-password")); err == nil && !info.IsDir() {
		hasPassword = true
	}
	if opts.Token == "" && !hasPassword && !isLoopbackAddr(opts.Addr) {
		return nil, fmt.Errorf("尚未设置管理员密码：请先用回环地址启动（homer serve）并在浏览器完成初始化，或用 --token 提供机器令牌")
	}
	server := &Server{opts: opts, auth: newAuthStore(opts.HomerHome)}
	server.handler = http.HandlerFunc(server.serveHTTP)
	return server, nil
}

func (s *Server) Handler() http.Handler {
	if s == nil {
		return http.NotFoundHandler()
	}
	return s.handler
}

func (s *Server) ListenAndServe() error {
	if s == nil {
		return errors.New("nil web server")
	}
	server := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}
	return server.ListenAndServe()
}
