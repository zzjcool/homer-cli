package web

import (
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installScriptSandbox runs the real install script against a throw-away HOME
// and a stub `homer` that only records it was called. Safety rules, because an
// earlier manual check of this script reached the real user systemd:
//   - XDG_RUNTIME_DIR / DBUS_SESSION_BUS_ADDRESS are not passed, so the
//     user-systemd branch (`-n "$XDG_RUNTIME_DIR"`) cannot be taken;
//   - the root/system-systemd branch needs uid 0, so the test skips as root;
//   - PATH contains no systemctl, so even a changed branch could not act.
func runInstallScript(t *testing.T, token string, seed func(home string)) (home, output string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root: install.sh would write a real /etc/systemd/system unit")
	}
	root := t.TempDir()
	fakeHome := filepath.Join(root, "home")
	homerHome := filepath.Join(root, "homer-home")
	binDir := filepath.Join(fakeHome, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(homerHome, 0o700); err != nil {
		t.Fatal(err)
	}
	// Stub binary: exits immediately so the script's setsid/nohup start is
	// harmless and no real agent is created.
	if err := os.WriteFile(filepath.Join(binDir, "homer"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if seed != nil {
		seed(homerHome)
	}
	// The script always downloads the binary first. Serve a harmless stub from
	// a local test server so nothing touches the network or a real hub.
	const stubBinary = "#!/bin/sh\nexit 0\n"
	stubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/homer":
			_, _ = w.Write([]byte(stubBinary))
		case "/dl/homer.gz":
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(stubBinary))
			_ = gz.Close()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stubServer.Close)
	script := RenderInstallScript(stubServer.URL, "linux", "amd64")
	scriptPath := filepath.Join(root, "install.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", scriptPath, "--token", token)
	cmd.Env = []string{
		"HOME=" + fakeHome,
		"HOMER_HOME=" + homerHome,
		"PATH=" + binDir + ":/usr/bin:/bin",
	}
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("install.sh did not return within 30s: %s", out)
	}
	if runErr != nil {
		t.Fatalf("install.sh failed: %v\n%s", runErr, out)
	}
	return homerHome, string(out)
}

func TestInstallScriptReenrollMovesOldAgentIdentityAside(t *testing.T) {
	home, output := runInstallScript(t, "hr_new-enrollment-code", func(home string) {
		if err := os.WriteFile(filepath.Join(home, "agent.json"), []byte(`{"agentId":"old","agentSecret":"old-secret"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(filepath.Join(home, "agent.json")); !os.IsNotExist(err) {
		t.Fatalf("old agent.json must not stay in place when re-enrolling with a new hr_ code (err=%v)", err)
	}
	backup, err := os.ReadFile(filepath.Join(home, "agent.json.before-reenroll"))
	if err != nil || !strings.Contains(string(backup), "old-secret") {
		t.Fatalf("old identity should be kept as agent.json.before-reenroll, not deleted: %v %q", err, backup)
	}
	if token, _ := os.ReadFile(filepath.Join(home, "keys", "hub-token")); string(token) != "hr_new-enrollment-code" {
		t.Fatalf("hub-token = %q, want the new enrollment code", token)
	}
	if !strings.Contains(output, "agent.json.before-reenroll") {
		t.Fatalf("the user should be told the old identity was moved: %s", output)
	}
}

// A plain hub token is a shared credential, not an identity swap: the
// machine's own agent.json must be left exactly as it is.
func TestInstallScriptHubTokenKeepsAgentIdentity(t *testing.T) {
	home, _ := runInstallScript(t, "plain-hub-token", func(home string) {
		if err := os.WriteFile(filepath.Join(home, "agent.json"), []byte(`{"agentId":"keep","agentSecret":"keep-secret"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	data, err := os.ReadFile(filepath.Join(home, "agent.json"))
	if err != nil || !strings.Contains(string(data), "keep-secret") {
		t.Fatalf("agent.json must be untouched for a non-hr_ token: %v %q", err, data)
	}
	if _, err := os.Stat(filepath.Join(home, "agent.json.before-reenroll")); !os.IsNotExist(err) {
		t.Fatalf("no backup should be made for a non-hr_ token (err=%v)", err)
	}
}

func TestInstallScriptFreshInstallNeedsNoBackup(t *testing.T) {
	home, output := runInstallScript(t, "hr_first-code", nil)
	if _, err := os.Stat(filepath.Join(home, "agent.json.before-reenroll")); !os.IsNotExist(err) {
		t.Fatalf("a fresh install has nothing to back up (err=%v)", err)
	}
	if strings.Contains(output, "before-reenroll") {
		t.Fatalf("fresh install should not mention a backup: %s", output)
	}
}
