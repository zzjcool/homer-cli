package web

import (
	"net"
	"net/http"
	"strings"
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
	if s.opts.Token == "" {
		return true
	}
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	return provided != "" && provided == s.opts.Token
}

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.authorized(r) {
		return true
	}
	writeError(w, http.StatusUnauthorized, "unauthorized", "未授权：请提供有效的 Bearer token", nil)
	return false
}
