package web

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Advisor ruling 2026-09-28: dual-layer credentials.
// Humans: password -> HMAC-signed HttpOnly session cookie (key in keys/session-key).
// Agents/scripts: existing Bearer hub token, unchanged.

func authFixture(t *testing.T) webFixture {
	t.Helper()
	return makeFixture(t, "base\n", "base\n")
}

func postJSON(t *testing.T, handler http.Handler, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func sessionCookie(response *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "homer-session" {
			return cookie
		}
	}
	return nil
}

// Rule 4: without a password file, non-loopback binds refuse to start.
func TestNonLoopbackWithoutPasswordRefusesToStart(t *testing.T) {
	t.Setenv("HOMER_HOME", t.TempDir())
	if _, err := NewServer(ServeOptions{Addr: "0.0.0.0:7760"}); err == nil {
		t.Fatal("non-loopback server without password/token accepted")
	}
}

// Rule 6: loopback without any credential no longer authorizes /api/*.
func TestLoopbackWithoutPasswordLocked(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	if response := request(t, server.Handler(), http.MethodGet, "/api/status"); response.Code != http.StatusUnauthorized {
		t.Fatalf("loopback bare status = %d (must be 401 now)", response.Code)
	}
	// health stays open.
	if response := request(t, server.Handler(), http.MethodGet, "/api/health"); response.Code != http.StatusOK {
		t.Fatalf("health = %d", response.Code)
	}
	// index and static stay open.
	if response := request(t, server.Handler(), http.MethodGet, "/"); response.Code != http.StatusOK {
		t.Fatalf("index = %d", response.Code)
	}
}

// Rule 4: setup POST is only accepted from loopback/private/link-local peers
// with a matching Host header; anything else gets 403.
func TestSetupNetworkRestriction(t *testing.T) {
	fixture := authFixture(t)

	cases := []struct {
		name   string
		remote string
		host   string
		want   int
	}{
		{"loopback peer + loopback host", "127.0.0.1:5555", "127.0.0.1:7760", http.StatusOK},
		{"private peer + private host", "192.168.8.20:5555", "192.168.8.204:7760", http.StatusOK},
		{"loopback peer + public host (reverse proxy)", "127.0.0.1:5555", "homerhw.openaaas.org", http.StatusForbidden},
		{"public peer", "203.0.113.9:5555", "example.com", http.StatusForbidden},
	}
	for _, testCase := range cases {
		// A fresh server per case: setup is one-shot (second attempt 409s).
		server := newWebServer(t, fixture, "", nil, nil)
		_ = os.Remove(filepath.Join(fixture.home, "keys", "hub-password"))
		request := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"password":"字password12345"}`))
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = testCase.remote
		request.Host = testCase.host
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != testCase.want {
			t.Fatalf("%s = %d, want %d; body=%s", testCase.name, response.Code, testCase.want, response.Body)
		}
	}
}

// Rule 3: password policy — 12+ runes, <=72 UTF-8 bytes, no composition rules.
func TestSetupPasswordPolicy(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	handler := server.Handler()

	short := setupFromLoopback(t, handler, `{"password":"12345678901"}`)
	if short.Code != http.StatusBadRequest {
		t.Fatalf("11-char password = %d", short.Code)
	}
	long := `{"password":"` + strings.Repeat("字", 25) + `"}` // 75 bytes
	if tooLong := setupFromLoopback(t, handler, long); tooLong.Code != http.StatusBadRequest {
		t.Fatalf("75-byte password = %d", tooLong.Code)
	}
	ok := setupFromLoopback(t, handler, `{"password":"字password12345"}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("valid password = %d body=%s", ok.Code, ok.Body)
	}
	// hash persisted 0600, never the plaintext.
	hashBytes, err := os.ReadFile(filepath.Join(fixture.home, "keys", "hub-password"))
	if err != nil || len(hashBytes) == 0 {
		t.Fatalf("hash file missing: %v", err)
	}
	info, _ := os.Stat(filepath.Join(fixture.home, "keys", "hub-password"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("hash mode = %v", info.Mode().Perm())
	}
	if strings.Contains(string(hashBytes), "字password12345") {
		t.Fatal("plaintext leaked into hash file")
	}
	// second setup attempt is refused (already initialized).
	again := setupFromLoopback(t, handler, `{"password":"another-password-123"}`)
	if again.Code != http.StatusConflict {
		t.Fatalf("second setup = %d", again.Code)
	}
}

