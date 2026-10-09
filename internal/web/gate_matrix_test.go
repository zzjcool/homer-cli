package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type gateCredential struct {
	name   string
	token  string
	cookie bool
}

func TestGateMatrix(t *testing.T) {
	fixture := authFixture(t)
	enrollment := &matrixEnrollmentService{codes: map[string]bool{"hr_live": true}}
	var forwardedPath string
	server, err := NewServer(ServeOptions{
		Addr:       "127.0.0.1:0",
		HomerHome:  fixture.home,
		Token:      "hub-token",
		Enrollment: enrollment,
		AgentEndpointAuthorized: func(r *http.Request) bool {
			return r.Header.Get("Authorization") == "Bearer agent-secret-live"
		},
		AgentEndpoint: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forwardedPath = r.URL.Path
			switch r.URL.Path {
			case "/agent/v1/stream":
				token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
				if token == "hub-token" || token == "agent-secret-live" || enrollment.ValidCode(token) {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
			case "/agent/v1/enroll", "/agent/v1/register", "/agent/v1/poll", "/agent/v1/report":
				w.WriteHeader(http.StatusGone)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := setupAndLogin(t, server.Handler())

	credentials := []gateCredential{
		{name: "none"},
		{name: "cookie", cookie: true},
		{name: "hub token", token: "hub-token"},
		{name: "unused enrollment code", token: "hr_live"},
		{name: "burned or expired enrollment code", token: "hr_burned"},
		{name: "per-agent secret", token: "agent-secret-live"},
		{name: "revoked per-agent secret", token: "agent-secret-revoked"},
	}
	type gatePath struct {
		name   string
		method string
		path   string
		body   string
		want   func(gateCredential) int
	}
	paths := []gatePath{
		{
			name: "GET /agent/v1/stream (web bypass; endpoint authenticates)",
			path: "/agent/v1/stream",
			want: func(credential gateCredential) int {
				switch credential.name {
				case "hub token", "unused enrollment code", "per-agent secret":
					return http.StatusOK
				default:
					return http.StatusUnauthorized
				}
			},
		},
		{
			name: "GET /agent/v1/enroll tombstone",
			path: "/agent/v1/enroll",
			want: func(gateCredential) int { return http.StatusGone },
		},
		{
			name: "GET /agent/v1/register tombstone",
			path: "/agent/v1/register",
			want: func(gateCredential) int { return http.StatusGone },
		},
		{
			name: "GET /agent/v1/poll tombstone",
			path: "/agent/v1/poll",
			want: func(gateCredential) int { return http.StatusGone },
		},
		{
			name: "GET /agent/v1/report tombstone",
			path: "/agent/v1/report",
			want: func(gateCredential) int { return http.StatusGone },
		},
		{
			name: "other /agent/*",
			path: "/agent/v1/other",
			want: func(gateCredential) int { return http.StatusNotFound },
		},
		{
			name: "/api/* (KNOWN-BROAD)",
			path: "/api/console",
			want: func(credential gateCredential) int {
				if gateAllowsHumanAPI(credential) {
					return http.StatusOK
				}
				return http.StatusUnauthorized
			},
		},
		{
			name:   "GET /api/resolve/record",
			method: http.MethodGet,
			path:   "/api/resolve/record?agent=box",
			want: func(credential gateCredential) int {
				if gateAllowsHumanAPI(credential) {
					return http.StatusNotImplemented
				}
				return http.StatusUnauthorized
			},
		},
		{
			name:   "POST /api/resolve/clear",
			method: http.MethodPost,
			path:   "/api/resolve/clear?agent=box&confirm=true",
			body:   `{"adapters":["pi"]}`,
			want: func(credential gateCredential) int {
				if gateAllowsHumanAPI(credential) {
					return http.StatusNotImplemented
				}
				return http.StatusUnauthorized
			},
		},
		{
			name: "GET /api/snapshot",
			path: "/api/snapshot",
			want: func(credential gateCredential) int {
				if gateAllowsHumanAPI(credential) {
					return http.StatusConflict // authenticated, but no generation exists yet
				}
				return http.StatusUnauthorized
			},
		},
		{
			name: "GET /dl/homer",
			path: "/dl/homer",
			want: func(credential gateCredential) int {
				if gateAllowsHumanAPI(credential) {
					return http.StatusPartialContent
				}
				return http.StatusUnauthorized
			},
		},
		{
			name: "GET /dl/homer.gz",
			path: "/dl/homer.gz",
			want: func(credential gateCredential) int {
				if gateAllowsHumanAPI(credential) {
					return http.StatusPartialContent
				}
				return http.StatusUnauthorized
			},
		},
		{
			name: "public /install.sh",
			path: "/install.sh",
			want: func(gateCredential) int { return http.StatusOK },
		},
	}

	for _, path := range paths {
		path := path
		t.Run(path.name, func(t *testing.T) {
			for _, credential := range credentials {
				credential := credential
				t.Run(credential.name, func(t *testing.T) {
					forwardedPath = ""
					method := path.method
					if method == "" {
						method = http.MethodGet
					}
					request := httptest.NewRequest(method, path.path, strings.NewReader(path.body))
					if credential.token != "" {
						request.Header.Set("Authorization", "Bearer "+credential.token)
					}
					if credential.cookie {
						request.AddCookie(cookie)
					}
					if strings.HasPrefix(path.path, "/dl/") {
						request.Header.Set("Range", "bytes=0-0")
					}
					response := httptest.NewRecorder()
					server.Handler().ServeHTTP(response, request)
					if response.Code != path.want(credential) {
						t.Fatalf("status = %d, want %d, body=%s", response.Code, path.want(credential), response.Body)
					}
					if strings.HasPrefix(path.path, "/agent/") && forwardedPath != path.path {
						t.Fatalf("path was not delegated to AgentEndpoint: got %q", forwardedPath)
					}
				})
			}
		})
	}
}

func TestResolveRecordRouteMethods(t *testing.T) {
	fixture := authFixture(t)
	server := newWebServer(t, fixture, "test-token", nil, nil)
	for _, testCase := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPut, path: "/api/resolve/record"},
		{method: http.MethodGet, path: "/api/resolve/clear"},
	} {
		response := request(t, server.Handler(), testCase.method, testCase.path)
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s = %d %s, want 405", testCase.method, testCase.path, response.Code, response.Body)
		}
	}
}

func gateAllowsHumanAPI(credential gateCredential) bool {
	if credential.cookie {
		return true
	}
	switch credential.name {
	case "hub token", "unused enrollment code", "per-agent secret":
		return true
	default:
		return false
	}
}

type matrixEnrollmentService struct {
	codes map[string]bool
}

func (s *matrixEnrollmentService) Mint(time.Duration) (string, error) {
	s.codes["hr_live"] = true
	return "hr_live", nil
}

func (*matrixEnrollmentService) Revoke(string) bool { return false }
func (s *matrixEnrollmentService) ValidCode(code string) bool {
	return s.codes[code]
}
