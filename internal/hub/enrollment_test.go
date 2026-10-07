package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Enrollment manager (Tailscale-style): one-time codes minted by the
// administrator, redeemed by agents for per-agent secrets.
func TestEnrollmentLifecycle(t *testing.T) {
	mgr := NewEnrollmentManager()

	// Mint a code valid 24h.
	code, err := mgr.Mint(24 * time.Hour)
	if err != nil || code == "" || len(code) < 20 {
		t.Fatalf("mint: %v %q", err, code)
	}

	// Redeem it for an agent secret.
	secret, err := mgr.Redeem(code)
	if err != nil || secret == "" {
		t.Fatalf("redeem: %v %q", err, secret)
	}

	// The code is one-shot: second redeem fails.
	if _, err := mgr.Redeem(code); err == nil {
		t.Fatal("code reused successfully — must be one-shot")
	}

	// Redeem issues the secret; binding happens at registration.
	mgr.BindAgent("agent-1", secret)
	if !mgr.VerifySecret("agent-1", secret) {
		t.Fatal("issued secret not verifiable")
	}
	if mgr.VerifySecret("agent-1", "wrong") {
		t.Fatal("wrong secret verified")
	}
	if mgr.VerifySecret("other-agent", secret) {
		t.Fatal("secret verified for a different agent")
	}
}

func TestEnrollmentExpiry(t *testing.T) {
	mgr := NewEnrollmentManager()
	code, _ := mgr.Mint(-time.Hour) // already expired
	if _, err := mgr.Redeem(code); err == nil {
		t.Fatal("expired code redeemed")
	} else if !strings.Contains(err.Error(), "过期") {
		t.Fatalf("expiry error = %v", err)
	}
}

func TestEnrollmentRevocation(t *testing.T) {
	mgr := NewEnrollmentManager()
	code, _ := mgr.Mint(time.Hour)
	secret, _ := mgr.Redeem(code)
	mgr.BindAgent("agent-1", secret)

	if !mgr.Revoke("agent-1") {
		t.Fatal("revoke returned false for a bound agent")
	}
	if mgr.VerifySecret("agent-1", secret) {
		t.Fatal("revoked agent still verifies")
	}
	if mgr.Revoke("agent-1") {
		t.Fatal("second revoke should report nothing to do")
	}
	// The agentID stays un-banned: a fresh enrollment may reuse it.
	code2, _ := mgr.Mint(time.Hour)
	if _, err := mgr.Redeem(code2); err != nil {
		t.Fatalf("re-enroll blocked: %v", err)
	}
}

func TestEnrollmentRebind(t *testing.T) {
	// A re-installed machine re-enrolls with the same agent ID: the new
	// secret replaces the old one.
	mgr := NewEnrollmentManager()
	code1, _ := mgr.Mint(time.Hour)
	secret1, _ := mgr.Redeem(code1)
	mgr.BindAgent("agent-1", secret1)
	code2, _ := mgr.Mint(time.Hour)
	secret2, _ := mgr.Redeem(code2)
	mgr.BindAgent("agent-1", secret2)
	if mgr.VerifySecret("agent-1", secret1) {
		t.Fatal("old secret still valid after rebind")
	}
	if !mgr.VerifySecret("agent-1", secret2) {
		t.Fatal("new secret invalid after rebind")
	}
}

func TestEnrollmentBindingSurvivesRestart(t *testing.T) {
	home := t.TempDir()
	const secret = "machine-secret-not-for-disk"
	first, err := OpenEnrollment(home)
	if err != nil {
		t.Fatal(err)
	}
	first.BindAgent("box", secret)
	second, err := OpenEnrollment(home)
	if err != nil {
		t.Fatal(err)
	}
	if !second.VerifySecret("box", secret) || second.AuthorizedAgent(secret) != "box" {
		t.Fatal("restart forgot the enrolled machine")
	}
	body, err := os.ReadFile(filepath.Join(home, "keys", agentSecretsFilename))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("secret stored in clear: %s", body)
	}
	info, err := os.Stat(filepath.Join(home, "keys", agentSecretsFilename))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v err=%v", info, err)
	}
	if !second.Revoke("box") {
		t.Fatal("revoke")
	}
	third, err := OpenEnrollment(home)
	if err != nil {
		t.Fatal(err)
	}
	if third.VerifySecret("box", secret) {
		t.Fatal("revocation did not survive restart")
	}
}
