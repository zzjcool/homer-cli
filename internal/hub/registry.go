package hub

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/toolctl"
)

type AgentInfo struct {
	AgentID  string    `json:"agentId"`
	Hostname string    `json:"hostname"`
	LastSeen time.Time `json:"lastSeen"`
	Version  string    `json:"version,omitempty"`
	Stale    bool      `json:"stale"`
	// Drift is the machine's last self-reported status summary relative to
	// the storage it last synced with.
	Drift *AgentDrift `json:"drift,omitempty"`
	// Host is the machine's last self-reported resource snapshot.
	Host *HostSnapshot `json:"host,omitempty"`
	// Tools is the machine's last self-reported list of installed programs.
	// Nil means "not reported yet"; an empty list means "none installed".
	Tools []toolctl.Status `json:"tools,omitempty"`

	// caps is negotiated by hello and is private because it is used only by
	// the dispatcher to avoid sending methods an agent did not advertise.
	caps []string
}

type AgentDrift struct {
	Push      int    `json:"push"`
	Pull      int    `json:"pull"`
	Conflicts int    `json:"conflicts"`
	Error     string `json:"error,omitempty"`
	// Resolutions counts adapters whose conflict currently has a recorded
	// decision (staged-resolution plan, D8). 0 is omitted.
	Resolutions int `json:"resolutions,omitempty"`
}

type Registry struct {
	mu     sync.Mutex
	agents map[string]*registryAgent
}

type registryAgent struct {
	info             AgentInfo
	session          *stream.Session
	ReportedAdapters []string
}

func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]*registryAgent)}
}

// Attach binds an agent's current WebSocket session. Reattaching an identity
// replaces its current session and returns the previous one so the hub can
// close it as superseded. Reports omitted by a reconnect are kept.
func (r *Registry) Attach(info AgentInfo, sess *stream.Session) (prev *stream.Session) {
	if r == nil {
		return nil
	}
	info.AgentID = strings.TrimSpace(info.AgentID)
	if info.AgentID == "" {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if info.LastSeen.IsZero() {
		info.LastSeen = time.Now()
	}
	info.Stale = false
	info.Host = normalizeHost(info.Host)
	info.Tools = normalizeTools(info.Tools)
	info.caps = cloneStrings(info.caps)
	if current, ok := r.agents[info.AgentID]; ok {
		prev = current.session
		if info.Host == nil {
			info.Host = current.info.Host
		}
		if info.Tools == nil {
			info.Tools = current.info.Tools
		}
		if info.Drift == nil {
			info.Drift = current.info.Drift
		}
		if info.Version == "" {
			info.Version = current.info.Version
		}
		// A fresh hello negotiates capabilities anew. This intentionally does
		// not retain a method the new process no longer advertises.
		r.agents[info.AgentID] = &registryAgent{info: cloneAgentInfo(info), session: sess}
		return prev
	}
	r.agents[info.AgentID] = &registryAgent{info: cloneAgentInfo(info), session: sess}
	return nil
}

// Detach clears an agent's session only if sess is still the current one.
// A late return from a superseded connection must not detach its replacement.
func (r *Registry) Detach(agentID string, sess *stream.Session) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if agent, ok := r.agents[agentID]; ok && agent.session == sess {
		agent.session = nil
	}
}

// Session returns the currently attached, still-live session.
func (r *Registry) Session(agentID string) (*stream.Session, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	agent, ok := r.agents[agentID]
	if !ok || agent.session == nil || sessionDone(agent.session) {
		return nil, false
	}
	return agent.session, true
}

func (r *Registry) UpdateVersion(agentID, version string) {
	if r == nil {
		return
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if agent, ok := r.agents[agentID]; ok {
		agent.info.Version = version
	}
}

func (r *Registry) UpdateDrift(agentID string, drift AgentDrift) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if agent, ok := r.agents[agentID]; ok {
		copy := drift
		agent.info.Drift = &copy
	}
}

// NoteResolutions updates the optimistic staged-decision count without
// creating a drift summary for agents that have not reported status yet.
func (r *Registry) NoteResolutions(agentID string, pending int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if agent, ok := r.agents[agentID]; ok && agent.info.Drift != nil {
		agent.info.Drift.Resolutions = pending
	}
}

