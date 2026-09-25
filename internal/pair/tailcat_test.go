package pair

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeFakeTailcat(t *testing.T) (binary, argsFile, childPIDFile, envFile string) {
	t.Helper()
	dir := t.TempDir()
	binary = filepath.Join(dir, "tailcat")
	argsFile = filepath.Join(dir, "argv")
	childPIDFile = filepath.Join(dir, "child.pid")
	envFile = filepath.Join(dir, "env")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$TAILCAT_ARGS_FILE"
if [ "${TAILCAT_MODE-}" = "serve" ]; then
  printf 'tc-test-address\n' > "$TAILCAT_ADDR_FILE"
fi
if [ -n "${TAILCAT_ENV_FILE-}" ]; then
  /usr/bin/env > "$TAILCAT_ENV_FILE"
fi
if [ -n "${TAILCAT_CHILD_PID_FILE-}" ]; then
  /bin/sleep 30 &
  printf '%s\n' "$!" > "$TAILCAT_CHILD_PID_FILE"
fi
if [ "${TAILCAT_MODE-}" = "no-address" ]; then
  /bin/sleep 30
else
  /bin/cat
fi
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TERM", "xterm-test")
	t.Setenv("TAILCAT_SHOULD_NOT_LEAK", "secret-value")
	return binary, argsFile, childPIDFile, envFile
}

func waitForFile(t *testing.T, filename string, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filename)
		if err == nil && len(data) > 0 {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", filename)
	return nil
}

