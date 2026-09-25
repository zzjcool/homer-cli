package pair

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// TailcatOptions configures the optional tailcat CLI transport.
type TailcatOptions struct {
	Binary          string
	AddrFileTimeout time.Duration
	ExtraEnv        []string
	Stderr          io.Writer
}

// TailcatTransport is the production PairTransport. tailcat is deliberately
// kept behind the small PairTransport seam: Homer owns the bytes carried over
// stdin/stdout, while tailcat only establishes the one-shot tunnel.
type TailcatTransport struct {
	binary          string
	addrFileTimeout time.Duration
	extraEnv        []string
	stderr          io.Writer
}

var (
	_ PairTransport = (*TailcatTransport)(nil)
	_ PairServer    = (*tailcatServer)(nil)
)

var (
	errTailcatClosed        = errors.New("tailcat session is closed")
	errTailcatAlreadyAccept = errors.New("tailcat session already accepted")
)

const tailcatCloseWait = 2 * time.Second

// NewTailcatTransport resolves and validates the tailcat executable once. The
// resolved path is retained so a later PATH mutation cannot make a running
// command select a different binary.
func NewTailcatTransport(options TailcatOptions) (*TailcatTransport, error) {
	binary := options.Binary
	var err error
	if binary == "" {
		binary, err = DetectTailcat(nil)
	} else {
		binary, err = DetectTailcat(func(string) (string, error) {
			return exec.LookPath(options.Binary)
		})
	}
	if err != nil {
		return nil, ErrNoTailcat
	}

	timeout := options.AddrFileTimeout
	if timeout <= 0 {
		timeout = AddrFileTimeout
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	return &TailcatTransport{
		binary:          binary,
		addrFileTimeout: timeout,
		extraEnv:        append([]string(nil), options.ExtraEnv...),
		stderr:          stderr,
	}, nil
}

// Serve starts tailcat's pipe-mode server. The address file is intentionally
// created before starting the child so the path itself is private and has
// 0600 permissions from its first filesystem appearance.
func (transport *TailcatTransport) Serve(ctx context.Context) (PairServer, error) {
	ctx = nonNilContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	addressFile, err := os.CreateTemp("", "homer-tailcat-addr-")
	if err != nil {
		return nil, fmt.Errorf("create tailcat address file: %w", err)
	}
	addressPath := addressFile.Name()
	cleanupAddress := func() { _ = os.Remove(addressPath) }
	if err := addressFile.Chmod(0o600); err != nil {
		_ = addressFile.Close()
		cleanupAddress()
		return nil, fmt.Errorf("secure tailcat address file: %w", err)
	}
	if err := addressFile.Close(); err != nil {
		cleanupAddress()
		return nil, fmt.Errorf("close tailcat address file: %w", err)
	}

	proc, err := transport.start(ctx, []string{"--key=new"}, transport.commandEnv(addressPath))
	if err != nil {
		cleanupAddress()
		return nil, err
	}

	timeout := transport.addrFileTimeout
	if timeout <= 0 {
		timeout = AddrFileTimeout
	}
	address, waitErr := waitForTailcatAddress(ctx, proc, addressPath, timeout)
	if waitErr == nil {
		return &tailcatServer{
			process:     proc,
			address:     address,
			addressPath: addressPath,
			closed:      make(chan struct{}),
		}, nil
	}
	// Calling Close here is required for the timeout and child-exit paths,
	// and is idempotent for cancellation. Preserve the address-wait error
	// rather than replacing a useful timeout with signal: killed.
	_ = proc.close()
	cleanupAddress()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, waitErr
}

// Connect starts tailcat's pipe-mode client. The explicit `--` is part of
// the argv contract: a bearer address beginning with '-' must remain data,
// never become a tailcat option.
func (transport *TailcatTransport) Connect(ctx context.Context, addr string) (io.ReadWriteCloser, error) {
	ctx = nonNilContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(addr) == "" {
		return nil, errors.New("tailcat address must not be empty")
	}
	// The context bounds dialing, not the lifetime of the established stream.
	// RunPairJoin cancels its connect timeout as soon as Connect returns; tying
	// the child watcher to that same context would kill a healthy session before
	// the hello frame is sent.
	proc, err := transport.start(context.Background(), []string{"--", addr}, transport.commandEnv(""))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = proc.close()
		return nil, err
	}
	return &tailcatStream{process: proc}, nil
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// commandEnv intentionally does not pass Homer configuration, credentials,
// or arbitrary ambient variables to the optional child. PATH/HOME/TERM are
// the terminal/runtime contract tailcat needs; ExtraEnv is the explicit
// escape hatch for deployment-specific settings such as DERP maps.
func (transport *TailcatTransport) commandEnv(addressPath string) []string {
	keys := []string{"PATH", "HOME", "TERM"}
	env := make([]string, 0, len(keys)+len(transport.extraEnv)+1)
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	for _, item := range transport.extraEnv {
		key, _, ok := strings.Cut(item, "=")
		if !ok || key == "" || strings.ContainsRune(key, 0) {
			// exec.Cmd would reject malformed entries. Ignoring one malformed
			// optional entry keeps the child boundary deterministic and avoids
			// turning an otherwise usable transport into an opaque Start error.
			continue
		}
		removeEnvKey(&env, key)
		env = append(env, item)
	}
	if addressPath != "" {
		removeEnvKey(&env, "TAILCAT_ADDR_FILE")
		env = append(env, "TAILCAT_ADDR_FILE="+addressPath)
	}
	return env
}

func removeEnvKey(env *[]string, key string) {
	prefix := key + "="
	filtered := (*env)[:0]
	for _, item := range *env {
		if !strings.HasPrefix(item, prefix) {
			filtered = append(filtered, item)
		}
	}
	*env = filtered
}

func (transport *TailcatTransport) start(ctx context.Context, args, env []string) (*tailcatProcess, error) {
	cmd := exec.Command(transport.binary, args...)
	cmd.Env = env
	cmd.Stderr = transport.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("prepare tailcat stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("prepare tailcat stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start tailcat: %w", err)
	}

	process := &tailcatProcess{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		done:      make(chan struct{}),
		watchStop: make(chan struct{}),
	}
	go process.waitChild()
	if done := ctx.Done(); done != nil {
		go process.watchContext(ctx)
	}
	return process, nil
}

type tailcatProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	done chan struct{}
	mu   sync.Mutex
	err  error

	closeOnce sync.Once
	closeErr  error
	watchStop chan struct{}
	watchOnce sync.Once
}

