package commands

import (
	"errors"
	"testing"
)

func TestIsHomerAgentArgs(t *testing.T) {
	if !isHomerAgentArgs([]string{"/home/user/.local/bin/homer", "agent", "--connect", "http://127.0.0.1:7760"}) {
		t.Fatal("agent argv should match")
	}
	if isHomerAgentArgs([]string{"/home/user/.local/bin/homer", "serve"}) {
		t.Fatal("serve is not an agent")
	}
	if isHomerAgentArgs([]string{"/home/user/.local/bin/homer", "upgrade"}) {
		t.Fatal("upgrade is not an agent")
	}
}

func TestPlanAgentRestartLeavesStoppedAgentAlone(t *testing.T) {
	plan := planAgentRestart(1, 0, false, nil)
	if plan.systemd || len(plan.relaunch) != 0 {
		t.Fatalf("stopped agent should stay stopped: %+v", plan)
	}
}

func TestPlanAgentRestartUsesSystemdForTheUnit(t *testing.T) {
	procs := []agentProc{
		{PID: 10, Args: []string{"/usr/bin/homer", "agent", "--connect", "http://127.0.0.1:7760"}},
		{PID: 11, Args: []string{"/usr/bin/homer", "agent", "--connect", "https://example"}},
		{PID: 12, Args: []string{"/usr/bin/homer", "serve"}},
	}
	plan := planAgentRestart(1, 10, true, procs)
	if !plan.systemd {
		t.Fatal("active unit should be restarted through systemd")
	}
	if len(plan.relaunch) != 1 || plan.relaunch[0].PID != 11 {
		t.Fatalf("only the stray agent should be relaunched: %+v", plan.relaunch)
	}
}

func TestRestarterRestartsStrayAgentAndSkipsWhenNone(t *testing.T) {
	var stopped []int
	var started [][]string
	r := restarter{
		self: 1,
		list: func() ([]agentProc, error) {
			return []agentProc{{PID: 11, Args: []string{"/old/homer", "agent", "--id", "box"}}}, nil
		},
		stop: func(pid int) error {
			stopped = append(stopped, pid)
			return nil
		},
		start: func(binary string, args []string, logPath string) error {
			started = append(started, append([]string{binary, logPath}, args...))
			return nil
		},
		unit:        func() (bool, int, error) { return false, 0, nil },
		restartUnit: func() error { return nil },
	}
	note, err := r.run("/new/homer", "/tmp/agent.log")
	if err != nil {
		t.Fatal(err)
	}
	if note != agentRestartedNote {
		t.Fatalf("note %q", note)
	}
	if len(stopped) != 1 || stopped[0] != 11 {
		t.Fatalf("stopped %v", stopped)
	}
	if len(started) != 1 || started[0][0] != "/new/homer" || started[0][2] != "agent" {
		t.Fatalf("started %v", started)
	}

	r.list = func() ([]agentProc, error) { return nil, nil }
	note, err = r.run("/new/homer", "/tmp/agent.log")
	if err != nil {
		t.Fatal(err)
	}
	if note != agentStoppedNote {
		t.Fatalf("note %q", note)
	}
}

func TestRestarterReportsStopFailure(t *testing.T) {
	r := restarter{
		self: 1,
		list: func() ([]agentProc, error) {
			return []agentProc{{PID: 11, Args: []string{"homer", "agent"}}}, nil
		},
		stop:        func(int) error { return errors.New("拒绝") },
		start:       func(string, []string, string) error { return nil },
		unit:        func() (bool, int, error) { return false, 0, nil },
		restartUnit: func() error { return nil },
	}
	if _, err := r.run("/new/homer", "/tmp/agent.log"); err == nil {
		t.Fatal("expected stop failure")
	}
}
