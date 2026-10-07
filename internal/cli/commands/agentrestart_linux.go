//go:build linux

package commands

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zzjcool/homer-cli/internal/core"
)

// RestartRunningAgents reloads homer agent processes that are already
// running so they execute the binary just installed. A stopped agent is
// left stopped.
func RestartRunningAgents(binary, home string) (string, error) {
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" && strings.TrimSpace(home) != "" {
			return home
		}
		return os.Getenv(key)
	})
	return systemRestarter().run(binary, filepath.Join(paths.Home, "agent.log"))
}

func systemRestarter() restarter {
	return restarter{
		self:        os.Getpid(),
		list:        listAgentProcesses,
		stop:        stopAgentProcess,
		start:       startAgentProcess,
		unit:        lookupUserUnit,
		restartUnit: restartUserUnit,
	}
}

func listAgentProcesses() ([]agentProc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var procs []agentProc
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || len(data) == 0 {
			continue
		}
		args := splitCmdline(data)
		if !isHomerAgentArgs(args) {
			continue
		}
		procs = append(procs, agentProc{PID: pid, Args: args})
	}
	return procs, nil
}

func splitCmdline(data []byte) []string {
	parts := strings.Split(string(data), "\x00")
	var args []string
	for _, part := range parts {
		if part == "" {
			continue
		}
		args = append(args, part)
	}
	return args
}

func restartUserUnit() error {
	cmd := exec.Command("systemctl", "--user", "restart", agentUnitName)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return errors.New(detail)
	}
	return nil
}

func lookupUserUnit() (bool, int, error) {
	cmd := exec.Command("systemctl", "--user", "show", "-p", "ActiveState", "-p", "MainPID", "--value", agentUnitName)
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		return false, 0, nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "active" {
		return false, 0, nil
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(lines[1]))
	return true, pid, nil
}

func stopAgentProcess(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return nil
}

func startAgentProcess(binary string, args []string, logPath string) error {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(binary, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
