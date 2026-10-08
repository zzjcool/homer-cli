package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestInstallScriptEnrollFlow reproduces the user's real first-contact path:
// install.sh + one-time hr_ code → downloaded binary → background agent →
// WebSocket hello redeems the code and persists an agent secret → heartbeat
// reports drift. It never substitutes a shared hub token for enrollment.
func TestInstallScriptEnrollFlow(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer W11 E2E\n\temail = homer-w11@example.invalid\n")
	binary := binaryOf(t)
	machineA := makeMachine(t, root, "A", global, true)
	if result := runHomer(t, binary, machineA, "init", "--json"); result.code != 0 {
		t.Fatalf("init A: %s%s", result.stdout, result.stderr)
	}
	serverHome := filepath.Join(root, "server-home")
	if err := os.MkdirAll(serverHome, 0o700); err != nil {
		t.Fatal(err)
	}
	token := "install-flow-token"
	hubPort := freePort(t)
	hubProc := startHomer(t, binary, machineA, global,
		"serve", "--addr", fmt.Sprintf("127.0.0.1:%d", hubPort), "--token", token, "--home", serverHome)
	defer stopProc(t, hubProc)
	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)
	auth := map[string]string{"Authorization": "Bearer " + token}

	// Mint a one-time code as the console does.
	join := apiGet(t, hubURL+"/api/auth/join", auth, hubProc)
	codeLine := jsonPath(t, join, "command").(string)
	if !strings.Contains(codeLine, "hr_") {
		t.Fatalf("join command has no hr_ code: %s", codeLine)
	}
	code := strings.TrimSpace(codeLine[strings.LastIndex(codeLine, "hr_"):])
	if strings.Contains(code, " ") || len(code) < 10 {
		t.Fatalf("extracted enrollment code = %q", code)
	}

	// Run the same public script the user pipes into sh. It must install the
	// binary and return without keeping the terminal attached to the daemon.
	script := apiGet(t, hubURL+"/install.sh", map[string]string{}, hubProc)
	scriptFile := filepath.Join(root, "install.sh")
	writeFile(t, scriptFile, script)
	machineB := makeMachine(t, root, "B", global, false)
	homerHome := filepath.Join(root, "B-machine-home")
	if err := os.MkdirAll(homerHome, 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	install := exec.CommandContext(ctx, "sh", scriptFile, "--token", code)
	install.Env = []string{
		"HOME=" + machineB.fakeHome,
		"HOMER_HOME=" + homerHome,
		"PATH=" + filepath.Dir(binary) + ":" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + global,
		"GIT_CONFIG_NOSYSTEM=1",
	}
	started := time.Now()
	out, err := install.CombinedOutput()
	t.Logf("install.sh output (returned in %s):\n%s", time.Since(started).Round(time.Millisecond), out)
	if ctx.Err() != nil {
		t.Fatalf("install.sh did not return within 30s; background agent may have occupied the foreground: %s", out)
	}
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if time.Since(started) >= 30*time.Second {
		t.Fatalf("install.sh returned too slowly: %s", time.Since(started))
	}
	pid := installScriptAgentPID(t, string(out))
	t.Cleanup(func() { stopBackgroundPID(pid) })

	// The daemon got an isolated home and must have completed the hello code
	// exchange before persisting its long-lived per-agent secret.
	var persisted struct {
		AgentID     string `json:"agentId"`
		HubURL      string `json:"hubUrl"`
		AgentSecret string `json:"agentSecret"`
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(filepath.Join(homerHome, "agent.json"))
		if readErr == nil && json.Unmarshal(data, &persisted) == nil && persisted.AgentID != "" && persisted.AgentSecret != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if persisted.AgentID == "" || persisted.AgentSecret == "" {
		t.Fatalf("agent did not persist the secret returned by WS hello; agent.json=%+v log=%s", persisted, readAgentInstallLog(homerHome))
	}
	if persisted.HubURL != hubURL {
		t.Fatalf("agent.json hubUrl = %q, want %q", persisted.HubURL, hubURL)
	}
	if strings.Contains(string(mustReadFile(t, filepath.Join(homerHome, "agent.json"))), code) {
		t.Fatal("agent.json retained the one-time enrollment code")
	}

	waitRegistered(t, hubURL, auth, hubProc, persisted.AgentID)
	// Drift comes from the stream hello/heartbeat; it is not a legacy poll.
	deadline = time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		body := apiGet(t, hubURL+"/api/console", auth, hubProc)
		for _, machine := range jsonPath(t, body, "machines").([]any) {
			item := machine.(map[string]any)
			if item["agentId"] == persisted.AgentID && item["drift"] != nil {
				t.Logf("WS drift arrived for %s: %v", persisted.AgentID, item["drift"])
				stopBackgroundPID(pid)
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("drift never arrived for %s after WS hello (agent log: %s)", persisted.AgentID, readAgentInstallLog(homerHome))
}

var installPIDPattern = regexp.MustCompile(`(?m)PID:\s*([0-9]+)`)

func installScriptAgentPID(t *testing.T, output string) int {
	t.Helper()
	match := installPIDPattern.FindStringSubmatch(output)
	if len(match) != 2 {
		t.Fatalf("install.sh did not report a background agent PID:\n%s", output)
	}
	var pid int
	if _, err := fmt.Sscanf(match[1], "%d", &pid); err != nil || pid <= 1 {
		t.Fatalf("invalid background agent PID %q: %v", match[1], err)
	}
	return pid
}

func stopBackgroundPID(pid int) {
	if pid <= 1 {
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := process.Signal(os.Signal(nil)); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readAgentInstallLog(home string) string {
	data, err := os.ReadFile(filepath.Join(home, "agent.log"))
	if err != nil {
		return err.Error()
	}
	if len(data) > 4000 {
		data = data[len(data)-4000:]
	}
	return string(data)
}
