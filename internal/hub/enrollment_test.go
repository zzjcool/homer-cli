package hub

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEnrollmentLifecycle(t *testing.T) {
	mgr := NewEnrollmentManager()
	code, err := mgr.Mint(24 * time.Hour)
	if err != nil || code == "" || len(code) < 20 {
		t.Fatalf("Mint(): %v, %q", err, code)
	}
	if !mgr.ValidCode(code) {
		t.Fatal("minted code is not valid")
	}
	secret, err := mgr.Redeem(code)
	if err != nil || secret == "" {
		t.Fatalf("Redeem(): %v, %q", err, secret)
	}
	if mgr.ValidCode(code) {
		t.Fatal("Redeem() did not burn the one-time code")
	}
	if _, err := mgr.Redeem(code); err == nil {
		t.Fatal("one-time code redeemed more than once")
	}
	mgr.BindAgent("agent-1", secret)
	if !mgr.VerifySecret("agent-1", secret) {
		t.Fatal("issued secret not verifiable")
	}
	if mgr.VerifySecret("agent-1", "wrong") || mgr.VerifySecret("other-agent", secret) {
		t.Fatal("wrong secret or wrong agent ID verified")
	}
	if mgr.AuthorizedAgent(secret) != "agent-1" || mgr.AuthorizedAgent("wrong") != "" {
		t.Fatal("AuthorizedAgent returned an unexpected binding")
	}
}

func TestEnrollmentExpiry(t *testing.T) {
	mgr := NewEnrollmentManager()
	code, err := mgr.Mint(-time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if mgr.ValidCode(code) {
		t.Fatal("expired code is still valid")
	}
	if _, err := mgr.Redeem(code); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("Redeem(expired) error = %v", err)
	}
}

func TestEnrollmentRevocationAndHook(t *testing.T) {
	mgr := NewEnrollmentManager()
	mgr.BindAgent("agent-1", "machine-secret")
	var calls int
	mgr.SetRevokeHook(func(agentID string) {
		calls++
		if agentID != "agent-1" {
			t.Errorf("revoke hook agentID = %q", agentID)
		}
		if mgr.VerifySecret(agentID, "machine-secret") {
			t.Error("revoked secret remains valid inside the hook")
		}
	})
	if !mgr.Revoke("agent-1") {
		t.Fatal("Revoke() returned false for a bound agent")
	}
	if calls != 1 {
		t.Fatalf("revoke hook called %d times, want once", calls)
	}
	if mgr.VerifySecret("agent-1", "machine-secret") {
		t.Fatal("revoked secret still verifies")
	}
	if mgr.Revoke("agent-1") {
		t.Fatal("second Revoke() should return false")
	}
	if calls != 1 {
		t.Fatalf("revoke hook called %d times after failed revoke, want once", calls)
	}
	mgr.BindAgent("agent-1", "replacement-secret")
	if !mgr.VerifySecret("agent-1", "replacement-secret") {
		t.Fatal("a fresh binding for the revoked agent ID was rejected")
	}
}

func TestEnrollmentRebind(t *testing.T) {
	mgr := NewEnrollmentManager()
	mgr.BindAgent("agent-1", "first-secret")
	mgr.BindAgent("agent-1", "second-secret")
	if mgr.VerifySecret("agent-1", "first-secret") || !mgr.VerifySecret("agent-1", "second-secret") {
		t.Fatal("rebinding did not replace the old secret")
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
		t.Fatal("Revoke(box) = false")
	}
	third, err := OpenEnrollment(home)
	if err != nil {
		t.Fatal(err)
	}
	if third.VerifySecret("box", secret) {
		t.Fatal("revocation did not survive restart")
	}
}

func TestEnrollmentRace(t *testing.T) {
	mgr := NewEnrollmentManager()
	const workers = 32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := "agent-race"
			secret := "secret-" + strings.Repeat("x", i+1)
			for j := 0; j < 30; j++ {
				mgr.BindAgent(id, secret)
				_ = mgr.VerifySecret(id, secret)
				_ = mgr.AuthorizedAgent(secret)
				if j%3 == 0 {
					_ = mgr.Revoke(id)
				}
			}
		}()
	}
	wg.Wait()
}
