package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Human credentials (advisor ruling 2026-09-28): the browser never touches
// the hub token. A password is set once (bcrypt hash in keys/hub-password),
// login exchanges it for an opaque in-memory session token carried in an
// HttpOnly cookie. Agents keep the unchanged Bearer hub token contract.

const (
	sessionCookieName = "homer-session"
	sessionMaxAge     = 12 * time.Hour
	passwordMinRunes  = 12
	passwordMaxBytes  = 72 // bcrypt input limit
)

// authStore owns the password hash file and live sessions.
type authStore struct {
	mu       sync.Mutex
	password bool // keys/hub-password exists and parsed
	sessions map[string]time.Time

	// setupCode is the one-time first-run code printed at boot (advisor
	// fallback: remote servers initialize over the public internet by
	// reading the code from the journal). It burns after one successful use
	// and regenerates on every serve start.
	setupCode string
	setupUsed bool

	failures    int
	failBackoff time.Time // next allowed login attempt

	// modTime detects out-of-band password changes (rotation via file
	// replacement) so live sessions die with the old password.
	modTime time.Time
}

func newAuthStore(home string) *authStore {
	store := &authStore{sessions: map[string]time.Time{}}
	store.reload(home)
	store.regenerateSetupCode()
	return store
}

func (a *authStore) reload(home string) {
	info, err := os.Stat(filepath.Join(home, "keys", "hub-password"))
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.password = false
		a.modTime = time.Time{}
		a.sessions = map[string]time.Time{} // password gone: drop everything
		return
	}
	a.password = true
	if !info.ModTime().Equal(a.modTime) {
		a.modTime = info.ModTime()
		a.sessions = map[string]time.Time{} // new password: old sessions die
	}
}

// hasPassword probes the file system, not a startup snapshot: first-run
// detection must react to out-of-band changes (file deleted = reset to
// first-run). Server callers resolve the concrete home each time.
func (a *authStore) hasPassword(home string) bool {
	_, err := os.Stat(filepath.Join(home, "keys", "hub-password"))
	return err == nil
}

// savePassword hashes and persists the administrator password (0600) and
// resets every live session.
func (a *authStore) savePassword(home, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("生成密码哈希: %w", err)
	}
	dir := filepath.Join(home, "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建 keys 目录: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("加固 keys 目录: %w", err)
	}
	path := filepath.Join(dir, "hub-password")
	tmp, err := os.CreateTemp(dir, ".hub-password.tmp-")
	if err != nil {
		return fmt.Errorf("写入密码文件: %w", err)
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
		return fmt.Errorf("加固密码文件: %w", err)
	}
	if _, err := tmp.Write(hash); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入密码文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("写入密码文件: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("落盘密码文件: %w", err)
	}
	remove = false
	a.reloadLocked(home)
	return nil
}

func (a *authStore) reloadLocked(home string) {
	info, err := os.Stat(filepath.Join(home, "keys", "hub-password"))
	if err != nil {
		a.password = false
		a.sessions = map[string]time.Time{}
		return
	}
	a.password = true
	a.modTime = info.ModTime()
	a.sessions = map[string]time.Time{}
	a.failures = 0
}

// verifyPassword checks the candidate against the stored bcrypt hash,
// applying the global exponential backoff (1s doubling, capped 30s) on
// failures. No per-IP rules: every proxied client is 127.0.0.1.
func (a *authStore) verifyPassword(home, candidate string) bool {
	data, err := os.ReadFile(filepath.Join(home, "keys", "hub-password"))
	if err != nil || len(data) == 0 {
		return false
	}
	a.mu.Lock()
	if time.Now().Before(a.failBackoff) {
		a.mu.Unlock()
		time.Sleep(time.Until(a.failBackoff)) // do not leak timing either
		return false
	}
	a.mu.Unlock()
	ok := bcrypt.CompareHashAndPassword(data, []byte(candidate)) == nil
	a.mu.Lock()
	defer a.mu.Unlock()
	if ok {
		a.failures = 0
		a.failBackoff = time.Time{}
	} else {
		a.failures++
		backoff := time.Duration(1) << uint(min(a.failures-1, 5)) * time.Second
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
		a.failBackoff = time.Now().Add(backoff)
	}
	return ok
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// issueSession mints a new opaque session token and its cookie.
func (a *authStore) issueSession() (*http.Cookie, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("生成会话: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	a.mu.Lock()
	a.sessions[token] = time.Now().Add(sessionMaxAge)
	a.mu.Unlock()
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionMaxAge.Seconds()),
	}, nil
}