func (process *tailcatProcess) waitChild() {
	err := process.cmd.Wait()
	process.mu.Lock()
	process.err = err
	process.mu.Unlock()
	close(process.done)
}

func (process *tailcatProcess) watchContext(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = process.close()
	case <-process.done:
	case <-process.watchStop:
	}
}

func (process *tailcatProcess) waitError() error {
	<-process.done
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.err
}

func (process *tailcatProcess) close() error {
	process.closeOnce.Do(func() {
		process.watchOnce.Do(func() { close(process.watchStop) })

		// Kill the process group, not only the shell/binary entry point. This
		// matters for tailcat helpers that retain the pipe or spawn a network
		// worker while the parent is being torn down.
		if process.cmd.Process != nil {
			if err := syscall.Kill(-process.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				process.closeErr = err
			}
		}
		_ = process.stdin.Close()
		_ = process.stdout.Close()

		select {
		case <-process.done:
		case <-time.After(tailcatCloseWait):
			if process.closeErr == nil {
				process.closeErr = errors.New("timed out waiting for tailcat process group")
			}
		}
		// The exit status from an intentional group kill is not a Close error.
		// waitError is still called by the wait branch above, and retaining it
		// here would make normal idempotent cleanup report signal: killed.
	})
	return process.closeErr
}

type tailcatStream struct {
	process *tailcatProcess
}

func (stream *tailcatStream) Read(data []byte) (int, error) {
	return stream.process.stdout.Read(data)
}

func (stream *tailcatStream) Write(data []byte) (int, error) {
	return stream.process.stdin.Write(data)
}

func (stream *tailcatStream) Close() error {
	return stream.process.close()
}

type tailcatServer struct {
	process     *tailcatProcess
	address     string
	addressPath string
	closed      chan struct{}

	acceptOnce sync.Once
	acceptErr  error
	stream     io.ReadWriteCloser

	closeOnce sync.Once
	closeErr  error
}

func (server *tailcatServer) Addr() string { return server.address }

func (server *tailcatServer) Accept(ctx context.Context) (io.ReadWriteCloser, error) {
	firstCall := false
	server.acceptOnce.Do(func() {
		firstCall = true
		ctx = nonNilContext(ctx)
		select {
		case <-server.closed:
			server.acceptErr = errTailcatClosed
		case <-ctx.Done():
			server.acceptErr = ctx.Err()
		case <-server.process.done:
			server.acceptErr = fmt.Errorf("tailcat exited before accepting peer: %w", server.process.waitError())
		default:
			server.stream = &tailcatStream{process: server.process}
		}
	})
	if !firstCall {
		return nil, errTailcatAlreadyAccept
	}
	if server.stream == nil {
		if server.acceptErr == nil {
			server.acceptErr = errTailcatAlreadyAccept
		}
		return nil, server.acceptErr
	}
	return server.stream, nil
}

func (server *tailcatServer) Close() error {
	server.closeOnce.Do(func() {
		close(server.closed)
		server.closeErr = server.process.close()
		_ = os.Remove(server.addressPath)
	})
	return server.closeErr
}

func waitForTailcatAddress(ctx context.Context, process *tailcatProcess, path string, timeout time.Duration) (string, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(AddrFilePollInterval)
	defer ticker.Stop()

	for {
		if address, ok := readTailcatAddress(path); ok {
			return address, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-process.done:
			if err := process.waitError(); err != nil {
				return "", fmt.Errorf("tailcat exited before writing address: %w", err)
			}
			return "", errors.New("tailcat exited before writing address")
		case <-deadline.C:
			// A write racing the timer should still be accepted.
			if address, ok := readTailcatAddress(path); ok {
				return address, nil
			}
			return "", fmt.Errorf("tailcat address file %s was not populated within %s", path, timeout)
		case <-ticker.C:
		}
	}
}

func readTailcatAddress(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	address := strings.TrimSpace(string(data))
	return address, address != ""
}
