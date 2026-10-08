package hub

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

type PrincipalKind int

const (
	PrincipalSecret PrincipalKind = iota + 1
	PrincipalHubToken
	PrincipalEnrollCode
)

type Principal struct {
	Kind    PrincipalKind
	AgentID string // populated only for a verified per-agent secret
	Code    string // populated only for an enrollment code
}

type Authenticator struct {
	Token      string
	Enrollment *EnrollmentManager
}

// Authenticate validates an Authorization bearer. Enrollment codes are only
// checked here; burning one is deliberately reserved for the hello exchange.
func (a *Authenticator) Authenticate(bearer string) (Principal, bool) {
	if a == nil || bearer == "" {
		return Principal{}, false
	}
	if a.Token != "" && subtle.ConstantTimeCompare([]byte(a.Token), []byte(bearer)) == 1 {
		return Principal{Kind: PrincipalHubToken}, true
	}
	if a.Enrollment == nil {
		return Principal{}, false
	}
	if agentID := a.Enrollment.AuthorizedAgent(bearer); agentID != "" {
		return Principal{Kind: PrincipalSecret, AgentID: agentID}, true
	}
	if a.Enrollment.ValidCode(bearer) {
		return Principal{Kind: PrincipalEnrollCode, Code: bearer}, true
	}
	return Principal{}, false
}

// Authorized is the web data-plane gate. The one-time enrollment code can
// download bootstrap assets and open a stream, but it is not accepted for
// /api or /dl data access after this initial admission check.
func (a *Authenticator) Authorized(r *http.Request) bool {
	principal, ok := a.Authenticate(authBearerToken(r))
	return ok && (principal.Kind == PrincipalSecret || principal.Kind == PrincipalHubToken)
}

func authBearerToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(value, prefix))
}
