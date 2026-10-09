//go:build linux

package e2e

import (
	"os/exec"
	"syscall"
)

// orphanGuard makes a child test process die with its parent and puts it in
// its own process group. Without it, a go test that is killed or hits -timeout
// skips every t.Cleanup and leaves hubs and agents running; a helper agent
// would then retry-reconnect forever. Pdeathsig also survives the in-place
// syscall.Exec an upgrade performs.
func orphanGuard(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
