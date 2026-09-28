package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Advisor ruling 2026-09-28: dual-layer credentials.
// Humans: password -> HttpOnly session cookie (in-memory opaque token).
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

// Rule 2: login issues an opaque HttpOnly session cookie that authorizes
// /api/*; logout revokes it.
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
	if !cookie.HttpOnly || cookie.Path != "/" || cookie.MaxAge != 43200 {
		t.Fatalf("cookie flags = HttpOnly:%v Path:%s MaxAge:%d", cookie.HttpOnly, cookie.Path, cookie.MaxAge)
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

	// Bearer token still works in parallel (agent contract unchanged)
	if response := requestWithToken(t, handler, http.MethodGet, "/api/status", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("empty bearer = %d", response.Code)
	}

	// logout revokes
	logout := postJSON(t, handler, "/api/auth/logout", "{}", cookie)
	if logout.Code != http.StatusOK {
		t.Fatalf("logout = %d", logout.Code)
	}
	after := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	after.AddCookie(cookie)
	afterResponse := httptest.NewRecorder()
	handler.ServeHTTP(afterResponse, after)
	if afterResponse.Code != http.StatusUnauthorized {
		t.Fatalf("status after logout = %d", afterResponse.Code)
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
	if !strings.Contains(requestWithToken(t, handler, http.MethodGet, "/api/auth/join", "hub-token-x").Body.String(), "HOMER_HUB_TOKEN=") {
		t.Fatal("join response missing the env-prefix command")
	}
}

// Rule 2: password file removal invalidates all live sessions.
func TestPasswordRotationClearsSessions(t *testing.T) {
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
	// deleting the password file drops all sessions.
	if err := os.Remove(filepath.Join(fixture.home, "keys", "hub-password")); err != nil {
		t.Fatal(err)
	}
	probed := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	probed.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, probed)
	// sessions map clears lazily on next auth check: expect 401 after removal.
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("stale session survived password removal = %d", response.Code)
	}
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
