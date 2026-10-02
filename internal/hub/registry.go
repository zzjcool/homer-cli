package hub

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

type AgentInfo struct {
	AgentID  string    `json:"agentId"`
	Hostname string    `json:"hostname"`
	Mode     AgentMode `json:"mode"`
	Addr     string    `json:"addr,omitempty"`
	LastSeen time.Time `json:"lastSeen"`
	Version  string    `json:"version,omitempty"`
	Stale    bool      `json:"stale"`
	// Drift is the machine's last self-reported status summary relative
	// to the storage it last synced with (uploaded with each poll).
	Drift *AgentDrift `json:"drift,omitempty"`
	// Host is the machine's last self-reported resource snapshot
	// (CPU, memory, disks, interfaces), uploaded with each poll.
	Host *HostSnapshot `json:"host,omitempty"`
}

// AgentDrift is the per-machine status summary the console renders in the
// machine list: counts of local-only changes, center-only changes, and
// three-way conflicts.
type AgentDrift struct {
	Push      int    `json:"push"`
	Pull      int    `json:"pull"`
	Conflicts int    `json:"conflicts"`
	Error     string `json:"error,omitempty"`
}

type Registry struct {
	mu         sync.Mutex
	agents     map[string]*registryAgent
	tasks      map[string]*registryTask
	taskOwners map[string]string
}

type registryAgent struct {
	info   AgentInfo
	queue  []Task
	notify chan struct{}
}

type registryTask struct {
	task      Task
	inFlight  bool
	submitted bool
	expired   bool
	result    TaskResult
	done      chan struct{}
}

func NewRegistry() *Registry {
	return &Registry{
		agents:     make(map[string]*registryAgent),
		tasks:      make(map[string]*registryTask),
		taskOwners: make(map[string]string),
	}
}

func (r *Registry) Register(info AgentInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if info.AgentID == "" {
		return fmt.Errorf("hub: agent ID is required")
	}
	switch info.Mode {
	case AgentModeListen:
		if info.Addr == "" {
			return fmt.Errorf("hub: listen agent %q must have an address", info.AgentID)
		}
	case AgentModeConnect:
	default:
		return fmt.Errorf("hub: invalid mode %q", info.Mode)
	}

	if info.LastSeen.IsZero() {
		info.LastSeen = time.Now()
	}
	info.Stale = false
	info.Host = normalizeHost(info.Host)
	if agent, ok := r.agents[info.AgentID]; ok {
		// Re-registration (listen agents do this every minute) must not
		// wipe the last drift or resource report when the payload omits them.
		if info.Host == nil {
			info.Host = agent.info.Host
		}
		if info.Drift == nil {
			info.Drift = agent.info.Drift
		}
		agent.info = info
		return nil
	}
	r.agents[info.AgentID] = &registryAgent{
		info:   info,
		notify: make(chan struct{}),
	}
	return nil
}

// UpdateDrift caches a machine's self-reported status summary.
func (r *Registry) UpdateDrift(agentID string, drift AgentDrift) {
	r.mu.Lock()
	defer r.mu.Unlock()
	agent, ok := r.agents[agentID]
	if !ok {
		return
	}
	agent.info.Drift = &drift
}

// UpdateHost caches a machine's self-reported resource snapshot.
func (r *Registry) UpdateHost(agentID string, host HostSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	agent, ok := r.agents[agentID]
	if !ok {
		return
	}
	if normalized := normalizeHost(&host); normalized != nil {
		agent.info.Host = normalized
	}
}

func (r *Registry) Touch(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if agent, ok := r.agents[agentID]; ok {
		agent.info.LastSeen = time.Now()
		agent.info.Stale = false
	}
}

func (r *Registry) Get(agentID string) (AgentInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	agent, ok := r.agents[agentID]
	if !ok {
		return AgentInfo{}, false
	}
	// Staleness is derived from LastSeen at read time — a registered
	// agent that stopped polling must read as stale even without a
	// List() pass (fast-fail paths key on this).
	info := agent.info
	info.Stale = time.Since(info.LastSeen) >= AgentStaleAfter
	info.Host = normalizeHost(info.Host)
	return info, true
}

// taskInfo is a read-only protocol helper used by the report endpoint. It is
// deliberately unexported so the frozen Registry API remains unchanged.
func (r *Registry) taskInfo(taskID string) (Task, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.tasks[taskID]
	if !ok || state.expired || state.submitted {
		return Task{}, "", false
	}
	for agentID, agent := range r.agents {
		for _, queued := range agent.queue {
			if queued.TaskID == taskID {
				return state.task, agentID, true
			}
		}
	}
	// A task is removed from the queue when a poller claims it. At that point
	// the task state still records enough information for the endpoint to
	// validate the reporting agent without exposing mutable Registry state.
	return state.task, stateAgentIDLocked(r, taskID), state.inFlight
}

// stateAgentIDLocked finds the owner from the immutable queue/agent relation.
// The task owner is populated by Enqueue through taskOwners; this helper is
// kept separate to make the read-only lookup easy to audit.
func stateAgentIDLocked(r *Registry, taskID string) string {
	return r.taskOwners[taskID]
}

func (r *Registry) List() []AgentInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	agents := make([]AgentInfo, 0, len(r.agents))
	for _, agent := range r.agents {
		info := agent.info
		info.Stale = now.Sub(info.LastSeen) >= AgentStaleAfter
		info.Host = normalizeHost(info.Host)
		agents = append(agents, info)
	}
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].AgentID < agents[j].AgentID
	})
	return agents
}

