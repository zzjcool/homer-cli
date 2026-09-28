package e2e

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestAgentHubTopology verifies the P2/P3 flow end to end on one host with
// three homer processes: a hub (serve), a listen-mode agent A, and a
// connect-mode agent B. It walks plan §5 steps 2-4: both agents register,
// remote status collection works over both transports, and a remote push
// round-trips config from A through the git origin to B.
func TestAgentHubTopology(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer W11 E2E\n\temail = homer-w11@example.invalid\n")
	origin := filepath.Join(root, "origin.git")
	runGit(t, global, root, "init", "--bare", "-b", "main", origin)

	binary := binaryOf(t)
	machineA := makeMachine(t, root, "A", global, true)
	machineB := makeMachine(t, root, "B", global, false)
	runGit(t, global, root, "clone", origin, machineA.homerHome)
	if result := runHomer(t, binary, machineA, "init", "--json"); result.code != 0 {
		t.Fatalf("init A failed: %s%s", result.stdout, result.stderr)
	}
	if result := runHomer(t, binary, machineA, "push", "--yes"); result.code != 0 {
		t.Fatalf("push A failed: %s%s", result.stdout, result.stderr)
	}
	// B joins through the standard first-contact flow, which clones the
	// origin and applies the config without a pre-existing init.
	if result := runHomer(t, binary, machineB, "home", origin, "--yes"); result.code != 0 {
		t.Fatalf("home B failed: %s%s", result.stdout, result.stderr)
	}

	token := "e2e-hub-token"

	// Hub on a fixed loopback port (agents need a stable address).
	hubPort := freePort(t)
	hubProc := startHomer(t, binary, machineA, global,
		"serve", "--addr", fmt.Sprintf("127.0.0.1:%d", hubPort), "--token", token, "--home", machineA.homerHome)
	defer stopProc(t, hubProc)

	// Agent A: listen mode, reachable at its own port.
	listenPort := freePort(t)
	agentA := startHomer(t, binary, machineA, global,
		"agent", "--listen", fmt.Sprintf("127.0.0.1:%d", listenPort),
		"--advertise", fmt.Sprintf("http://127.0.0.1:%d", listenPort),
		"--hub", fmt.Sprintf("http://127.0.0.1:%d", hubPort),
		"--token", token, "--id", "agent-a", "--home", machineA.homerHome)
	defer stopProc(t, agentA)

	// Agent B: connect mode (dials out; the NAT shape).
	agentB := startHomer(t, binary, machineB, global,
		"agent", "--connect", fmt.Sprintf("http://127.0.0.1:%d", hubPort),
		"--token", token, "--id", "agent-b", "--home", machineB.homerHome)
	defer stopProc(t, agentB)

	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)
	auth := map[string]string{"Authorization": "Bearer " + token}

	// Both agents must appear in the registry (listen registered explicitly,
	// connect registered on startup).
	deadline := time.Now().Add(30 * time.Second)
	ids := map[string]bool{}
	for time.Now().Before(deadline) && !(ids["agent-a"] && ids["agent-b"]) {
		body := apiGet(t, hubURL+"/api/agents", auth, hubProc)
		ids = map[string]bool{}
		for _, item := range jsonPath(t, body, "agents").([]any) {
			ids[item.(map[string]any)["agentId"].(string)] = true
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ids["agent-a"] || !ids["agent-b"] {
		t.Fatalf("registered agents = %v; hub output: %s", ids, procOutput[hubProc])
	}

	// Remote status over the listen (direct) transport.
	body := apiPost(t, hubURL+"/api/agents/agent-a/status", auth)
	if report, ok := jsonPath(t, body, "report").(map[string]any); !ok || len(report["adapters"].([]any)) == 0 {
		t.Fatalf("agent-a status report = %v", body)
	}

	// Remote status over the connect (queued) transport.
	body = apiPost(t, hubURL+"/api/agents/agent-b/status", auth)
	if report, ok := jsonPath(t, body, "report").(map[string]any); !ok || len(report["adapters"].([]any)) == 0 {
		t.Fatalf("agent-b status report = %v", body)
	}

	// Drift on A, remote push through the hub, then remote pull on B.
	writeFile(t, filepath.Join(piRoot(machineA), "settings.json"), "{\n  \"agentHub\": true\n}\n")
	body = apiPost(t, hubURL+"/api/agents/agent-a/push?confirm=true", auth)
	if jsonPath(t, body, "ok") != true {
		t.Fatalf("agent-a remote push = %s", body)
	}
	body = apiPost(t, hubURL+"/api/agents/agent-b/pull?confirm=true", auth)
	if jsonPath(t, body, "ok") != true {
		t.Fatalf("agent-b remote pull = %s", body)
	}

	// The config actually flowed: B's tool dir now carries A's change.
	pulled, err := os.ReadFile(filepath.Join(piRoot(machineB), "settings.json"))
	if err != nil || !strings.Contains(string(pulled), "agentHub") {
		t.Fatalf("agent-b settings.json = %q err=%v", pulled, err)
	}
}

var procOutput = map[*exec.Cmd]string{}

func startHomer(t *testing.T, binary string, m machine, global string, args ...string) *exec.Cmd {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Env = []string{
		"HOME=" + m.fakeHome,
		"HOMER_HOME=" + m.homerHome,
		"GIT_CONFIG_GLOBAL=" + global,
		"GIT_CONFIG_NOSYSTEM=1",
		"PATH=" + os.Getenv("PATH"),
	}
	var combined strings.Builder
	command.Stdout = &combined
	command.Stderr = &combined
	command.Env = append(command.Env, "HOMER_E2E_LOG=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	// Expose the process output for failure diagnostics.
	t.Cleanup(func() { procOutput[command] = combined.String() })
	// Wait briefly so a fast startup failure surfaces in the output.
	time.Sleep(300 * time.Millisecond)
	if command.ProcessState != nil {
		t.Logf("process exited early: %s", combined.String())
	}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	return command
}

func stopProc(t *testing.T, command *exec.Cmd) {
	t.Helper()
	if command == nil || command.Process == nil {
		return
	}
	_ = command.Process.Signal(syscall.SIGINT)
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func apiGet(t *testing.T, url string, headers map[string]string, proc *exec.Cmd) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v; process output: %s", url, err, procOutput[proc])
	}
	defer response.Body.Close()
	buf := make([]byte, 1<<16)
	n, _ := response.Body.Read(buf)
	return string(buf[:n])
}

func apiPost(t *testing.T, url string, headers map[string]string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	buf := make([]byte, 1<<20)
	n, _ := response.Body.Read(buf)
	return string(buf[:n])
}

func jsonPath(t *testing.T, body string, path ...string) any {
	t.Helper()
	var current any
	if err := json.Unmarshal([]byte(body), &current); err != nil {
		t.Fatalf("invalid JSON %q: %v", body, err)
	}
	for _, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %T is not an object in %s", path, current, body)
		}
		current, ok = object[segment]
		if !ok {
			t.Fatalf("path %v: missing key %q in %s", path, segment, body)
		}
	}
	return current
}
