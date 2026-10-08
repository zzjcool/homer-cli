package hub

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EnrollmentManager implements the Tailscale-style onboarding contract:
// the administrator mints a one-time enrollment code (valid for a limited
// window), an agent redeems it exactly once and receives a per-agent
// secret, and all subsequent agent traffic authenticates with that secret.
// A leaked or retired machine never compromises its siblings.
//
// One-time codes stay in memory. Bound secret hashes are written to
// keys/agent-secrets.json so a hub restart still recognizes machines
// that already enrolled. The file stores hashes, never the secret itself.
type EnrollmentManager struct {
	mu sync.Mutex

	// path is empty for an in-memory manager (tests). OpenEnrollment sets it.
	path string
	// codes holds minted enrollment codes that are not yet redeemed.
	codes map[string]enrollmentCode
	// secrets maps agentID -> sha256 of the live per-agent secret.
	secrets    map[string][32]byte
	revokeHook func(agentID string)
}

const agentSecretsFilename = "agent-secrets.json"

type enrollmentCode struct {
	expiresAt time.Time
}

func NewEnrollmentManager() *EnrollmentManager {
	return &EnrollmentManager{
		codes:   make(map[string]enrollmentCode),
		secrets: make(map[string][32]byte),
	}
}

// OpenEnrollment loads the machines already bound under home. A missing
// file is a fresh hub. A damaged file is returned as an error so a later
// bind cannot overwrite it with an empty set.
func OpenEnrollment(home string) (*EnrollmentManager, error) {
	manager := NewEnrollmentManager()
	if home == "" {
		return manager, nil
	}
	dir := filepath.Join(home, hubTokenDirectory)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建 keys 目录: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("加固 keys 目录: %w", err)
	}
	manager.path = filepath.Join(dir, agentSecretsFilename)
	if err := manager.load(); err != nil {
		return nil, err
	}
	return manager, nil
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
	_ = m.saveLocked()
}

// VerifySecret reports whether the presented secret matches the live
// binding for agentID.
func (m *EnrollmentManager) VerifySecret(agentID, secret string) bool {
	if m == nil || agentID == "" || secret == "" {
		return false
	}
	m.mu.Lock()
	want, ok := m.secrets[agentID]
	m.mu.Unlock()
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// Revoke drops an agent's secret binding. The agent's next request fails
// with 401; the agentID itself stays eligible for a fresh enrollment
// (revocation targets the credential, not the machine identity).
func (m *EnrollmentManager) SetRevokeHook(fn func(agentID string)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.revokeHook = fn
	m.mu.Unlock()
}

func (m *EnrollmentManager) Revoke(agentID string) bool {
	if m == nil || agentID == "" {
		return false
	}
	m.mu.Lock()
	if _, ok := m.secrets[agentID]; !ok {
		m.mu.Unlock()
		return false
	}
	delete(m.secrets, agentID)
	_ = m.saveLocked()
	hook := m.revokeHook
	m.mu.Unlock()
	if hook != nil {
		hook(agentID)
	}
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

type agentSecretsFile struct {
	Agents map[string]string `json:"agents"`
}

func (m *EnrollmentManager) load() error {
	data, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取已接入机器: %w", err)
	}
	var doc agentSecretsFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("已接入机器记录损坏: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for agentID, encoded := range doc.Agents {
		sum, err := hex.DecodeString(encoded)
		if err != nil || len(sum) != sha256.Size || agentID == "" {
			return fmt.Errorf("已接入机器记录损坏")
		}
		var parsed [32]byte
		copy(parsed[:], sum)
		m.secrets[agentID] = parsed
	}
	return nil
}

func (m *EnrollmentManager) saveLocked() error {
	if m.path == "" {
		return nil
	}
	doc := agentSecretsFile{Agents: make(map[string]string, len(m.secrets))}
	for agentID, sum := range m.secrets {
		doc.Agents[agentID] = hex.EncodeToString(sum[:])
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	dir := filepath.Dir(m.path)
	tmp, err := os.CreateTemp(dir, "."+agentSecretsFilename+".tmp-")
	if err != nil {
		return fmt.Errorf("写入已接入机器: %w", err)
	}
	tmpName := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("加固已接入机器记录: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入已接入机器: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("写入已接入机器: %w", err)
	}
	if err := os.Rename(tmpName, m.path); err != nil {
		return fmt.Errorf("落盘已接入机器: %w", err)
	}
	remove = false
	return nil
}