func (r *Registry) Enqueue(agentID string, task Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	agent, ok := r.agents[agentID]
	if !ok {
		return fmt.Errorf("hub: unknown agent %q", agentID)
	}

	r.purgeExpiredQueuedLocked(agent)
	if len(agent.queue) >= TaskQueueCapacity {
		return fmt.Errorf("hub: task queue for agent %q is full", agentID)
	}
	if _, ok := r.tasks[task.TaskID]; ok {
		return fmt.Errorf("hub: task %q already exists", task.TaskID)
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now()
	}

	r.tasks[task.TaskID] = &registryTask{
		task: task,
		done: make(chan struct{}),
	}
	r.taskOwners[task.TaskID] = agentID
	agent.queue = append(agent.queue, task)
	r.notifyAgentLocked(agent)
	return nil
}

func (r *Registry) Poll(agentID string, wait time.Duration, ctx context.Context) (Task, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return Task{}, false
	}

	var timer *time.Timer
	var timerCh <-chan time.Time
	if wait > 0 {
		timer = time.NewTimer(wait)
		timerCh = timer.C
		defer timer.Stop()
	}

	for {
		if ctx.Err() != nil {
			return Task{}, false
		}

		r.mu.Lock()
		agent, ok := r.agents[agentID]
		if !ok {
			r.mu.Unlock()
			return Task{}, false
		}
		agent.info.LastSeen = time.Now()
		agent.info.Stale = false

		for len(agent.queue) > 0 {
			task := agent.queue[0]
			agent.queue = agent.queue[1:]
			state, exists := r.tasks[task.TaskID]
			if !exists || state.expired || state.submitted {
				continue
			}
			if r.taskExpired(task) {
				r.expireTaskLocked(task.TaskID, state)
				continue
			}
			state.inFlight = true
			r.mu.Unlock()
			return task, true
		}

		if wait <= 0 {
			r.mu.Unlock()
			return Task{}, false
		}
		notify := agent.notify
		r.mu.Unlock()

		select {
		case <-notify:
			continue
		case <-ctx.Done():
			return Task{}, false
		case <-timerCh:
			return Task{}, false
		}
	}
}

func (r *Registry) Submit(result TaskResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.tasks[result.TaskID]
	if !ok || state.expired {
		return fmt.Errorf("hub: unknown or expired task %q", result.TaskID)
	}
	if !state.inFlight || state.submitted {
		return fmt.Errorf("hub: task %q is not awaiting a result", result.TaskID)
	}
	if r.taskExpired(state.task) {
		r.expireTaskLocked(result.TaskID, state)
		return fmt.Errorf("hub: task %q has expired", result.TaskID)
	}

	state.result = cloneTaskResult(result)
	state.inFlight = false
	state.submitted = true
	delete(r.taskOwners, result.TaskID)
	close(state.done)
	return nil
}

func (r *Registry) Wait(ctx context.Context, taskID string) (TaskResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return TaskResult{}, err
	}

	r.mu.Lock()
	state, ok := r.tasks[taskID]
	if !ok || state.expired {
		r.mu.Unlock()
		return TaskResult{}, fmt.Errorf("hub: unknown or expired task %q", taskID)
	}
	if state.submitted {
		result := cloneTaskResult(state.result)
		r.mu.Unlock()
		return result, nil
	}
	if r.taskExpired(state.task) {
		r.expireTaskLocked(taskID, state)
		r.mu.Unlock()
		return TaskResult{}, fmt.Errorf("hub: task %q has expired", taskID)
	}

	done := state.done
	remaining := time.Until(state.task.CreatedAt.Add(TaskTTL))
	r.mu.Unlock()

	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		r.mu.Lock()
		defer r.mu.Unlock()
		if state.submitted {
			return cloneTaskResult(state.result), nil
		}
		return TaskResult{}, fmt.Errorf("hub: task %q has expired", taskID)
	case <-ctx.Done():
		return TaskResult{}, ctx.Err()
	case <-timer.C:
		r.mu.Lock()
		defer r.mu.Unlock()
		if state.submitted {
			return cloneTaskResult(state.result), nil
		}
		if !state.expired {
			r.expireTaskLocked(taskID, state)
		}
		return TaskResult{}, fmt.Errorf("hub: task %q has expired", taskID)
	}
}

func (r *Registry) taskExpired(task Task) bool {
	return !time.Now().Before(task.CreatedAt.Add(TaskTTL))
}

func (r *Registry) purgeExpiredQueuedLocked(agent *registryAgent) {
	if len(agent.queue) == 0 {
		return
	}
	queue := agent.queue[:0]
	for _, task := range agent.queue {
		state, ok := r.tasks[task.TaskID]
		if !ok || state.expired || state.submitted {
			continue
		}
		if r.taskExpired(task) {
			r.expireTaskLocked(task.TaskID, state)
			continue
		}
		queue = append(queue, task)
	}
	agent.queue = queue
}

func (r *Registry) notifyAgentLocked(agent *registryAgent) {
	close(agent.notify)
	agent.notify = make(chan struct{})
}

func (r *Registry) expireTaskLocked(taskID string, state *registryTask) {
	if state.expired || state.submitted {
		return
	}
	state.expired = true
	state.inFlight = false
	if current, ok := r.tasks[taskID]; ok && current == state {
		delete(r.tasks, taskID)
	}
	delete(r.taskOwners, taskID)
	close(state.done)
}

func cloneTaskResult(result TaskResult) TaskResult {
	if result.Report != nil {
		result.Report = append([]byte(nil), result.Report...)
	}
	return result
}

// Remove drops a machine from the registry entirely (revocation only kills
// the credential). The removed agent 404s on its next poll and re-registers
// through the standard recovery path if it still holds a valid credential.
func (r *Registry) Remove(agentID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[agentID]; !ok {
		return false
	}
	delete(r.agents, agentID)
	return true
}
