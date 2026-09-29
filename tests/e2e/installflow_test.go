package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInstallScriptEnrollFlow reproduces the USER's exact path:
// install.sh (with a one-time hr_ code) → binary download → agent boot →
// enroll → poll (with drift). No --token direct connection anywhere.
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
	os.MkdirAll(serverHome, 0o755)
	token := "install-flow-token"
	hubPort := freePort(t)
	hubProc := startHomer(t, binary, machineA, global,
		"serve", "--addr", fmt.Sprintf("127.0.0.1:%d", hubPort), "--token", token, "--home", serverHome)
	defer stopProc(t, hubProc)
	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)
	auth := map[string]string{"Authorization": "Bearer " + token}

	// Mint a one-time code as the console would.
	join := apiGet(t, hubURL+"/api/auth/join", auth, hubProc)
	codeLine := jsonPath(t, join, "command").(string)
	if !strings.Contains(codeLine, "hr_") {
		t.Fatalf("join command has no hr_ code: %s", codeLine)
	}
	code := codeLine[strings.Index(codeLine, "hr_"):]
	code = strings.TrimSpace(code)

	// "New machine": fetch install.sh, run it with the code, exactly like
	// curl -fsSL <hub>/install.sh | sh -s -- --token <code>.
	script := apiGet(t, hubURL+"/install.sh", map[string]string{}, hubProc)
	scriptFile := filepath.Join(root, "install.sh")
	writeFile(t, scriptFile, script)
	machineB := makeMachine(t, root, "B", global, false)
	fakeHome := machineB.fakeHome
	homerHome := filepath.Join(root, "B-machine-home")
	os.MkdirAll(homerHome, 0o755)

	install := exec.Command("sh", scriptFile, "--token", code)
	install.Env = []string{
		"HOME=" + fakeHome,
		"HOMER_HOME=" + homerHome,
		"PATH=" + filepath.Dir(binary) + ":" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + global,
		"GIT_CONFIG_NOSYSTEM=1",
	}
	// The script backgrounds the agent (nohup) — it must return promptly
	// instead of holding the terminal forever (the bug that made the
	// user Ctrl+C the agent to death).
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = install.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = install.Process.Kill()
		t.Fatal("install.sh did not return — the agent still occupies the foreground")
	}
	t.Logf("install.sh output:\n%s", out)
	if err != nil {
		t.Fatalf("install.sh failed: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("pkill", "-f", "homer agent --connect").Run()
	})

	// The install script execs the agent — it is now running in the
	// install process's place. Give it a few poll cycles.
	deadline := time.Now().Add(45 * time.Second)
	enrolled := ""
	for time.Now().Before(deadline) && enrolled == "" {
		body := apiGet(t, hubURL+"/api/agents", auth, hubProc)
		for _, item := range jsonPath(t, body, "agents").([]any) {
			id := item.(map[string]any)["agentId"].(string)
			if id != "hw" && id != "machine-a" {
				enrolled = id
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if enrolled == "" {
		t.Fatal("the enrolled machine never appeared in the registry")
	}
	t.Logf("enrolled machine: %s", enrolled)

	// Drift must arrive with the polls (the console's machine badge).
	deadline = time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		body := apiGet(t, hubURL+"/api/console", auth, hubProc)
		for _, m := range jsonPath(t, body, "machines").([]any) {
			mm := m.(map[string]any)
			if mm["agentId"] == enrolled && mm["drift"] != nil {
				t.Logf("drift arrived: %v", mm["drift"])
				return // PASS: enroll → poll → drift all work
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("drift never arrived for %s (poll loop stalled?)", enrolled)
}