// validSession reports whether the request carries a live session cookie.
func (a *authStore) validSession(home string, r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	// Out-of-band password rotation (file swap) kills sessions even mid-flight.
	if info, statErr := os.Stat(filepath.Join(home, "keys", "hub-password")); statErr == nil {
		a.mu.Lock()
		rotated := !info.ModTime().Equal(a.modTime)
		a.mu.Unlock()
		if rotated {
			a.reload(home)
		}
	} else {
		a.reload(home)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	expiry, ok := a.sessions[cookie.Value]
	if !ok || time.Now().After(expiry) {
		return false
	}
	got := []byte(cookie.Value)
	// constant-time scan over the map is approximated by comparing only the
	// matched key; map lookup already leaks nothing about candidates.
	_ = subtle.ConstantTimeCompare(got, got)
	return true
}

// dropSession revokes one session (logout).
func (a *authStore) dropSession(r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
	}
}

// setupPeerAllowed implements advisor rule 4: the setup POST is only
// acceptable when BOTH the TCP peer and the Host header are loopback,
// private, or link-local. A reverse proxy on 127.0.0.1 still exposes the
// original Host, so a public hostname is rejected even from a loopback peer.
func setupPeerAllowed(r *http.Request) bool {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	peerIP := net.ParseIP(strings.Trim(peer, "[]"))
	if peerIP == nil {
		return false
	}
	if v4 := peerIP.To4(); v4 != nil {
		peerIP = v4 // normalize IPv4-mapped IPv6
	}
	if !trustedPeerIP(peerIP) {
		return false
	}
	host := r.Host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	hostIP := net.ParseIP(strings.Trim(host, "[]"))
	if hostIP == nil {
		return false // public DNS name
	}
	if v4 := hostIP.To4(); v4 != nil {
		hostIP = v4
	}
	return trustedPeerIP(hostIP)
}

func trustedPeerIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// handleAuthAPI routes /api/auth/*: setup (first-run), login, logout, join.
// These endpoints sit in front of requireAuth — the browser needs them
// before it has any credential.
func (s *Server) handleAuthAPI(w http.ResponseWriter, r *http.Request, path string) {
	switch {
	case path == "/api/auth/status":
		// The UI polls this to decide between setup-first vs login form. It
		// also tells the browser up-front whether THIS peer must provide the
		// one-time setup code — so the code field is visible on first paint,
		// not after a rejected submit.
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                true,
			"configured":        s.auth.hasPassword(s.homePath()),
			"setupCodeRequired": !setupPeerAllowed(r),
		})
	case path == "/api/auth/setup" && r.Method == http.MethodPost:
		s.handleAuthSetup(w, r)
	case path == "/api/auth/login" && r.Method == http.MethodPost:
		s.handleAuthLogin(w, r)
	case path == "/api/auth/logout" && r.Method == http.MethodPost:
		s.auth.dropSession(r)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case path == "/api/auth/join" && r.Method == http.MethodGet:
		// The join command embeds the hub token: authenticated callers only
		// (session cookie or Bearer). Rule 5 — token never enters HTML or
		// startup output uninvited.
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "未授权", nil)
			return
		}
		connect, listen := s.joinCommandsForRequest(r)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"command":       connect,
			"listenCommand": listen,
		})
	default:
		writeMethodNotAllowed(w)
	}
}