// setupFromLoopback posts to /api/auth/setup with a loopback-shaped request
// (httptest's default RemoteAddr 192.0.2.1 is not a trusted peer).
func setupFromLoopback(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "127.0.0.1:5555"
	request.Host = "127.0.0.1:7760"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// Rule 2: login issues an HMAC-signed HttpOnly session cookie that authorizes
// /api/*. Logout clears the cookie; a copy of it stays valid until expiry.
func TestLoginSessionLifecycle(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	handler := server.Handler()

	// setup via loopback-shaped request
	setup := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"password":"字password12345"}`))
	setup.Header.Set("Content-Type", "application/json")
	setup.RemoteAddr = "127.0.0.1:5555"
	setup.Host = "127.0.0.1:7760"
	setupResponse := httptest.NewRecorder()
	handler.ServeHTTP(setupResponse, setup)
	if setupResponse.Code != http.StatusOK {
		t.Fatalf("setup = %d", setupResponse.Code)
	}

	// right password -> cookie (tried before the wrong-password case so the
	// global backoff window does not shadow the happy path)
	login := postJSON(t, handler, "/api/auth/login", `{"password":"字password12345"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login = %d body=%s", login.Code, login.Body)
	}
	cookie := sessionCookie(login)
	if cookie == nil || cookie.Value == "" {
		t.Fatal("no session cookie issued")
	}
	if !cookie.HttpOnly || cookie.Path != "/" || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != int(sessionMaxAge.Seconds()) {
		t.Fatalf("cookie flags = HttpOnly:%v Path:%s SameSite:%v MaxAge:%d", cookie.HttpOnly, cookie.Path, cookie.SameSite, cookie.MaxAge)
	}
	keyInfo, err := os.Stat(filepath.Join(fixture.home, "keys", sessionKeyFilename))
	if err != nil {
		t.Fatal(err)
	}
	if keyInfo.Mode().Perm() != 0o600 {
		t.Fatalf("session key mode = %v", keyInfo.Mode().Perm())
	}
	keyBytes, err := os.ReadFile(filepath.Join(fixture.home, "keys", sessionKeyFilename))
	if err != nil || len(keyBytes) != sessionKeySize {
		t.Fatalf("session key len = %d err=%v", len(keyBytes), err)
	}

	// wrong password is rejected (no lockout of the correct one)
	wrong := postJSON(t, handler, "/api/auth/login", `{"password":"wrong-password-99"}`)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", wrong.Code)
	}

	// cookie authorizes /api/status
	authed := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	authed.AddCookie(cookie)
	authedResponse := httptest.NewRecorder()
	handler.ServeHTTP(authedResponse, authed)
	if authedResponse.Code != http.StatusOK {
		t.Fatalf("status with session = %d body=%s", authedResponse.Code, authedResponse.Body)
	}
	if extra := sessionCookie(authedResponse); extra != nil {
		t.Fatalf("fresh session was reissued, MaxAge=%d", extra.MaxAge)
	}

	// Bearer token still works in parallel (agent contract unchanged)
	if response := requestWithToken(t, handler, http.MethodGet, "/api/status", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("empty bearer = %d", response.Code)
	}

	// logout clears the browser cookie and does not revoke a copy
	logout := postJSON(t, handler, "/api/auth/logout", "{}", cookie)
	if logout.Code != http.StatusOK {
		t.Fatalf("logout = %d", logout.Code)
	}
	cleared := sessionCookie(logout)
	if cleared == nil || cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Fatalf("logout cookie = %#v", cleared)
	}
	after := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	after.AddCookie(cookie)
	afterResponse := httptest.NewRecorder()
	handler.ServeHTTP(afterResponse, after)
	if afterResponse.Code != http.StatusOK {
		t.Fatalf("copied cookie after logout = %d", afterResponse.Code)
	}
}

