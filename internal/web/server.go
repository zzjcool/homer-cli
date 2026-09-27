package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
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
	AgentPush(ctx context.Context, agentID string, confirm bool) (json.RawMessage, error)
	AgentPull(ctx context.Context, agentID string, confirm bool) (json.RawMessage, error)
}

type ServeOptions struct {
	Addr          string
	HomerHome     string
	Token         string
	Identity      *AgentIdentity
	Agents        AgentsSource
	AgentEndpoint http.Handler
}

// Server is the P1 HTTP server. Its handler is built once so Handler can be
// passed directly to httptest, another server, or an agent daemon.
type Server struct {
	opts    ServeOptions
	handler http.Handler
}

// writeMutex is intentionally package-global. Multiple Server values in one
// process still operate on the same Homer workspace in the common embedding
// and test scenarios, so push and pull must share one serialization point.
var writeMutex sync.Mutex

func NewServer(opts ServeOptions) (*Server, error) {
	if strings.TrimSpace(opts.Addr) == "" {
		return nil, errors.New("web server address is required")
	}
	if opts.Token == "" && !isLoopbackAddr(opts.Addr) {
		return nil, fmt.Errorf("token 为空时 Addr 必须是回环地址: %s", opts.Addr)
	}
	server := &Server{opts: opts}
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
