package toolctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// maxOutput caps what is kept of one command's output. The end of the output
// is what explains a failure, so the beginning is what gets dropped.
const maxOutput = 64 << 10

type output struct {
	stdout string
	stderr string
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if over := len(b.data) - b.limit; over > 0 {
		b.data = append(b.data[:0], b.data[over:]...)
	}
	return len(p), nil
}

// errTimeout marks a command that was stopped for running too long.
type errTimeout struct{ after time.Duration }

func (e errTimeout) Error() string {
	return fmt.Sprintf("超过 %s 还没有结束，已经停止", e.after)
}

// run executes bin with args against path. It never goes through a shell: bin
// and args reach the program verbatim. The command gets no input, its own
// process group (so a timeout also stops what it spawned), and a bounded
// amount of output.
func run(ctx context.Context, bin string, args []string, path string, timeout time.Duration, extraEnv []string) (output, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Env = commandEnv(path, extraEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// A descendant that outlives the command and keeps its pipes open must
	// not hold this call hostage after the command itself is done.
	cmd.WaitDelay = time.Second
	stdout := &tailBuffer{limit: maxOutput}
	stderr := &tailBuffer{limit: maxOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err := cmd.Run()
	out := output{stdout: string(stdout.data), stderr: string(stderr.data)}
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return out, errTimeout{after: timeout}
	}
	return out, err
}

// commandEnv is this process's environment with PATH replaced and prompts
// switched off. TERM=dumb and NO_COLOR keep escape sequences out of output.
func commandEnv(path string, extra []string) []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "PATH="+path, "TERM=dumb", "NO_COLOR=1")
	return append(env, extra...)
}

// failureReason is the one sentence worth showing for a failed command: the
// last line it printed, or else why it stopped.
func failureReason(out output, err error) string {
	var timeout errTimeout
	if errors.As(err, &timeout) {
		return timeout.Error()
	}
	if line := lastLine(out.stderr); line != "" {
		return line
	}
	if line := lastLine(out.stdout); line != "" {
		return line
	}
	return "命令没有正常结束（" + clip(err.Error(), 160) + "）"
}