func (r *Registry) UpdateHost(agentID string, host HostSnapshot) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if agent, ok := r.agents[agentID]; ok {
		if normalized := normalizeHost(&host); normalized != nil {
			agent.info.Host = normalized
		}
	}
}

func (r *Registry) UpdateTools(agentID string, tools []toolctl.Status) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if agent, ok := r.agents[agentID]; ok {
		agent.info.Tools = normalizeTools(tools)
		if agent.info.Tools == nil {
			agent.info.Tools = []toolctl.Status{}
		}
	}
}

// NoteWriteOutcome updates the cached drift summary from a push/pull report.
func (r *Registry) NoteWriteOutcome(agentID, status string, ok bool, conflicts int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	agent, exists := r.agents[agentID]
	if !exists {
		return
	}
	switch status {
	case "conflicts", "conflicts-remain":
		if conflicts < 1 {
			conflicts = 1
		}
		resolutions := 0
		if agent.info.Drift != nil {
			resolutions = agent.info.Drift.Resolutions
		}
		agent.info.Drift = &AgentDrift{Conflicts: conflicts, Resolutions: resolutions}
	case "applied", "no-drift", "resolved", "no-conflicts":
		if !ok {
			return
		}
		fresh := agent.info.Drift != nil && strings.Contains(agent.info.Drift.Error, "未找到 homer 配置")
		hadConflicts := agent.info.Drift != nil && agent.info.Drift.Conflicts > 0
		if agent.info.Drift == nil || fresh || hadConflicts {
			agent.info.Drift = &AgentDrift{}
		}
	}
}

func (r *Registry) NoteReportedAdapters(agentID string, ids []string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if agent, ok := r.agents[agentID]; ok {
		agent.ReportedAdapters = cloneStrings(ids)
	}
}

func (r *Registry) NoteToolVersion(agentID, toolID, version string) {
	if r == nil {
		return
	}
	version = clipText(version, maxToolText)
	if version == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	agent, ok := r.agents[agentID]
	if !ok {
		return
	}
	updated := append([]toolctl.Status(nil), agent.info.Tools...)
	for i := range updated {
		if updated[i].ID == toolID {
			updated[i].Version = version
			updated[i].Error = ""
			agent.info.Tools = updated
			return
		}
	}
}

// Touch updates LastSeen. AgentHub wraps every session connection's Read so
// all successfully received frames refresh the timestamp, not just heartbeats.
func (r *Registry) Touch(agentID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if agent, ok := r.agents[agentID]; ok {
		agent.info.LastSeen = time.Now()
	}
}

func (r *Registry) Get(agentID string) (AgentInfo, bool) {
	if r == nil {
		return AgentInfo{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	agent, ok := r.agents[agentID]
	if !ok {
		return AgentInfo{}, false
	}
	info := cloneAgentInfo(agent.info)
	info.Stale = agentIsStale(agent, time.Now())
	return info, true
}

func (r *Registry) List() []AgentInfo {
	if r == nil {
		return []AgentInfo{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	agents := make([]AgentInfo, 0, len(r.agents))
	for _, agent := range r.agents {
		info := cloneAgentInfo(agent.info)
		info.Stale = agentIsStale(agent, now)
		agents = append(agents, info)
	}
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].AgentID < agents[j].AgentID
	})
	return agents
}

// Remove removes the machine's cached identity and current stream binding.
func (r *Registry) Remove(agentID string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[agentID]; !ok {
		return false
	}
	delete(r.agents, agentID)
	return true
}

func agentIsStale(agent *registryAgent, now time.Time) bool {
	return agent.session == nil || sessionDone(agent.session) || now.Sub(agent.info.LastSeen) >= AgentStaleAfter
}

func sessionDone(session *stream.Session) bool {
	if session == nil {
		return true
	}
	select {
	case <-session.Done():
		return true
	default:
		return false
	}
}

func cloneAgentInfo(info AgentInfo) AgentInfo {
	if info.Drift != nil {
		drift := *info.Drift
		info.Drift = &drift
	}
	info.Host = normalizeHost(info.Host)
	info.Tools = cloneTools(info.Tools)
	info.caps = cloneStrings(info.caps)
	return info
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append(make([]string, 0, len(values)), values...)
}