// Rule 3: login backoff — repeated failures slow down (global, not per-IP).
func TestLoginBackoff(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	handler := server.Handler()
	// set a password directly through the API first
	setup := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"password":"字password12345"}`))
	setup.Header.Set("Content-Type", "application/json")
	setup.RemoteAddr = "127.0.0.1:5555"
	setup.Host = "127.0.0.1:7760"
	handler.ServeHTTP(httptest.NewRecorder(), setup)

	start := time.Now()
	for i := 0; i < 3; i++ {
		postJSON(t, handler, "/api/auth/login", `{"password":"wrong-password-99"}`)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("3 failed logins took %v (no backoff)", elapsed)
	}
	// correct password still succeeds once the backoff window passes (no
	// hard lockout) — the third failure set a 4s window, so wait it out.
	time.Sleep(4 * time.Second)
	ok := postJSON(t, handler, "/api/auth/login", `{"password":"字password12345"}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("login after failures = %d (hard-locked?)", ok.Code)
	}
}

// Rule 5: join command is only rendered for authenticated callers.
func TestJoinCommandRequiresAuth(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "hub-token-x", nil, nil)
	handler := server.Handler()
	if response := request(t, handler, http.MethodGet, "/api/auth/join"); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated join = %d", response.Code)
	}
	if response := requestWithToken(t, handler, http.MethodGet, "/api/auth/join", "hub-token-x"); response.Code != http.StatusOK {
		t.Fatalf("bearer join = %d body=%s", response.Code, response.Body)
	}
	joined := requestWithToken(t, handler, http.MethodGet, "/api/auth/join", "hub-token-x")
	if !strings.Contains(joined.Body.String(), "curl -fsSL http://example.com/install.sh | sh -s -- --token hub-token-x") {
		t.Fatal("join response missing the install-pipe command")
	}
	var payload map[string]any
	if err := json.Unmarshal(joined.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["listenCommand"]; exists {
		t.Fatalf("removed listen command is still exposed: %s", joined.Body)
	}
	if payload["command"] == nil {
		t.Fatalf("join response has no command: %s", joined.Body)
	}
}

