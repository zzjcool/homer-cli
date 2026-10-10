package e2e

import (
	"bytes"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// postJSONWithToken posts body with the hub token and returns the status code
// plus the response body (trimmed). It is the JSON-body counterpart of
// postStatusWithToken in hub_serve_test.go.
func postJSONWithToken(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(bytes.TrimSpace(data))
}

// TestPluginsServeLifecycle exercises the plugin API against a real `homer
// serve` subprocess (the assembly reviewers flagged as the F1 gap): install
// persists across a hub restart, and the uninstall guards answer with the
// documented codes.
func TestPluginsServeLifecycle(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer Plugins E2E\n\temail = homer-plugins@example.invalid\n")

	binary := binaryOf(t)
	machineA := makeMachine(t, root, "A", global, true)
	if result := runHomer(t, binary, machineA, "init", "--json"); result.code != 0 {
		t.Fatalf("init failed: %s %s", result.stdout, result.stderr)
	}
	// A baseline push gives the hub a generation to build on.
	if result := runHomer(t, binary, machineA, "push", "--yes", "--json"); result.code != 0 {
		t.Fatalf("baseline push failed: %s %s", result.stdout, result.stderr)
	}

	serve := exec.Command(binary, "serve", "--home", machineA.homerHome, "--addr", "127.0.0.1:0", "--token", "plugins-e2e-token")
	orphanGuard(serve)
	serve.Env = serveEnv(machineA, global)
	out := &lockedBuilder{}
	serve.Stdout = out
	serve.Stderr = out
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = serve.Process.Kill()
		_, _ = serve.Process.Wait()
	}()
	addr := waitForServeAddr(t, out)
	token := "plugins-e2e-token"

	// Fresh hub home must seed every official plugin (legacy migration).
	list := waitHTTPWithToken(t, "http://"+addr+"/api/plugins", token, 10*time.Second)
	for _, id := range []string{`"pi"`, `"keyring"`, `"ssh-key"`} {
		if !strings.Contains(list, id) {
			t.Fatalf("seeded plugin list missing %s: %.200s", id, list)
		}
	}

	// Uninstalling an adapter that still has center data is guarded.
	uninstallKey := func(body string) (int, string) {
		code, resp := postJSONWithToken(t, "http://"+addr+"/api/plugins/uninstall", token, body)
		return code, resp
	}
	code, resp := uninstallKey(`{"id":"pi"}`)
	if code != http.StatusConflict || !strings.Contains(resp, "uninstall-guard") {
		t.Fatalf("unguarded pi uninstall = %d %.200s, want 409 uninstall-guard", code, resp)
	}

	// ssh-key is an action: uninstall succeeds without force.
	code, resp = uninstallKey(`{"id":"ssh-key"}`)
	if code != http.StatusOK || !strings.Contains(resp, `"ok":true`) {
		t.Fatalf("ssh-key uninstall = %d %.200s, want 200", code, resp)
	}

	// Reinstall for the restart assertion below.
	code, resp = postJSONWithToken(t, "http://"+addr+"/api/plugins/install", token, `{"id":"ssh-key"}`)
	if code != http.StatusOK || !strings.Contains(resp, `"ssh-key"`) {
		t.Fatalf("ssh-key install = %d %.200s, want 200", code, resp)
	}

	// Restart the hub process: the installed set must survive (plugins.json).
	if err := serve.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waitExit(serve):
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit on SIGINT")
	}
	serve2 := exec.Command(binary, "serve", "--home", machineA.homerHome, "--addr", "127.0.0.1:0", "--token", token)
	orphanGuard(serve2)
	serve2.Env = serveEnv(machineA, global)
	out2 := &lockedBuilder{}
	serve2.Stdout = out2
	serve2.Stderr = out2
	if err := serve2.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = serve2.Process.Kill()
		_, _ = serve2.Process.Wait()
	}()
	addr2 := waitForServeAddr(t, out2)

	list2 := waitHTTPWithToken(t, "http://"+addr2+"/api/plugins", token, 10*time.Second)
	if !strings.Contains(list2, `"ssh-key"`) {
		t.Fatalf("ssh-key did not survive hub restart: %.300s", list2)
	}
	if strings.Contains(list2, `"available":[{`) || !strings.Contains(list2, `"available"`) {
		// available must exist; ssh-key reinstalled means it is not in it.
		t.Fatalf("available shape unexpected: %.300s", list2)
	}
	if idx := strings.Index(list2, `"available"`); idx >= 0 && strings.Contains(list2[idx:], `"ssh-key"`) {
		t.Fatalf("reinstalled ssh-key still listed as available: %.300s", list2[idx:])
	}
}
