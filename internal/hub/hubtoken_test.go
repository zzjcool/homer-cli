package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTokenFile(t *testing.T, dir, token string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hub-token"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHubTokenFileRoundTrip(t *testing.T) {
	home := t.TempDir()
	// Saving creates keys/ with 0700 and the file with 0600.
	token, created, err := EnsureHubToken(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if !created || len(token) < 32 {
		t.Fatalf("token = %q created=%v", token, created)
	}
	info, err := os.Stat(filepath.Join(home, "keys"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("keys dir = %v %v", info, err)
	}
	fileInfo, err := os.Stat(filepath.Join(home, "keys", "hub-token"))
	if err != nil || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v %v", fileInfo, err)
	}
	// A second read returns the same token without regenerating.
	again, createdAgain, err := EnsureHubToken(home, "")
	if err != nil || again != token || createdAgain {
		t.Fatalf("second read = %q created=%v err=%v", again, createdAgain, err)
	}
}

func TestHubTokenPriorityOrder(t *testing.T) {
	home := t.TempDir()
	writeTokenFile(t, filepath.Join(home, "keys"), "file-token")
	// Explicit flag token wins and is persisted (deliberate rotation).
	token, created, err := EnsureHubToken(home, "flag-token")
	if err != nil || token != "flag-token" || created {
		t.Fatalf("flag priority = %q created=%v err=%v", token, created, err)
	}
	persisted, _ := os.ReadFile(filepath.Join(home, "keys", "hub-token"))
	if strings.TrimSpace(string(persisted)) != "flag-token" {
		t.Fatalf("flag not persisted: %q", persisted)
	}
	// The env override beats the file but never writes back.
	t.Setenv("HOMER_HUB_TOKEN", "env-token")
	token, _, err = EnsureHubToken(home, "")
	if err != nil || token != "env-token" {
		t.Fatalf("env priority = %q err=%v", token, err)
	}
	persisted, _ = os.ReadFile(filepath.Join(home, "keys", "hub-token"))
	if strings.TrimSpace(string(persisted)) != "flag-token" {
		t.Fatalf("env leaked into file: %q", persisted)
	}
	// Flag beats env.
	token, _, err = EnsureHubToken(home, "flag2")
	if err != nil || token != "flag2" {
		t.Fatalf("flag over env = %q err=%v", token, err)
	}
}

func TestHubTokenFileKeepsSecretsOutOfOfWorkspace(t *testing.T) {
	home := t.TempDir()
	if _, _, err := EnsureHubToken(home, ""); err != nil {
		t.Fatal(err)
	}
	gitignore, err := os.ReadFile(filepath.Join(home, ".gitignore"))
	if err == nil && strings.Contains(string(gitignore), "hub-token") {
		t.Fatal("token file must live under keys/, which is already ignored")
	}
	// The token lives under keys/, which requiredGitignoreLines excludes.
	tokenPath := filepath.Join(home, "keys", "hub-token")
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("token file missing: %v", err)
	}
}

func TestLanIPv4(t *testing.T) {
	// Pure helper: wildcard requirement detection.
	if !requiresAdvertiseHint("0.0.0.0:7760") {
		t.Fatal("0.0.0.0 should require an advertise hint")
	}
	if requiresAdvertiseHint("192.168.1.5:7760") {
		t.Fatal("concrete IP should not require an advertise hint")
	}
	if requiresAdvertiseHint("[::]:7760") {
		t.Fatal("IPv6 wildcard handling is explicit, not a hint case")
	}
	// Interface enumeration must never guess when ambiguous: the test host
	// has at least loopback; the function's contract is "exactly one global
	// IPv4 or error".
	if _, err := LanIPv4(); err != nil && !strings.Contains(err.Error(), "全局 IPv4") {
		t.Fatalf("lanIPv4 error = %v", err)
	}
}

// An enrollment code must be able to download the binary BEFORE it is
// redeemed — a fresh machine has nothing else. The code is checked but
// NOT burned (download retries are legitimate; redemption stays one-shot).
func TestEnrollCodeAuthorizesDownload(t *testing.T) {
	manager := NewEnrollmentManager()
	code, err := manager.Mint(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !manager.ValidCode(code) {
		t.Fatal("minted code must validate before redemption")
	}
	if manager.ValidCode("hr_nonexistent") {
		t.Fatal("unknown code must not validate")
	}
	// Redemption still burns it exactly once.
	if _, err := manager.Redeem(code); err != nil {
		t.Fatal(err)
	}
	if manager.ValidCode(code) {
		t.Fatal("redeemed code must no longer validate")
	}
}
