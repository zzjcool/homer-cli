package e2e

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestServeSmoke boots `homer serve` as a subprocess on an ephemeral
// loopback port with a prepared fixture machine, then exercises the P1 API
// surface (plan §5) and shuts it down with SIGINT.
func TestServeSmoke(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer W11 E2E\n\temail = homer-w11@example.invalid\n")
	origin := filepath.Join(root, "origin.git")
	runGit(t, global, root, "init", "--bare", "-b", "main", origin)

	binary := binaryOf(t)
	machineA := makeMachine(t, root, "A", global, true)
	runGit(t, global, root, "clone", origin, machineA.homerHome)
	init := mustJSONHomer(t, binary, machineA, "init", "--json")
	if len(init["adapters"].([]any)) == 0 {
		t.Fatal("init produced no adapters")
	}
	if homer(t, binary, machineA, "push", "--yes") != 0 {
		t.Fatal("baseline push failed")
	}

	// Drift: change a tracked file after the baseline push so the confirm
	// gate matters (no-drift pushes are legitimately 200 without confirm).
	writeFile(t, filepath.Join(piRoot(machineA), "settings.json"), "{\n  \"served\": true\n}\n")

	serve := exec.Command(binary, "serve", "--home", machineA.homerHome, "--addr", "127.0.0.1:0")
	serve.Env = serveEnv(machineA, global)
	var serveOut strings.Builder
	serve.Stdout = &serveOut
	serve.Stderr = &serveOut
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = serve.Process.Kill()
		_, _ = serve.Process.Wait()
	}()

	addr := waitForServeAddr(t, &serveOut)
	health := waitHTTP(t, "http://"+addr+"/api/health", http.StatusOK, 10*time.Second)
	if !strings.Contains(health, `"ok":true`) {
		t.Fatalf("health body = %q", health)
	}
	status := waitHTTP(t, "http://"+addr+"/api/status", http.StatusOK, 10*time.Second)
	if !strings.Contains(status, `"adapters"`) {
		t.Fatalf("status body = %q", status)
	}
	config := waitHTTP(t, "http://"+addr+"/api/config", http.StatusOK, 10*time.Second)
	if !strings.Contains(config, `"adapters"`) {
		t.Fatalf("config body = %q", config)
	}
	diff := waitHTTP(t, "http://"+addr+"/api/diff", http.StatusOK, 10*time.Second)
	if !strings.Contains(diff, `"text"`) {
		t.Fatalf("diff body = %q", diff)
	}

	// Unconfirmed push must be a 409 (aborted, non-TTY confirm fallback).
	if code := postStatus(t, "http://"+addr+"/api/push"); code != http.StatusConflict {
		t.Fatalf("push without confirm = %d, want 409", code)
	}
	// Confirmed push goes through and commits.
	if code := postStatus(t, "http://"+addr+"/api/push?confirm=true"); code != http.StatusOK {
		t.Fatalf("confirmed push = %d, want 200", code)
	}

	index := waitHTTP(t, "http://"+addr+"/", http.StatusOK, 10*time.Second)
	for _, marker := range []string{"status-cards", "diff-view", "actions", "agents", "not-initialized"} {
		if !strings.Contains(index, marker) {
			t.Fatalf("index missing UI block %q", marker)
		}
	}

	if err := serve.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	select {
	case err := <-waitExit(serve):
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				t.Fatalf("serve wait: %v", err)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit on SIGINT")
	}
	if exitCode != 0 {
		t.Fatalf("serve exited %d on SIGINT, want 0 (graceful)", exitCode)
	}
}

// waitExit adapts cmd.Wait into a channel so shutdown assertions can select
// with a deadline instead of blocking.
func waitExit(command *exec.Cmd) <-chan error {
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	return done
}

