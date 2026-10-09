//go:build !linux

package e2e

import "os/exec"

// orphanGuard is a no-op where Pdeathsig does not exist. Cleanup still runs on
// a normal test exit; only a killed test binary can leave children behind.
func orphanGuard(*exec.Cmd) {}