func TestTailcatServePollsAddressAndCleansProcessGroup(t *testing.T) {
	_, argsFile, childPIDFile, envFile := writeFakeTailcat(t)
	transport, err := NewTailcatTransport(TailcatOptions{
		ExtraEnv: []string{
			"TAILCAT_MODE=serve",
			"TAILCAT_ARGS_FILE=" + argsFile,
			"TAILCAT_CHILD_PID_FILE=" + childPIDFile,
			"TAILCAT_ENV_FILE=" + envFile,
		},
	})
	if err != nil {
		t.Fatalf("NewTailcatTransport: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server, err := transport.Serve(ctx)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	impl, ok := server.(*tailcatServer)
	if !ok {
		t.Fatalf("Serve type = %T, want *tailcatServer", server)
	}
	if got, want := server.Addr(), "tc-test-address"; got != want {
		t.Fatalf("Addr = %q, want %q", got, want)
	}
	info, err := os.Stat(impl.addressPath)
	if err != nil {
		t.Fatalf("stat address file: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("address permissions = %o, want %o", got, want)
	}
	if got := strings.Fields(string(waitForFile(t, argsFile, time.Second))); len(got) != 1 || got[0] != "--key=new" {
		t.Fatalf("serve argv = %#v, want --key=new", got)
	}
	env := string(waitForFile(t, envFile, time.Second))
	if !strings.Contains(env, "PATH=") || !strings.Contains(env, "HOME=") || !strings.Contains(env, "TERM=xterm-test\n") {
		t.Fatalf("minimal tailcat environment = %q", env)
	}
	if strings.Contains(env, "TAILCAT_SHOULD_NOT_LEAK") {
		t.Fatalf("ambient secret leaked into tailcat environment: %q", env)
	}
	if !strings.Contains(env, "TAILCAT_ADDR_FILE="+impl.addressPath) {
		t.Fatalf("address file missing from environment: %q", env)
	}

	stream, err := server.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := server.Accept(ctx); err == nil {
		t.Fatal("second Accept unexpectedly succeeded")
	}
	message := []byte("pair-stream-round-trip\n")
	if _, err := stream.Write(message); err != nil {
		t.Fatalf("stream Write: %v", err)
	}
	echo := make([]byte, len(message))
	if _, err := io.ReadFull(stream, echo); err != nil {
		t.Fatalf("stream Read: %v", err)
	}
	if string(echo) != string(message) {
		t.Fatalf("echo = %q, want %q", echo, message)
	}

	childPID, err := strconv.Atoi(strings.TrimSpace(string(waitForFile(t, childPIDFile, time.Second))))
	if err != nil {
		t.Fatalf("parse child pid: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream Close second call: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("server Close: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("server Close second call: %v", err)
	}
	if _, err := os.Stat(impl.addressPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("address file after Close: err=%v", err)
	}
	waitForProcessGone(t, childPID)
}

func TestTailcatServeAddressTimeoutKillsChild(t *testing.T) {
	_, argsFile, childPIDFile, _ := writeFakeTailcat(t)
	transport, err := NewTailcatTransport(TailcatOptions{
		AddrFileTimeout: 150 * time.Millisecond,
		ExtraEnv: []string{
			"TAILCAT_MODE=no-address",
			"TAILCAT_ARGS_FILE=" + argsFile,
			"TAILCAT_CHILD_PID_FILE=" + childPIDFile,
		},
	})
	if err != nil {
		t.Fatalf("NewTailcatTransport: %v", err)
	}
	started := time.Now()
	server, err := transport.Serve(context.Background())
	if server != nil {
		t.Fatal("timeout Serve returned a server")
	}
	if err == nil || !strings.Contains(err.Error(), "not populated") {
		t.Fatalf("timeout Serve error = %v, want address timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timeout cleanup took %s", elapsed)
	}
	childPID, parseErr := strconv.Atoi(strings.TrimSpace(string(waitForFile(t, childPIDFile, time.Second))))
	if parseErr != nil {
		t.Fatalf("parse timeout child pid: %v", parseErr)
	}
	waitForProcessGone(t, childPID)
}

func TestTailcatConnectUsesOptionTerminatorForAddress(t *testing.T) {
	_, argsFile, childPIDFile, envFile := writeFakeTailcat(t)
	transport, err := NewTailcatTransport(TailcatOptions{
		ExtraEnv: []string{
			"TAILCAT_MODE=join",
			"TAILCAT_ARGS_FILE=" + argsFile,
			"TAILCAT_CHILD_PID_FILE=" + childPIDFile,
			"TAILCAT_ENV_FILE=" + envFile,
		},
	})
	if err != nil {
		t.Fatalf("NewTailcatTransport: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := transport.Connect(ctx, "-address-looking-like-a-flag")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	args := strings.Fields(string(waitForFile(t, argsFile, time.Second)))
	if len(args) != 2 || args[0] != "--" || args[1] != "-address-looking-like-a-flag" {
		t.Fatalf("join argv = %#v, want -- <addr>", args)
	}
	if strings.Contains(string(waitForFile(t, envFile, time.Second)), "TAILCAT_ADDR_FILE=") {
		t.Fatal("join unexpectedly received TAILCAT_ADDR_FILE")
	}
	// Cancelling the dial timeout after Connect returns must not tear down the
	// established stream; its Close owns the child process lifetime.
	cancel()
	if _, err := stream.Write([]byte("join-echo")); err != nil {
		t.Fatalf("join Write: %v", err)
	}
	echo := make([]byte, len("join-echo"))
	if _, err := io.ReadFull(stream, echo); err != nil {
		t.Fatalf("join Read: %v", err)
	}
	if string(echo) != "join-echo" {
		t.Fatalf("join echo = %q", echo)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("join Close: %v", err)
	}
}

func TestTailcatServeContextCancellationAndCloseAreIdempotent(t *testing.T) {
	_, argsFile, childPIDFile, _ := writeFakeTailcat(t)
	transport, err := NewTailcatTransport(TailcatOptions{
		ExtraEnv: []string{
			"TAILCAT_MODE=serve",
			"TAILCAT_ARGS_FILE=" + argsFile,
			"TAILCAT_CHILD_PID_FILE=" + childPIDFile,
		},
	})
	if err != nil {
		t.Fatalf("NewTailcatTransport: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server, err := transport.Serve(ctx)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	impl := server.(*tailcatServer)
	stream, err := server.Accept(context.Background())
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(waitForFile(t, childPIDFile, time.Second))))
	if err != nil {
		t.Fatalf("parse cancellation child pid: %v", err)
	}
	cancel()
	waitForProcessGone(t, childPID)
	if err := stream.Close(); err != nil {
		t.Fatalf("stream Close after context cancellation: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream Close second call after context cancellation: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("server Close after context cancellation: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("server Close second call after context cancellation: %v", err)
	}
	if _, err := os.Stat(impl.addressPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("address file after cancellation cleanup: %v", err)
	}
}

func TestTailcatServeAddressTimeoutHonorsContext(t *testing.T) {
	_, argsFile, _, _ := writeFakeTailcat(t)
	transport, err := NewTailcatTransport(TailcatOptions{
		AddrFileTimeout: 5 * time.Second,
		ExtraEnv: []string{
			"TAILCAT_MODE=no-address",
			"TAILCAT_ARGS_FILE=" + argsFile,
		},
	})
	if err != nil {
		t.Fatalf("NewTailcatTransport: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = transport.Serve(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context Serve error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("context cleanup took %s", elapsed)
	}
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d is still alive", pid)
}
