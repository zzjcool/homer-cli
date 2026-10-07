package commands

import (
	"path/filepath"
)

// agentProc is one running `homer agent` process.
type agentProc struct {
	PID  int
	Args []string
}

// restarter finds and restarts agents. Tests supply fakes; the real
// wiring lives in the OS-specific files.
type restarter struct {
	self        int
	list        func() ([]agentProc, error)
	stop        func(pid int) error
	start       func(binary string, args []string, logPath string) error
	unit        func() (active bool, pid int, err error)
	restartUnit func() error
}

const (
	agentUnitName      = "homer-agent.service"
	agentRestartedNote = ">> agent 已重启"
	agentStoppedNote   = ">> 没有正在运行的 agent，下次启动会用新程序"
)

// isHomerAgentArgs reports whether argv is a homer agent process.
// The command word is the second argument (`homer agent ...`).
func isHomerAgentArgs(args []string) bool {
	if len(args) < 2 {
		return false
	}
	if filepath.Base(args[0]) != "homer" {
		return false
	}
	return args[1] == "agent"
}

type restartPlan struct {
	systemd  bool
	relaunch []agentProc
}

// planAgentRestart decides how to reload agents onto a new binary.
// A systemd-managed agent is restarted through its unit so the supervisor
// does not start a second copy. Other agent processes are stopped and
// started again with the same arguments. A stopped agent stays stopped.
func planAgentRestart(selfPID, unitPID int, unitActive bool, procs []agentProc) restartPlan {
	var plan restartPlan
	if unitActive && unitPID > 0 {
		plan.systemd = true
	}
	for _, proc := range procs {
		if proc.PID == 0 || proc.PID == selfPID {
			continue
		}
		if unitActive && proc.PID == unitPID {
			continue
		}
		if !isHomerAgentArgs(proc.Args) {
			continue
		}
		plan.relaunch = append(plan.relaunch, proc)
	}
	return plan
}

func (r restarter) run(binary, logPath string) (string, error) {
	active, unitPID, _ := r.unit()
	procs, err := r.list()
	if err != nil && !(active && unitPID > 0) {
		return "", err
	}
	plan := planAgentRestart(r.self, unitPID, active, procs)
	if !plan.systemd && len(plan.relaunch) == 0 {
		return agentStoppedNote, nil
	}
	if plan.systemd {
		if err := r.restartUnit(); err != nil {
			return "", err
		}
	}
	for _, proc := range plan.relaunch {
		if err := r.stop(proc.PID); err != nil {
			return "", err
		}
		if err := r.start(binary, proc.Args[1:], logPath); err != nil {
			return "", err
		}
	}
	return agentRestartedNote, nil
}
