//go:build !linux

package commands

import "errors"

// RestartRunningAgents is a no-op off Linux. The binary is already
// replaced; this platform has no process scan to restart a running agent.
func RestartRunningAgents(string, string) (string, error) {
	return "", errors.New("这个系统上需要手动重新启动 agent")
}