// TestServeNonLoopbackWithoutToken verifies the CLI exits 1 with the Chinese
// error when serving a non-loopback address without a token (plan §2.1).
func TestServeNonLoopbackWithoutToken(t *testing.T) {
	binary := binaryOf(t)
	result := runProcess(t, t.TempDir(), filepath.Join(t.TempDir(), "gitconfig"), nil, binary, "serve", "--addr", "0.0.0.0:7760", "--home", t.TempDir())
	if result.code != 1 {
		t.Fatalf("exit = %d, want 1; out=%q err=%q", result.code, result.stdout, result.stderr)
	}
	if !strings.Contains(result.stderr, "token") && !strings.Contains(result.stderr, "回环") {
		t.Fatalf("missing token/loopback error: %q", result.stderr)
	}
}

func binaryOf(t *testing.T) string {
	t.Helper()
	return buildHomer(t)
}

func homer(t *testing.T, binary string, m machine, args ...string) int {
	t.Helper()
	return runHomer(t, binary, m, args...).code
}

func serveEnv(m machine, global string) []string {
	return []string{
		"HOME=" + m.fakeHome,
		"HOMER_HOME=" + m.homerHome,
		"GIT_CONFIG_GLOBAL=" + global,
		"GIT_CONFIG_NOSYSTEM=1",
		"PATH=" + os.Getenv("PATH"),
	}
}

func waitForServeAddr(t *testing.T, output *strings.Builder) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if addr, ok := firstServeURL(output.String()); ok {
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("serve did not print its URL: %q", output.String())
	return ""
}

func firstServeURL(output string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		if at := strings.Index(line, "http://127.0.0.1:"); at >= 0 {
			rest := line[at+len("http://"):]
			if end := strings.IndexAny(rest, "\uff08( "); end > 0 {
				return rest[:end], true
			}
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

func postStatus(t *testing.T, url string) int {
	t.Helper()
	response, err := http.Post(url, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func waitHTTP(t *testing.T, url string, want int, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		response, err := http.Get(url)
		if err == nil {
			body := make([]byte, 8192)
			n, _ := response.Body.Read(body)
			_ = response.Body.Close()
			last = string(body[:n])
			if response.StatusCode == want {
				return last
			}
		} else {
			last = err.Error()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("GET %s did not reach %d within %s: %q", url, want, timeout, last)
	return ""
}

var _ = os.Getenv

// TestServeTokenAuthSmoke boots serve with HOMER_HUB_TOKEN and walks the
// 401 → authorized flow end to end over the real HTTP surface.
func TestServeTokenAuthSmoke(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer W11 E2E\n\temail = homer-w11@example.invalid\n")
	binary := binaryOf(t)
	machine := makeMachine(t, root, "A", global, true)
	if result := runHomer(t, binary, machine, "init", "--json"); result.code != 0 {
		t.Fatalf("init failed: %s%s", result.stdout, result.stderr)
	}

	serve := exec.Command(binary, "serve", "--home", machine.homerHome, "--addr", "127.0.0.1:0")
	serve.Env = append(serveEnv(machine, global), "HOMER_HUB_TOKEN=e2e-token")
	var serveOut strings.Builder
	serve.Stdout = &serveOut
	serve.Stderr = &serveOut
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = serve.Process.Kill()
		_, _ = serve.Process.Wait()
	}()

	addr := waitForServeAddr(t, &serveOut)
	if code := getHTTPStatus(t, "http://"+addr+"/api/status"); code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d, want 401", code)
	}
	if code := getHTTPStatus(t, "http://"+addr+"/api/health"); code != http.StatusOK {
		t.Fatalf("health without token = %d, want 200 (exempt)", code)
	}
	authorized := waitAuthorized(t, addr, "e2e-token")
	if !strings.Contains(authorized, `"adapters"`) {
		t.Fatalf("authorized status body = %q", authorized)
	}
}

func getHTTPStatus(t *testing.T, url string) int {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func waitAuthorized(t *testing.T, addr, token string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/api/status", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			body := make([]byte, 8192)
			n, _ := response.Body.Read(body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return string(body[:n])
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("authorized status never reached 200")
	return ""
}
