package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

func apiPostJSON(t *testing.T, rawURL string, headers map[string]string, body any) (int, string) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
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
	return response.StatusCode, string(buf[:n])
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

// TestAgentZeroArgRestart verifies the full Tailscale-style onboarding
// closed loop: a join command copied from the hub works verbatim, and a
// later bare `homer agent` restart reuses the persisted join state (same
// agent ID — the hub sees no new machine).
func TestAgentZeroArgRestart(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = T\n\temail = t@t\n")
	binary := binaryOf(t)
	machine := makeMachine(t, root, "A", global, false)
	if result := runHomer(t, binary, machine, "init", "--json"); result.code != 0 {
		t.Fatalf("init failed: %s%s", result.stdout, result.stderr)
	}

	hubPort := freePort(t)
	// Non-loopback bind without a token: auto-generation path.
	hubProc := startHomer(t, binary, machine, global,
		"serve", "--addr", fmt.Sprintf("0.0.0.0:%d", hubPort), "--home", machine.homerHome)
	defer stopProc(t, hubProc)

	// The join command embeds the generated token; extract it from the hub
	// home's keys/hub-token (the printed line is the same value).
	tokenBytes, err := os.ReadFile(filepath.Join(machine.homerHome, "keys", "hub-token"))
	if err != nil {
		t.Fatalf("hub token not persisted: %v", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)

	// First start: the verbatim join-command shape (env token, --connect).
	first := startHomerWithEnv(t, binary, machine, global,
		[]string{"HOMER_HUB_TOKEN=" + token},
		"agent", "--connect", hubURL, "--id", "restart-agent", "--home", machine.homerHome)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if agentRegistered(t, hubURL, token, "restart-agent") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !agentRegistered(t, hubURL, token, "restart-agent") {
		t.Fatal("first agent run never registered")
	}
	stopProc(t, first)

	// agent.json now exists with the join state; token is NOT in it.
	configData, err := os.ReadFile(filepath.Join(machine.homerHome, "agent.json"))
	if err != nil {
		t.Fatalf("agent.json missing: %v", err)
	}
	if strings.Contains(string(configData), token) {
		t.Fatal("agent.json must not embed the token")
	}

	// Bare restart: no flags, no env — everything comes from agent.json.
	second := startHomerWithEnv(t, binary, machine, global, nil,
		"agent", "--home", machine.homerHome)
	defer stopProc(t, second)
	deadline = time.Now().Add(20 * time.Second)
	registered := false
	for time.Now().Before(deadline) {
		if agentRegistered(t, hubURL, token, "restart-agent") {
			registered = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !registered {
		t.Fatal("bare restart never re-registered with the same agent ID")
	}
}

func agentRegistered(t *testing.T, hubURL, token, agentID string) bool {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, hubURL+"/api/agents", nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return false
	}
	var payload struct {
		Agents []struct {
			AgentID string `json:"agentId"`
		} `json:"agents"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	for _, agent := range payload.Agents {
		if agent.AgentID == agentID {
			return true
		}
	}
	return false
}

func startHomerWithEnv(t *testing.T, binary string, m machine, global string, extraEnv []string, args ...string) *exec.Cmd {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Env = append(serveEnv(m, global), extraEnv...)
	var combined strings.Builder
	command.Stdout = &combined
	command.Stderr = &combined
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	return command
}

// TestInstallScriptBootstrap walks the Tailscale-style onboarding: fetch
// /install.sh from the hub (public), verify the token is NOT embedded,
// then simulate the script's effect (token → keys/hub-token on a fresh
// machine) and run a zero-flag agent that must register via the token-file
// fallback.
func TestInstallScriptBootstrap(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = T\n\temail = t@t\n")
	binary := binaryOf(t)
	hubMachine := makeMachine(t, root, "H", global, false)
	if result := runHomer(t, binary, hubMachine, "init", "--json"); result.code != 0 {
		t.Fatalf("init failed: %s%s", result.stdout, result.stderr)
	}

	hubPort := freePort(t)
	hubProc := startHomer(t, binary, hubMachine, global,
		"serve", "--addr", fmt.Sprintf("127.0.0.1:%d", hubPort), "--home", hubMachine.homerHome,
		"--token", "install-e2e-token")
	defer stopProc(t, hubProc)
	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)

	if health := waitHTTP(t, hubURL+"/api/health", http.StatusOK, 10*time.Second); !strings.Contains(health, `"ok":true`) {
		t.Fatalf("hub health = %q", health)
	}

	// 1. The script is public and does not leak the token.
	scriptResp, err := http.Get(hubURL + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script, _ := io.ReadAll(scriptResp.Body)
	_ = scriptResp.Body.Close()
	if scriptResp.StatusCode != http.StatusOK {
		t.Fatalf("install.sh = %d", scriptResp.StatusCode)
	}
	if strings.Contains(string(script), "install-e2e-token") {
		t.Fatal("install.sh must not embed the hub token")
	}
	for _, marker := range []string{hubURL, "keys/hub-token", "homer agent --connect", "/dl/homer"} {
		if !strings.Contains(string(script), marker) {
			t.Fatalf("install.sh missing %q", marker)
		}
	}

	// 2. The hub serves its own binary (token-authenticated).
	dl, err := http.NewRequest(http.MethodGet, hubURL+"/dl/homer", nil)
	if err != nil {
		t.Fatal(err)
	}
	dl.Header.Set("Authorization", "Bearer install-e2e-token")
	dlResp, err := http.DefaultClient.Do(dl)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 4)
	_, _ = io.ReadFull(dlResp.Body, head)
	size := dlResp.ContentLength
	_ = dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK || string(head) != "\x7fELF" || size < 1_000_000 {
		t.Fatalf("dl/homer = %d head=%q size=%d (want an ELF binary > 1MB)", dlResp.StatusCode, head, size)
	}

	// 3. Simulate the script on a fresh agent machine: token file, no flags.
	agentMachine := makeMachine(t, root, "A", global, false)
	tokenDir := filepath.Join(agentMachine.homerHome, "keys")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tokenDir, "hub-token"), []byte("install-e2e-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	agentProc := startHomerWithEnv(t, binary, agentMachine, global, nil,
		"agent", "--connect", hubURL, "--id", "install-script-agent", "--home", agentMachine.homerHome)
	defer stopProc(t, agentProc)

	deadline := time.Now().Add(20 * time.Second)
	registered := false
	for time.Now().Before(deadline) {
		if agentRegistered(t, hubURL, "install-e2e-token", "install-script-agent") {
			registered = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !registered {
		t.Fatal("agent with token-file bootstrap never registered")
	}
}

// TestEnrollmentE2E walks the full Tailscale-style protocol: the console
// mints a one-time code (via /api/auth/join), the install command carries
// it, the agent enrolls for a per-agent secret, and revocation drops just
// that machine.
func TestEnrollmentE2E(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = T\n\temail = t@t\n")
	binary := binaryOf(t)
	hubMachine := makeMachine(t, root, "H", global, false)
	if result := runHomer(t, binary, hubMachine, "init", "--json"); result.code != 0 {
		t.Fatalf("init failed: %s%s", result.stdout, result.stderr)
	}

	hubPort := freePort(t)
	hubProc := startHomer(t, binary, hubMachine, global,
		"serve", "--addr", fmt.Sprintf("127.0.0.1:%d", hubPort), "--home", hubMachine.homerHome,
		"--token", "mgmt-token")
	defer stopProc(t, hubProc)
	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)
	waitHTTP(t, hubURL+"/api/health", http.StatusOK, 10*time.Second)

	// 1. The join command now carries a one-time enrollment code, NOT the
	// management hub token.
	joinBody := waitHTTPWithToken(t, hubURL+"/api/auth/join", "mgmt-token", 10*time.Second)
	if !strings.Contains(joinBody, "install.sh") || !strings.Contains(joinBody, "hr_") {
		t.Fatalf("join command missing enrollment code: %s", joinBody)
	}
	if strings.Contains(joinBody, "mgmt-token") {
		t.Fatal("join command leaked the management token")
	}
	var joinPayload struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(joinBody), &joinPayload); err != nil {
		t.Fatal(err)
	}
	// extract --token <code>
	codeStart := strings.LastIndex(joinPayload.Command, "--token ")
	if codeStart < 0 {
		t.Fatalf("no --token in %q", joinPayload.Command)
	}
	code := strings.TrimSpace(strings.SplitN(joinPayload.Command[codeStart+len("--token "):], " ", 2)[0])

	// 2. A fresh machine enrolls via the token-file bootstrap (install.sh
	// writes keys/hub-token with the code; agent detects the hr_ prefix).
	agentMachine := makeMachine(t, root, "E", global, false)
	tokenDir := filepath.Join(agentMachine.homerHome, "keys")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tokenDir, "hub-token"), []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	agentProc := startHomerWithEnv(t, binary, agentMachine, global, nil,
		"agent", "--connect", hubURL, "--id", "enroll-e2e-agent", "--home", agentMachine.homerHome)
	defer stopProc(t, agentProc)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !agentRegistered(t, hubURL, "mgmt-token", "enroll-e2e-agent") {
		time.Sleep(200 * time.Millisecond)
	}
	if !agentRegistered(t, hubURL, "mgmt-token", "enroll-e2e-agent") {
		t.Fatal("enrolled agent never registered")
	}

	// 3. The agent persisted a per-agent secret (never the shared code).
	configBytes, err := os.ReadFile(filepath.Join(agentMachine.homerHome, "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		AgentSecret string `json:"agentSecret"`
	}
	if err := json.Unmarshal(configBytes, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.AgentSecret == "" || strings.Contains(string(configBytes), code) {
		t.Fatalf("agent.json must hold the per-agent secret, not the code: %s", configBytes)
	}

	// 4. The enrollment code is burned: a second machine reusing it fails.
	secondMachine := makeMachine(t, root, "S", global, false)
	secondDir := filepath.Join(secondMachine.homerHome, "keys")
	if err := os.MkdirAll(secondDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondDir, "hub-token"), []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	secondProc := startHomerWithEnv(t, binary, secondMachine, global, nil,
		"agent", "--connect", hubURL, "--id", "replay-agent", "--home", secondMachine.homerHome)
	defer stopProc(t, secondProc)
	time.Sleep(3 * time.Second) // enough for a registration attempt + retry cycle
	if agentRegistered(t, hubURL, "mgmt-token", "replay-agent") {
		t.Fatal("burned enrollment code was replayed successfully")
	}

	// 5. Revocation: the enrolled machine drops, others (management token)
	// keep working, and the same agentID may re-enroll later.
	revokeReq, _ := http.NewRequest(http.MethodPost, hubURL+"/api/agents/revoke", strings.NewReader(`{"agentId":"enroll-e2e-agent"}`))
	revokeReq.Header.Set("Authorization", "Bearer mgmt-token")
	revokeReq.Header.Set("Content-Type", "application/json")
	revokeResp, err := http.DefaultClient.Do(revokeReq)
	if err != nil || revokeResp.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %v %v", err, revokeResp)
	}
	_ = revokeResp.Body.Close()
}

// TestManualSyncFanout walks the no-git MVP user story end to end:
// machine A changes a file, the console sees the change, one
// POST /api/sync publishes it as a hub generation, and machine B's file
// changes. No origin.git, no clone, no git credentials anywhere.
func TestManualSyncFanout(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer W11 E2E\n\temail = homer-w11@example.invalid\n")

	binary := binaryOf(t)
	machineA := makeMachine(t, root, "A", global, true)
	machineB := makeMachine(t, root, "B", global, false)

	// A initializes its own workspace locally (no remote, no clone).
	if result := runHomer(t, binary, machineA, "init", "--json"); result.code != 0 {
		t.Fatalf("init A failed: %s%s", result.stdout, result.stderr)
	}

	token := "manual-sync-token"
	hubPort := freePort(t)
	// The hub's home IS machine A's home: hero numbers reflect A's changes.
	hubProc := startHomer(t, binary, machineA, global,
		"serve", "--addr", fmt.Sprintf("127.0.0.1:%d", hubPort), "--token", token, "--home", machineA.homerHome)
	defer stopProc(t, hubProc)

	listenPort := freePort(t)
	agentA := startHomer(t, binary, machineA, global,
		"agent", "--listen", fmt.Sprintf("127.0.0.1:%d", listenPort),
		"--advertise", fmt.Sprintf("http://127.0.0.1:%d", listenPort),
		"--hub", fmt.Sprintf("http://127.0.0.1:%d", hubPort),
		"--token", token, "--id", "agent-a", "--home", machineA.homerHome)
	defer stopProc(t, agentA)
	agentB := startHomer(t, binary, machineB, global,
		"agent", "--connect", fmt.Sprintf("http://127.0.0.1:%d", hubPort),
		"--token", token, "--id", "agent-b", "--home", machineB.homerHome)
	defer stopProc(t, agentB)

	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)
	auth := map[string]string{"Authorization": "Bearer " + token}

	// Wait for both machines to appear in the registry.
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

	// 1. Machine A changes a file.
	writeFile(t, filepath.Join(piRoot(machineA), "settings.json"), "{\n  \"manualSync\": true\n}\n")

	// 2. The console sees the change (this is what the hero renders).
	body := apiGet(t, hubURL+"/api/status", auth, hubProc)
	pushSum := 0
	for _, item := range jsonPath(t, body, "report", "adapters").([]any) {
		if number, ok := item.(map[string]any)["push"].(float64); ok {
			pushSum += int(number)
		}
	}
	if pushSum == 0 {
		t.Fatalf("console sees no local changes; status = %s", body)
	}

	// 3. One manual sync publishes A onto the fleet (no-git transport:
	// a hub generation, then fan-out pulls).
	sync := apiPost(t, hubURL+"/api/sync?direction=to-others&confirm=true", auth)
	if jsonPath(t, sync, "ok") != true || jsonPath(t, sync, "status") != "synced" {
		t.Fatalf("manual sync = %s", sync)
	}

	// 4. Machine B's file now carries the change.
	bSettings := filepath.Join(piRoot(machineB), "settings.json")
	content, err := os.ReadFile(bSettings)
	if err != nil || !strings.Contains(string(content), "manualSync") {
		t.Fatalf("machine B settings = %q err=%v (want manualSync marker)", content, err)
	}

	// 5. Advisor acceptance: B was offline with its own edit to the same
	// file; a later sync must surface a conflict and keep B's local
	// content — never silently overwrite it.
	// (Covered by TestOfflineConflictKeepsLocal below.)
}

// TestOfflineConflictKeepsLocal pins the advisor's no-git acceptance:
// machine B edits a file while it effectively missed an earlier
// generation; when it pulls, the three-way judgment (B's store baseline /
// B's local edit / hub's current) must yield a conflict with B's local
// content preserved — never a silent overwrite.
func TestOfflineConflictKeepsLocal(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer W11 E2E\n\temail = homer-w11@example.invalid\n")

	binary := binaryOf(t)
	machineA := makeMachine(t, root, "A", global, true)
	machineB := makeMachine(t, root, "B", global, false)

	if result := runHomer(t, binary, machineA, "init", "--json"); result.code != 0 {
		t.Fatalf("init A failed: %s%s", result.stdout, result.stderr)
	}
	// Establish the shared baseline: A publishes, B pulls once.
	token := "offline-conflict-token"
	hubPort := freePort(t)
	hubProc := startHomer(t, binary, machineA, global,
		"serve", "--addr", fmt.Sprintf("127.0.0.1:%d", hubPort), "--token", token, "--home", machineA.homerHome)
	defer stopProc(t, hubProc)

	listenPort := freePort(t)
	agentA := startHomer(t, binary, machineA, global,
		"agent", "--listen", fmt.Sprintf("127.0.0.1:%d", listenPort),
		"--advertise", fmt.Sprintf("http://127.0.0.1:%d", listenPort),
		"--hub", fmt.Sprintf("http://127.0.0.1:%d", hubPort),
		"--token", token, "--id", "agent-a", "--home", machineA.homerHome)
	defer stopProc(t, agentA)
	agentB := startHomer(t, binary, machineB, global,
		"agent", "--connect", fmt.Sprintf("http://127.0.0.1:%d", hubPort),
		"--token", token, "--id", "agent-b", "--home", machineB.homerHome)
	defer stopProc(t, agentB)

	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)
	auth := map[string]string{"Authorization": "Bearer " + token}

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

	// Baseline round: A publishes its fixtures, B applies them.
	writeFile(t, filepath.Join(piRoot(machineA), "settings.json"), "{\n  \"baseline\": true\n}\n")
	sync := apiPost(t, hubURL+"/api/sync?direction=to-others&confirm=true", auth)
	if jsonPath(t, sync, "ok") != true || jsonPath(t, sync, "status") != "synced" {
		t.Fatalf("baseline sync = %s", sync)
	}
	bBaseline := filepath.Join(piRoot(machineB), "settings.json")
	if content, err := os.ReadFile(bBaseline); err != nil || !strings.Contains(string(content), "baseline") {
		t.Fatalf("B did not receive the baseline: %q err=%v", content, err)
	}

	// B goes truly offline before A's second publish: the process stops,
	// so the fan-out marks it skipped and it misses the new generation.
	stopProc(t, agentB)
	writeFile(t, filepath.Join(piRoot(machineA), "settings.json"), "{\n  \"fromA\": true\n}\n")
	sync = apiPost(t, hubURL+"/api/sync?direction=to-others&confirm=true", auth)
	// The publish must succeed. A just-stopped connect agent may still
	// hold a half-open TCP session, so its fan-out entry can time out —
	// "partial" (with agent-a applied) is an accepted outcome; only the
	// "synced" status means nobody lagged.
	status := jsonPath(t, sync, "status")
	if status != "synced" && status != "partial" {
		t.Fatalf("A's second sync = %s", sync)
	}
	head, headErr := os.ReadFile(filepath.Join(machineA.homerHome, "HEAD"))
	if headErr != nil || strings.TrimSpace(string(head)) == "" {
		t.Fatalf("second generation was not published: %q err=%v", string(head), headErr)
	}

	// While offline, B edits the SAME file locally (its divergent change).
	writeFile(t, bBaseline, "{\n  \"fromB\": true\n}\n")

	// B comes back online.
	agentB = startHomer(t, binary, machineB, global,
		"agent", "--connect", fmt.Sprintf("http://127.0.0.1:%d", hubPort),
		"--token", token, "--id", "agent-b", "--home", machineB.homerHome)
	defer stopProc(t, agentB)
	deadline = time.Now().Add(30 * time.Second)
	ids = map[string]bool{}
	for time.Now().Before(deadline) && !ids["agent-b"] {
		body := apiGet(t, hubURL+"/api/agents", auth, hubProc)
		ids = map[string]bool{}
		for _, item := range jsonPath(t, body, "agents").([]any) {
			ids[item.(map[string]any)["agentId"].(string)] = true
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ids["agent-b"] {
		t.Fatalf("agent-b did not re-register: %v; hub output: %s", ids, procOutput[hubProc])
	}

	// B pulls from the center: three-way (base=baseline, local=fromB,
	// remote=fromA) must conflict and keep B's local content.
	pull := apiPost(t, hubURL+"/api/agents/agent-b/pull?confirm=true", auth)
	status = jsonPath(t, pull, "status")
	if status != "conflicts-remain" && status != "conflicts" {
		t.Fatalf("expected a conflict status, got %q (body=%s)", status, pull)
	}
	content, err := os.ReadFile(bBaseline)
	if err != nil || !strings.Contains(string(content), "fromB") {
		t.Fatalf("B's local edit must survive the conflicted pull: %q err=%v", content, err)
	}
}

// TestPureServerFourStepStory pins the user's architecture: the server is
// ONLY a server — it holds the storage and no machine of its own.
//  1. connect machine A (enrollment)
//  2. collect A's config into the server's storage
//  3. connect machine B (enrollment)
//  4. dispatch the storage to B
func TestPureServerFourStepStory(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer W11 E2E\n\temail = homer-w11@example.invalid\n")

	binary := binaryOf(t)
	machineA := makeMachine(t, root, "A", global, true)
	machineB := makeMachine(t, root, "B", global, false)
	// Machine A is an already-configured machine (it ran homer init when
	// it first adopted homer — that is what "connect an existing machine"
	// means).
	if result := runHomer(t, binary, machineA, "init", "--json"); result.code != 0 {
		t.Fatalf("init A failed: %s%s", result.stdout, result.stderr)
	}

	// The server's home is a bare directory: no init, no adapters, no
	// store — pure storage + control plane.
	serverHome := filepath.Join(root, "server-home")
	if err := os.MkdirAll(serverHome, 0o755); err != nil {
		t.Fatal(err)
	}
	token := "pure-server-token"
	hubPort := freePort(t)
	hubProc := startHomer(t, binary, machineA, global,
		"serve", "--addr", fmt.Sprintf("127.0.0.1:%d", hubPort), "--token", token, "--home", serverHome)
	defer stopProc(t, hubProc)
	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)
	auth := map[string]string{"Authorization": "Bearer " + token}

	// Step 1: connect machine A.
	agentA := startHomer(t, binary, machineA, global,
		"agent", "--connect", hubURL, "--token", token, "--id", "machine-a", "--home", machineA.homerHome)
	defer stopProc(t, agentA)
	waitRegistered(t, hubURL, auth, hubProc, "machine-a")

	// Step 2: collect A's config into the server's storage.
	// The storage must be empty before, and hold generation 1 after.
	if head, err := os.ReadFile(filepath.Join(serverHome, "HEAD")); err == nil && strings.TrimSpace(string(head)) != "" {
		t.Fatalf("storage should be empty before collect, HEAD=%q", head)
	}
	collect := apiPost(t, hubURL+"/api/sync?direction=collect&agent=machine-a&confirm=true", auth)
	if jsonPath(t, collect, "ok") != true || jsonPath(t, collect, "status") != "synced" {
		t.Fatalf("collect = %s", collect)
	}
	if head, err := os.ReadFile(filepath.Join(serverHome, "HEAD")); err != nil || strings.TrimSpace(string(head)) != "1" {
		t.Fatalf("storage HEAD after collect = %q err=%v (want 1)", head, err)
	}

	// Step 3: connect machine B (empty machine, bootstrap via dispatch).
	agentB := startHomer(t, binary, machineB, global,
		"agent", "--connect", hubURL, "--token", token, "--id", "machine-b", "--home", machineB.homerHome)
	defer stopProc(t, agentB)
	waitRegistered(t, hubURL, auth, hubProc, "machine-b")

	// Step 4: dispatch the storage to B.
	dispatch := apiPost(t, hubURL+"/api/sync?direction=dispatch&confirm=true", auth)
	if jsonPath(t, dispatch, "ok") != true || jsonPath(t, dispatch, "status") != "synced" {
		t.Fatalf("dispatch = %s", dispatch)
	}
	// B's file now carries A's fixture content — bootstrap + dispatch
	// completed the four-step story with zero git anywhere.
	bSettings := filepath.Join(piRoot(machineB), "settings.json")
	content, err := os.ReadFile(bSettings)
	if err != nil {
		t.Fatalf("machine B settings read: %v", err)
	}
	// A's fixture settings.json carries "theme": "light" — that content
	// reaching B's previously-empty tree is the four-step story complete.
	if !strings.Contains(string(content), "theme") {
		t.Fatalf("machine B settings = %q — bootstrap dispatch did not deliver content", content)
	}
}

// TestSelectiveAdapterSync collects and dispatches one adapter at a time.
// The center keeps adapters that were not selected, and a machine only
// receives the adapters the operator checked.
func TestSelectiveAdapterSync(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer W11 E2E\n\temail = homer-w11@example.invalid\n")
	binary := binaryOf(t)
	machineA := makeMachine(t, root, "A", global, true)
	machineB := makeMachine(t, root, "B", global, false)
	if result := runHomer(t, binary, machineA, "init", "--json"); result.code != 0 {
		t.Fatalf("init A failed: %s%s", result.stdout, result.stderr)
	}
	vscodeMarker := "vscode-only-marker"
	piMarker := "pi-only-marker"
	writeFile(t, vscodeSettings(machineA), "{\n  \"homerE2E\": \""+vscodeMarker+"\"\n}\n")
	writeFile(t, filepath.Join(piRoot(machineA), "settings.json"), "{\n  \"theme\": \"light\",\n  \"homerE2E\": \""+piMarker+"\"\n}\n")

	serverHome := filepath.Join(root, "server-home")
	if err := os.MkdirAll(serverHome, 0o755); err != nil {
		t.Fatal(err)
	}
	token := "selective-sync-token"
	hubPort := freePort(t)
	hubProc := startHomer(t, binary, machineA, global,
		"serve", "--addr", fmt.Sprintf("127.0.0.1:%d", hubPort), "--token", token, "--home", serverHome)
	defer stopProc(t, hubProc)
	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)
	auth := map[string]string{"Authorization": "Bearer " + token}
	agentA := startHomer(t, binary, machineA, global,
		"agent", "--connect", hubURL, "--token", token, "--id", "machine-a", "--home", machineA.homerHome)
	defer stopProc(t, agentA)
	waitRegistered(t, hubURL, auth, hubProc, "machine-a")

	choices := apiGet(t, hubURL+"/api/sync/choices?direction=collect&agent=machine-a", auth, hubProc)
	ids := map[string]bool{}
	for _, item := range jsonPath(t, choices, "adapters").([]any) {
		ids[item.(map[string]any)["id"].(string)] = true
	}
	if !ids["vscode"] || !ids["pi"] {
		t.Fatalf("collect choices = %s", choices)
	}

	code, collect := apiPostJSON(t, hubURL+"/api/sync?direction=collect&agent=machine-a&confirm=true", auth, map[string]any{"adapters": []string{"vscode"}})
	if code != http.StatusOK || jsonPath(t, collect, "ok") != true {
		t.Fatalf("collect vscode = %d %s\nhub: %s", code, collect, procOutput[hubProc])
	}
	stored := storageAdapterIDs(t, apiGet(t, hubURL+"/api/storage", auth, hubProc))
	if !stored["vscode"] || stored["pi"] {
		t.Fatalf("storage after vscode collect = %#v", stored)
	}
	if body := apiGet(t, hubURL+"/api/storage/file?adapter=vscode&path=settings/settings.json", auth, hubProc); !strings.Contains(body, vscodeMarker) {
		t.Fatalf("stored vscode = %s", body)
	}
	generation := strings.TrimSpace(string(mustReadFile(t, filepath.Join(serverHome, "HEAD"))))

	code, rejected := apiPostJSON(t, hubURL+"/api/sync?direction=collect&agent=machine-a&confirm=true", auth, map[string]any{"adapters": []string{"nope"}})
	if code == http.StatusOK && jsonPath(t, rejected, "ok") == true {
		t.Fatalf("unknown adapter was collected: %s", rejected)
	}
	code, empty := apiPostJSON(t, hubURL+"/api/sync?direction=collect&agent=machine-a&confirm=true", auth, map[string]any{"adapters": []string{}})
	if code != http.StatusBadRequest || !strings.Contains(empty, "请选择至少一个适配器") {
		t.Fatalf("empty selection = %d %s", code, empty)
	}
	code, invalid := apiPostJSON(t, hubURL+"/api/sync?direction=collect&agent=machine-a&confirm=true", auth, map[string]any{"adapters": []string{"../pi"}})
	if code != http.StatusBadRequest {
		t.Fatalf("invalid adapter = %d %s", code, invalid)
	}
	if got := strings.TrimSpace(string(mustReadFile(t, filepath.Join(serverHome, "HEAD")))); got != generation {
		t.Fatalf("rejected collects advanced HEAD from %s to %s", generation, got)
	}

	code, collectPi := apiPostJSON(t, hubURL+"/api/sync?direction=collect&agent=machine-a&confirm=true", auth, map[string]any{"adapters": []string{"pi"}})
	if code != http.StatusOK || jsonPath(t, collectPi, "ok") != true {
		t.Fatalf("collect pi = %d %s", code, collectPi)
	}
	stored = storageAdapterIDs(t, apiGet(t, hubURL+"/api/storage", auth, hubProc))
	if !stored["vscode"] || !stored["pi"] {
		t.Fatalf("storage after pi collect = %#v", stored)
	}
	if body := apiGet(t, hubURL+"/api/storage/file?adapter=vscode&path=settings/settings.json", auth, hubProc); !strings.Contains(body, vscodeMarker) {
		t.Fatalf("vscode changed while collecting pi: %s", body)
	}

	writeFile(t, vscodeSettings(machineA), "{\n  \"homerE2E\": \"vscode-second\"\n}\n")
	code, again := apiPostJSON(t, hubURL+"/api/sync?direction=collect&agent=machine-a&confirm=true", auth, map[string]any{"adapters": []string{"vscode"}})
	if code != http.StatusOK || jsonPath(t, again, "ok") != true {
		t.Fatalf("recollect vscode = %d %s", code, again)
	}
	if body := apiGet(t, hubURL+"/api/storage/file?adapter=vscode&path=settings/settings.json", auth, hubProc); !strings.Contains(body, "vscode-second") {
		t.Fatalf("vscode was not updated: %s", body)
	}
	if body := apiGet(t, hubURL+"/api/storage/file?adapter=pi&path=settings/settings.json", auth, hubProc); !strings.Contains(body, piMarker) {
		t.Fatalf("pi changed while recollecting vscode: %s", body)
	}

	agentB := startHomer(t, binary, machineB, global,
		"agent", "--connect", hubURL, "--token", token, "--id", "machine-b", "--home", machineB.homerHome)
	defer stopProc(t, agentB)
	waitRegistered(t, hubURL, auth, hubProc, "machine-b")

	code, missing := apiPostJSON(t, hubURL+"/api/agents/machine-b/pull?confirm=true", auth, map[string]any{"adapters": []string{"herdr"}})
	if code != http.StatusBadRequest || !strings.Contains(missing, "中心没有适配器 herdr") {
		t.Fatalf("dispatch missing herdr = %d %s", code, missing)
	}
	code, dispatch := apiPostJSON(t, hubURL+"/api/agents/machine-b/pull?confirm=true", auth, map[string]any{"adapters": []string{"vscode"}})
	if code != http.StatusOK || jsonPath(t, dispatch, "ok") != true {
		t.Fatalf("dispatch vscode = %d %s\nhub: %s", code, dispatch, procOutput[hubProc])
	}
	bVSCode, err := os.ReadFile(vscodeSettings(machineB))
	if err != nil || !strings.Contains(string(bVSCode), "vscode-second") {
		t.Fatalf("B vscode = %q err=%v", bVSCode, err)
	}
	if _, err := os.Stat(filepath.Join(piRoot(machineB), "settings.json")); err == nil {
		t.Fatal("dispatching vscode also wrote pi settings")
	}

	code, dispatchPi := apiPostJSON(t, hubURL+"/api/agents/machine-b/pull?confirm=true", auth, map[string]any{"adapters": []string{"pi"}})
	if code != http.StatusOK || jsonPath(t, dispatchPi, "ok") != true {
		t.Fatalf("dispatch pi = %d %s", code, dispatchPi)
	}
	bPi, err := os.ReadFile(filepath.Join(piRoot(machineB), "settings.json"))
	if err != nil || !strings.Contains(string(bPi), piMarker) {
		t.Fatalf("B pi = %q err=%v", bPi, err)
	}
	bVSCode, err = os.ReadFile(vscodeSettings(machineB))
	if err != nil || !strings.Contains(string(bVSCode), "vscode-second") {
		t.Fatalf("dispatching pi changed vscode: %q err=%v", bVSCode, err)
	}
}

func vscodeSettings(m machine) string {
	return filepath.Join(m.fakeHome, ".config", "Code", "settings.json")
}

func storageAdapterIDs(t *testing.T, body string) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, item := range jsonPath(t, body, "adapters").([]any) {
		ids[item.(map[string]any)["id"].(string)] = true
	}
	return ids
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// waitRegistered blocks until agentID appears in the hub's registry.
func waitRegistered(t *testing.T, hubURL string, auth map[string]string, hubProc *exec.Cmd, agentID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		body := apiGet(t, hubURL+"/api/agents", auth, hubProc)
		if items, ok := jsonPath(t, body, "agents").([]any); ok {
			for _, item := range items {
				if item.(map[string]any)["agentId"] == agentID {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s did not register; hub output: %s", agentID, procOutput[hubProc])
}
