package upgrade

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// runTar extracts the archive with the system tar inside a bounded timeout
// and its own process group, mirroring the process hygiene used by gitx and
// the manifest port.
func runTar(archivePath, dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tar", "-xzf", archivePath, "-C", dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// SelfPath returns the absolute path of the running homer binary.
func SelfPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(executable)
}