func TestJoinStatusTurnsInactiveWhenCodeIsUsed(t *testing.T) {
	fixture := authFixture(t)
	book := &joinCodeBook{live: map[string]bool{}}
	server, err := NewServer(ServeOptions{
		Addr:       "127.0.0.1:0",
		HomerHome:  fixture.home,
		Token:      "hub-token-x",
		Enrollment: book,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	issued := requestWithToken(t, handler, http.MethodGet, "/api/auth/join", "hub-token-x")
	var joined map[string]any
	if issued.Code != http.StatusOK || json.Unmarshal(issued.Body.Bytes(), &joined) != nil || joined["code"] != "hr_once" {
		t.Fatalf("join = %d %s", issued.Code, issued.Body)
	}
	active := requestWithToken(t, handler, http.MethodGet, "/api/auth/join/status?code=hr_once", "hub-token-x")
	var activeBody map[string]any
	if active.Code != http.StatusOK || json.Unmarshal(active.Body.Bytes(), &activeBody) != nil || activeBody["active"] != true {
		t.Fatalf("active = %d %s", active.Code, active.Body)
	}
	delete(book.live, "hr_once")
	used := requestWithToken(t, handler, http.MethodGet, "/api/auth/join/status?code=hr_once", "hub-token-x")
	var usedBody map[string]any
	if used.Code != http.StatusOK || json.Unmarshal(used.Body.Bytes(), &usedBody) != nil || usedBody["active"] != false {
		t.Fatalf("used = %d %s", used.Code, used.Body)
	}
	if response := request(t, handler, http.MethodGet, "/api/auth/join/status?code=hr_once"); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", response.Code)
	}
}

type joinCodeBook struct {
	live map[string]bool
}

func (b *joinCodeBook) Mint(time.Duration) (string, error) {
	b.live["hr_once"] = true
	return "hr_once", nil
}

func (b *joinCodeBook) ValidCode(code string) bool { return b.live[code] }

func (b *joinCodeBook) Revoke(string) bool { return false }

// Deleting the administrator password returns the hub to first-run and
// rejects session cookies. Replacing the file does not.
func TestPasswordRemovalDropsSession(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	handler := server.Handler()
	setup := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"password":"字password12345"}`))
	setup.Header.Set("Content-Type", "application/json")
	setup.RemoteAddr = "127.0.0.1:5555"
	setup.Host = "127.0.0.1:7760"
	handler.ServeHTTP(httptest.NewRecorder(), setup)
	login := postJSON(t, handler, "/api/auth/login", `{"password":"字password12345"}`)
	cookie := sessionCookie(login)
	if cookie == nil {
		t.Fatal("no session")
	}
	if err := os.Remove(filepath.Join(fixture.home, "keys", "hub-password")); err != nil {
		t.Fatal(err)
	}
	probed := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	probed.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, probed)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("session survived password removal = %d", response.Code)
	}
}

func TestPasswordReplacementKeepsSession(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	handler := server.Handler()
	cookie := setupAndLogin(t, handler)
	if err := os.WriteFile(filepath.Join(fixture.home, "keys", "hub-password"), []byte("replaced-password-hash"), 0o600); err != nil {
		t.Fatal(err)
	}
	probed := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	probed.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, probed)
	if response.Code != http.StatusOK {
		t.Fatalf("session died after password replacement = %d", response.Code)
	}
}

func TestSessionSurvivesRestart(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	cookie := setupAndLogin(t, server.Handler())
	restarted := newWebServer(t, fixture, "", nil, nil)
	probed := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	probed.AddCookie(cookie)
	response := httptest.NewRecorder()
	restarted.Handler().ServeHTTP(response, probed)
	if response.Code != http.StatusOK {
		t.Fatalf("session died across serve restart = %d body=%s", response.Code, response.Body)
	}
}

func TestSessionSlidesWhenHalfElapsed(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	handler := server.Handler()
	_ = setupAndLogin(t, handler)
	cookie := signedSessionCookie(t, fixture.home, time.Now().Add(sessionMaxAge/2-time.Second))
	probed := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	probed.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, probed)
	if response.Code != http.StatusOK {
		t.Fatalf("half-elapsed session = %d", response.Code)
	}
	refreshed := sessionCookie(response)
	if refreshed == nil || refreshed.MaxAge != int(sessionMaxAge.Seconds()) || !refreshed.HttpOnly {
		t.Fatalf("refreshed cookie = %#v", refreshed)
	}
	again := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	again.AddCookie(refreshed)
	againResponse := httptest.NewRecorder()
	handler.ServeHTTP(againResponse, again)
	if againResponse.Code != http.StatusOK {
		t.Fatalf("refreshed session = %d", againResponse.Code)
	}
	if extra := sessionCookie(againResponse); extra != nil {
		t.Fatal("just-refreshed session was reissued again")
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	handler := server.Handler()
	_ = setupAndLogin(t, handler)
	cookie := signedSessionCookie(t, fixture.home, time.Now().Add(-time.Minute))
	probed := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	probed.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, probed)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expired session = %d", response.Code)
	}
}

func TestTamperedSessionRejected(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	handler := server.Handler()
	cookie := setupAndLogin(t, handler)
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil || len(raw) == 0 {
		t.Fatalf("cookie decode: %v len=%d", err, len(raw))
	}
	raw[len(raw)-1] ^= 0xff
	cookie.Value = base64.RawURLEncoding.EncodeToString(raw)
	probed := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	probed.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, probed)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("tampered session = %d", response.Code)
	}
}

func setupAndLogin(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	setup := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"password":"字password12345"}`))
	setup.Header.Set("Content-Type", "application/json")
	setup.RemoteAddr = "127.0.0.1:5555"
	setup.Host = "127.0.0.1:7760"
	setupResponse := httptest.NewRecorder()
	handler.ServeHTTP(setupResponse, setup)
	if setupResponse.Code != http.StatusOK {
		t.Fatalf("setup = %d body=%s", setupResponse.Code, setupResponse.Body)
	}
	login := postJSON(t, handler, "/api/auth/login", `{"password":"字password12345"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login = %d body=%s", login.Code, login.Body)
	}
	cookie := sessionCookie(login)
	if cookie == nil || cookie.Value == "" {
		t.Fatal("no session cookie")
	}
	return cookie
}

func signedSessionCookie(t *testing.T, home string, expiry time.Time) *http.Cookie {
	t.Helper()
	key, err := os.ReadFile(filepath.Join(home, "keys", sessionKeyFilename))
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: signSession(key, expiry)}
}

// One-time setup code: public-internet first-run initialization. Trusted
// peers (loopback/LAN) skip the code; public peers must present it. The
// code regenerates on serve restart and burns after one use.
func TestSetupCodeFlows(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	handler := server.Handler()

	// Public peer without a code is rejected even with a valid password.
	public := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"password":"字password12345"}`))
	public.Header.Set("Content-Type", "application/json")
	public.RemoteAddr = "203.0.113.9:5555"
	public.Host = "homerhw.openaaas.org"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, public)
	if response.Code != http.StatusForbidden {
		t.Fatalf("public no-code setup = %d", response.Code)
	}

	// The setup code is derived deterministically per boot; expose it for
	// tests via the authStore.
	code := server.SetupCode()
	if code == "" || len(code) < 6 {
		t.Fatalf("setup code = %q", code)
	}

	// Public peer WITH the correct code succeeds.
	withCode := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"password":"字password12345","setupCode":"`+code+`"}`))
	withCode.Header.Set("Content-Type", "application/json")
	withCode.RemoteAddr = "203.0.113.9:5555"
	withCode.Host = "homerhw.openaaas.org"
	withCodeResponse := httptest.NewRecorder()
	handler.ServeHTTP(withCodeResponse, withCode)
	if withCodeResponse.Code != http.StatusOK {
		t.Fatalf("public with-code setup = %d body=%s", withCodeResponse.Code, withCodeResponse.Body)
	}

	// The code burns after use: a second public setup (password reset by
	// deleting the file) is refused even with the same code.
	if err := os.Remove(filepath.Join(fixture.home, "keys", "hub-password")); err != nil {
		t.Fatal(err)
	}
	burned := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"password":"another-password-123","setupCode":"`+code+`"}`))
	burned.Header.Set("Content-Type", "application/json")
	burned.RemoteAddr = "203.0.113.9:5555"
	burned.Host = "homerhw.openaaas.org"
	burnedResponse := httptest.NewRecorder()
	handler.ServeHTTP(burnedResponse, burned)
	if burnedResponse.Code != http.StatusForbidden {
		t.Fatalf("burned code reused = %d", burnedResponse.Code)
	}

	// Wrong code on a fresh server is rejected.
	fresh := newWebServer(t, fixture, "", nil, nil)
	wrong := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"password":"another-password-123","setupCode":"WRONG99"}`))
	wrong.Header.Set("Content-Type", "application/json")
	wrong.RemoteAddr = "203.0.113.9:5555"
	wrong.Host = "homerhw.openaaas.org"
	wrongResponse := httptest.NewRecorder()
	fresh.Handler().ServeHTTP(wrongResponse, wrong)
	if wrongResponse.Code != http.StatusForbidden {
		t.Fatalf("wrong code = %d", wrongResponse.Code)
	}
}

// Loopback peers never need the code (zero-friction local first-run).
func TestSetupCodeNotRequiredForLoopback(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	// no setupCode in the payload at all
	local := setupFromLoopback(t, server.Handler(), `{"password":"字password12345"}`)
	if local.Code != http.StatusOK {
		t.Fatalf("loopback setup without code = %d body=%s", local.Code, local.Body)
	}
}

// The status endpoint tells the browser up-front whether this peer needs
// the one-time code — the code field must be visible on first paint for
// public peers, never after a rejected submit.
func TestAuthStatusRevealsCodeRequirement(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	handler := server.Handler()

	local := httptest.NewRequest(http.MethodGet, "/api/auth/status", nil)
	local.RemoteAddr = "127.0.0.1:5555"
	local.Host = "127.0.0.1:7760"
	localResponse := httptest.NewRecorder()
	handler.ServeHTTP(localResponse, local)
	if !strings.Contains(localResponse.Body.String(), `"setupCodeRequired":false`) {
		t.Fatalf("loopback status = %s", localResponse.Body)
	}

	public := httptest.NewRequest(http.MethodGet, "/api/auth/status", nil)
	public.RemoteAddr = "203.0.113.9:5555"
	public.Host = "homerhw.openaaas.org"
	publicResponse := httptest.NewRecorder()
	handler.ServeHTTP(publicResponse, public)
	if !strings.Contains(publicResponse.Body.String(), `"setupCodeRequired":true`) {
		t.Fatalf("public status = %s", publicResponse.Body)
	}
}

// A weak password must NOT burn the one-time code: cheap validations run
// before the single-shot credential is consumed (deploying restarted serve
// once wasted a code this way during the live incident).
func TestWeakPasswordDoesNotBurnCode(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "", nil, nil)
	handler := server.Handler()
	code := server.SetupCode()

	// Public peer, weak password, correct code -> 400, code still alive.
	weak := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"password":"short","setupCode":"`+code+`"}`))
	weak.Header.Set("Content-Type", "application/json")
	weak.RemoteAddr = "203.0.113.9:5555"
	weak.Host = "homerhw.openaaas.org"
	weakResponse := httptest.NewRecorder()
	handler.ServeHTTP(weakResponse, weak)
	if weakResponse.Code != http.StatusBadRequest {
		t.Fatalf("weak password = %d", weakResponse.Code)
	}

	// Same code still works with a valid password.
	good := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"password":"字password12345","setupCode":"`+code+`"}`))
	good.Header.Set("Content-Type", "application/json")
	good.RemoteAddr = "203.0.113.9:5555"
	good.Host = "homerhw.openaaas.org"
	goodResponse := httptest.NewRecorder()
	handler.ServeHTTP(goodResponse, good)
	if goodResponse.Code != http.StatusOK {
		t.Fatalf("valid retry after weak password = %d body=%s", goodResponse.Code, goodResponse.Body)
	}
}

// Tailscale-style bootstrap: /install.sh is public (token arrives as an
// argument, never embedded); /dl/homer (the hub's own binary) requires the
// hub token — the binary itself is not public infrastructure.
func TestInstallEndpoints(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "hub-token-x", nil, nil)
	handler := server.Handler()

	script := request(t, handler, http.MethodGet, "/install.sh")
	if script.Code != http.StatusOK {
		t.Fatalf("install.sh = %d", script.Code)
	}
	if ct := script.Header().Get("Content-Type"); ct != "text/x-shellscript; charset=utf-8" {
		t.Fatalf("install.sh content-type = %q", ct)
	}

	// /dl/homer without a token: 401.
	if response := request(t, handler, http.MethodGet, "/dl/homer"); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dl = %d", response.Code)
	}
}

// Machine removal (console 「移除」): drops the registry entry entirely —
// vs. revocation which only kills the credential. Requires auth.
func TestAgentRemoveEndpoint(t *testing.T) {
	fixture := authFixture(t)
	source := &sourceStub{list: []AgentInfo{{AgentID: "box", Hostname: "box"}}}
	server := newWebServer(t, fixture, "hub-token-x", source, nil)
	handler := server.Handler()

	// Unauthenticated: 401.
	if response := request(t, handler, http.MethodPost, "/api/agents/remove"); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated remove = %d", response.Code)
	}
	// Authenticated: removes and the list no longer contains the machine.
	removeBox := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/agents/remove", strings.NewReader(`{"agentId":"box"}`))
		req.Header.Set("Authorization", "Bearer hub-token-x")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	response := removeBox()
	if response.Code != http.StatusOK {
		t.Fatalf("remove = %d body=%s", response.Code, response.Body)
	}
	list := requestWithToken(t, handler, http.MethodGet, "/api/agents", "hub-token-x")
	if strings.Contains(list.Body.String(), `"box"`) {
		t.Fatalf("removed agent still listed: %s", list.Body)
	}
	// Removing an unknown agent: 404.
	if response := removeBox(); response.Code != http.StatusNotFound {
		t.Fatalf("remove unknown = %d", response.Code)
	}
}
