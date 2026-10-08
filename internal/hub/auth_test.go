package hub

import (
	"net/http"
	"testing"
	"time"
)

func TestAuthenticator(t *testing.T) {
	enrollment := NewEnrollmentManager()
	code, err := enrollment.Mint(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	secret := "agent-secret-a"
	enrollment.BindAgent("agent-a", secret)
	auth := &Authenticator{Token: "hub-token", Enrollment: enrollment}

	tests := []struct {
		name      string
		bearer    string
		wantOK    bool
		wantKind  PrincipalKind
		wantAgent string
		wantCode  string
	}{
		{name: "hub token", bearer: "hub-token", wantOK: true, wantKind: PrincipalHubToken},
		{name: "agent secret", bearer: secret, wantOK: true, wantKind: PrincipalSecret, wantAgent: "agent-a"},
		{name: "enrollment code", bearer: code, wantOK: true, wantKind: PrincipalEnrollCode, wantCode: code},
		{name: "empty", wantOK: false},
		{name: "wrong", bearer: "wrong-token", wantOK: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := auth.Authenticate(test.bearer)
			if ok != test.wantOK {
				t.Fatalf("Authenticate(%q) ok = %v, want %v", test.bearer, ok, test.wantOK)
			}
			if !ok {
				return
			}
			if got.Kind != test.wantKind || got.AgentID != test.wantAgent || got.Code != test.wantCode {
				t.Fatalf("Authenticate(%q) = %+v", test.bearer, got)
			}
		})
	}

	if !enrollment.ValidCode(code) {
		t.Fatal("Authenticate burned the enrollment code; only hello may redeem it")
	}

	requestWithBearer := func(bearer string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, "http://example.test/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		return r
	}
	for _, test := range []struct {
		name   string
		bearer string
		want   bool
	}{
		{name: "hub token", bearer: "hub-token", want: true},
		{name: "agent secret", bearer: secret, want: true},
		{name: "enrollment code is not authorized for data routes", bearer: code, want: false},
		{name: "invalid", bearer: "wrong", want: false},
	} {
		t.Run("authorized/"+test.name, func(t *testing.T) {
			if got := auth.Authorized(requestWithBearer(test.bearer)); got != test.want {
				t.Fatalf("Authorized(%q) = %v, want %v", test.bearer, got, test.want)
			}
		})
	}

	if !enrollment.Revoke("agent-a") {
		t.Fatal("Revoke(agent-a) = false")
	}
	if _, ok := auth.Authenticate(secret); ok {
		t.Fatal("revoked agent secret authenticated")
	}
	if auth.Authorized(requestWithBearer(secret)) {
		t.Fatal("revoked agent secret authorized a data route")
	}
}
