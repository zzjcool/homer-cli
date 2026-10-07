//go:build linux

package agentd

import (
	"os"
	"syscall"
)

// reexecAgent replaces this process with the homer binary on disk. The
// upgrade writes that path first and reports success; the running process
// is still the previous inode until this call.
func reexecAgent() {
	bin, err := os.Executable()
	if err != nil || bin == "" || len(os.Args) == 0 {
		return
	}
	_ = syscall.Exec(bin, os.Args, os.Environ())
}
