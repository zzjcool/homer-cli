package web

import (
	"net"
	"net/http"
	"strings"
	"time"
)

// isLoopbackAddr checks the address passed to net/http's listener. An empty
// host (":7760") is explicitly treated as the frozen local shorthand; a
// wildcard numeric address is not.
func isLoopbackAddr(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	host := addr
	if parsedHost, _, err := net.SplitHostPort(addr); err == nil {
		host = parsedHost
	} else if strings.HasPrefix(addr, "[") && strings.Contains(addr, "]") {
		end := strings.IndexByte(addr, ']')
		host = addr[1:end]
	} else if strings.Count(addr, ":") == 1 {
		// A host:port without a valid numeric port still has an unambiguous
		// host component for the startup safety check.
		host = addr[:strings.LastIndexByte(addr, ':')]
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) authorized(r *http.Request) bool {
	// Dual channel (advisor ruling): humans carry an HMAC-signed session
	// cookie minted by password login; agents and scripts keep the Bearer hub token.
	if s.auth != nil {
		if _, ok := s.auth.validSession(s.opts.HomerHome, r); ok {
			return true
		}
	}
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	if provided == "" {
		return false
	}
	// The shared hub token authorizes everything.
	if s.opts.Token != "" && provided == s.opts.Token {
		return true
	}
	// A live one-time enrollment code authorizes the binary download
	// (a fresh machine has nothing else). The code is only CHECKED
	// here, never burned — redemption stays strictly one-shot at
	// /agent/v1/enroll.
	if s.opts.Enrollment != nil && s.opts.Enrollment.ValidCode(provided) {
		return true
	}
	// An enrolled machine's per-agent secret authorizes its OWN upgrade
	// path (`homer upgrade`): keys/hub-token still holds the burned hr_
	// code from install time, so the machine presents agent.json's
	// secret instead. Delegated to the agent API's machine-credential
	// authority (the same check /agent/v1/* uses).
	if s.opts.AgentEndpointAuthorized != nil && s.opts.AgentEndpointAuthorized(r) {
		return true
	}
	// Rule 6: no more implicit loopback trust. Without any credential
	// configured the API stays locked (setup/login endpoints aside).
	return false
}

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.authorized(r) {
		s.slideSession(w, r)
		return true
	}
	writeError(w, http.StatusUnauthorized, "unauthorized", "未授权：请提供有效的 Bearer token", nil)
	return false
}

// slideSession extends an active browser session once less than half of the
// idle window remains. Bearer callers have nothing to extend.
func (s *Server) slideSession(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		return
	}
	expiry, ok := s.auth.validSession(s.opts.HomerHome, r)
	if !ok || time.Until(expiry) >= sessionMaxAge/2 {
		return
	}
	cookie, err := s.auth.issueSession(s.opts.HomerHome)
	if err != nil {
		return
	}
	http.SetCookie(w, cookie)
}
