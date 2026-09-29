package hub

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// EnrollmentManager implements the Tailscale-style onboarding contract:
// the administrator mints a one-time enrollment code (valid for a limited
// window), an agent redeems it exactly once and receives a per-agent
// secret, and all subsequent agent traffic authenticates with that secret.
// A leaked or retired machine never compromises its siblings.
type EnrollmentManager struct {
	mu sync.Mutex

	// codes holds minted enrollment codes that are not yet redeemed.
	codes map[string]enrollmentCode
	// secrets maps agentID -> sha256 of the live per-agent secret.
	secrets map[string][32]byte
}

type enrollmentCode struct {
	expiresAt time.Time
}

func NewEnrollmentManager() *EnrollmentManager {
	return &EnrollmentManager{
		codes:   make(map[string]enrollmentCode),
		secrets: make(map[string][32]byte),
	}
}

// Mint creates a fresh one-time enrollment code valid for ttl.
func (m *EnrollmentManager) Mint(ttl time.Duration) (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成接入码: %w", err)
	}
	code := "hr_" + hex.EncodeToString(buf)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes[code] = enrollmentCode{expiresAt: time.Now().Add(ttl)}
	return code, nil
}

// ValidCode reports whether a one-time enrollment code is still
// redeemable WITHOUT burning it — the binary download on a fresh machine
// happens before enrollment and must be retryable. Redemption
// (Redeem) remains strictly one-shot.
func (m *EnrollmentManager) ValidCode(code string) bool {
	if code == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.codes[code]
	return ok && time.Now().Before(entry.expiresAt)
}

// Redeem consumes a one-time enrollment code and returns a fresh
// per-agent secret. The code is burned regardless of what the caller does
// with the secret; the caller binds the secret to an agentID via
// BindAgent (the hub learns the agentID at registration time).
func (m *EnrollmentManager) Redeem(code string) (string, error) {
	m.mu.Lock()
	entry, ok := m.codes[code]
	if ok {
		delete(m.codes, code)
	}
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("接入码无效或已使用")
	}
	if time.Now().After(entry.expiresAt) {
		return "", fmt.Errorf("接入码已过期，请到控制台重新生成")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成 agent 密钥: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// BindAgent records (or replaces — re-install scenario) the secret hash
// for an agentID. Binding at registration time is what ties a redeemed
// secret to a concrete machine.
func (m *EnrollmentManager) BindAgent(agentID, secret string) {
	if agentID == "" || secret == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secrets[agentID] = sha256.Sum256([]byte(secret))
}

// VerifySecret reports whether the presented secret matches the live
// binding for agentID.
func (m *EnrollmentManager) VerifySecret(agentID, secret string) bool {
	if agentID == "" || secret == "" {
		return false
	}
	want, ok := m.secrets[agentID]
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// Revoke drops an agent's secret binding. The agent's next request fails
// with 401; the agentID itself stays eligible for a fresh enrollment
// (revocation targets the credential, not the machine identity).
func (m *EnrollmentManager) Revoke(agentID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.secrets[agentID]; !ok {
		return false
	}
	delete(m.secrets, agentID)
	return true
}

// AuthorizedAgent resolves the agentID presenting a Bearer secret. It
// returns "" when the secret matches no live binding — agents may also
// authenticate via the management hub token during the migration window.
func (m *EnrollmentManager) AuthorizedAgent(bearer string) string {
	if bearer == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for agentID, want := range m.secrets {
		got := sha256.Sum256([]byte(bearer))
		if subtle.ConstantTimeCompare(want[:], got[:]) == 1 {
			return agentID
		}
	}
	return ""
}