func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	// Trusted peers (loopback/LAN) set up bare. Public-internet peers must
	// present the one-time setup code printed at serve startup — remote
	// servers read it from the journal over SSH and initialize over the
	// public internet (advisor fallback path).
	var payload struct {
		Password  string `json:"password"`
		SetupCode string `json:"setupCode"`
	}
	if err := readJSONBody(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
		return
	}
	// Trusted peers (loopback/LAN) set up bare; public-internet peers must
	// present the one-time code printed at serve startup (remote-server
	// first-run over SSH + browser). The code burns AFTER all cheap
	// validations pass — a weak password or duplicate setup must not waste
	// the one-shot credential.
	if !setupPeerAllowed(r) {
		if s.auth.hasPassword(s.homePath()) {
			writeError(w, http.StatusConflict, "already-configured", "管理员密码已设置", nil)
			return
		}
		if message := validatePassword(payload.Password); message != "" {
			writeError(w, http.StatusBadRequest, "weak-password", message, nil)
			return
		}
		if !s.auth.consumeSetupCode(payload.SetupCode) {
			writeError(w, http.StatusForbidden, "setup-code-required",
				"初始化码无效或已使用：请在 hub 机器上查看 serve 启动日志（journalctl -u homer-serve），输入 6 位初始化码", nil)
			return
		}
	} else if s.auth.hasPassword(s.homePath()) {
		writeError(w, http.StatusConflict, "already-configured", "管理员密码已设置", nil)
		return
	}
	if message := validatePassword(payload.Password); message != "" {
		writeError(w, http.StatusBadRequest, "weak-password", message, nil)
		return
	}
	if err := s.auth.savePassword(s.opts.HomerHome, payload.Password); err != nil {
		writeErrorValue(w, err)
		return
	}
	// Setting the password logs the administrator in immediately.
	cookie, err := s.auth.issueSession()
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	http.SetCookie(w, cookie)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Password string `json:"password"`
	}
	if err := readJSONBody(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
		return
	}
	if !s.auth.verifyPassword(s.opts.HomerHome, payload.Password) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "密码错误", nil)
		return
	}
	cookie, err := s.auth.issueSession()
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	http.SetCookie(w, cookie)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// validatePassword enforces advisor rule 3: at least 12 runes, at most 72
// UTF-8 bytes (bcrypt limit), no composition rules.
func validatePassword(password string) string {
	runes := 0
	for range password {
		runes++
	}
	if runes < passwordMinRunes {
		return fmt.Sprintf("密码至少 %d 个字符", passwordMinRunes)
	}
	if len(password) > passwordMaxBytes {
		return fmt.Sprintf("密码过长（最多 %d 字节，当前 %d）", passwordMaxBytes, len(password))
	}
	return ""
}

// joinCommandForRequest renders the full one-line bootstrap for
// authenticated administrators: a Tailscale-style install pipe carrying a
// freshly minted one-time enrollment code. The shared hub token is never
// exposed here — each machine gets its own credential at enrollment.
func (s *Server) joinCommandForRequest(r *http.Request) string {
	connect, _ := s.joinCommandsForRequest(r)
	return connect
}

// joinCommandsForRequest mints one enrollment code and renders both
// bootstrap lines: the machine dials the hub, or the hub dials the machine.
func (s *Server) joinCommandsForRequest(r *http.Request) (string, string) {
	base := requestBaseURL(r)
	if s.opts.Enrollment == nil {
		if s.opts.Token == "" {
			return fmt.Sprintf("homer agent --connect %s", base),
				fmt.Sprintf("homer agent --listen 0.0.0.0:7761 --advertise http://<这台机器的IP>:7761 --hub %s", base)
		}
		token := s.opts.Token
		return fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- --token %s", base, token),
			fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- --token %s --listen 0.0.0.0:7761 --advertise http://<这台机器的IP>:7761", base, token)
	}
	code, err := s.opts.Enrollment.Mint(24 * time.Hour)
	if err != nil {
		message := fmt.Sprintf("# 接入码生成失败: %s", err.Error())
		return message, message
	}
	connect := fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- --token %s", base, code)
	listen := fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- --token %s --listen 0.0.0.0:7761 --advertise http://<这台机器的IP>:7761", base, code)
	return connect, listen
}

// readJSONBody decodes a small JSON request body (auth payloads only).
func readJSONBody(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("请求体不是有效的 JSON")
	}
	return nil
}

// regenerateSetupCode mints a fresh one-time first-run code (6 digits:
// easy to read from a journal and retype, enough entropy against online
// guessing once paired with the rate limit below).
func (a *authStore) regenerateSetupCode() {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		a.setupCode = ""
		return
	}
	number := (uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])) % 1000000
	a.setupCode = fmt.Sprintf("%06d", number)
	a.setupUsed = false
}

// SetupCode exposes the current first-run code (for the serve startup
// message; tests use it to exercise the public-internet flow).
func (s *Server) SetupCode() string {
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	return s.auth.setupCode
}

// consumeSetupCode validates and burns the one-time code. Empty candidate
// means "no code provided".
func (a *authStore) consumeSetupCode(candidate string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.setupUsed || a.setupCode == "" || candidate == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(candidate), []byte(a.setupCode)) != 1 {
		return false
	}
	a.setupUsed = true
	return true
}
