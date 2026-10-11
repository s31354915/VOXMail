package web

import (
	"net"
	"net/http"
	"strings"

	"github.com/voxmail/voxmail/internal/store"
)

// requireOnline is the manual-operation half of the offline contract. Worker
// suppression alone must not allow HTTP requests to dial restored providers.
func (s *Server) requireOnline(w http.ResponseWriter, operation string) bool {
	if s.OfflineMode {
		writeError(w, http.StatusServiceUnavailable, operation+" is unavailable in offline mode")
		return false
	}
	return true
}

func (s *Server) shuttingDown() bool {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	return s.closing
}

func (s *Server) withAdmission(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.admissionMu.Lock()
		if s.closing {
			s.admissionMu.Unlock()
			// These two handlers do not touch external/shared dependencies when
			// closing and therefore cannot extend the request join indefinitely.
			if r.URL.Path == "/healthz" {
				s.health(w, r)
			} else if r.URL.Path == "/readyz" {
				s.ready(w, r)
			} else {
				writeError(w, http.StatusServiceUnavailable, "server is shutting down")
			}
			return
		}
		s.requestWG.Add(1)
		s.admissionMu.Unlock()
		defer s.requestWG.Done()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) require(w http.ResponseWriter, r *http.Request, write bool) (store.User, bool) {
	token := sessionToken(r)
	session, ok := s.Sessions.Get(token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, false
	}
	if write && r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-CSRF-Token") != session.CSRF {
		writeError(w, http.StatusForbidden, "csrf token required")
		return store.User{}, false
	}
	user, err := s.Store.UserByID(r.Context(), session.UserID)
	if err != nil || !user.Enabled {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, false
	}
	return user, true
}
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	u, ok := s.require(w, r, r.Method != "GET")
	if !ok {
		return u, false
	}
	if u.Role != "admin" {
		writeError(w, http.StatusForbidden, "administrator access required")
		return store.User{}, false
	}
	return u, true
}

// requireAlerts keeps the alert API unavailable when the administrator has
// disabled the feature for the deployment. The separate admin availability
// endpoint remains usable so the administrator can turn it back on.
func (s *Server) requireAlerts(w http.ResponseWriter, r *http.Request, write bool) (store.User, bool) {
	u, ok := s.require(w, r, write)
	if !ok {
		return store.User{}, false
	}
	available, err := s.Store.AlertsAvailable(r.Context())
	if err != nil {
		serverError(w, err)
		return store.User{}, false
	}
	if !available {
		writeError(w, http.StatusNotFound, "alert controls are disabled by the administrator")
		return store.User{}, false
	}
	return u, true
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, userID string) bool {
	grant, ok := s.reserveSession(w, userID)
	if !ok {
		return false
	}
	return grant.commit(w, r)
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie("voxmail_session")
	if err != nil {
		return ""
	}
	return c.Value
}

// loginKeys is retained as the direct-connection helper used by unit tests.
// The live login path uses Server.clientHost so a configured reverse proxy can
// provide a stable client identity without trusting headers from direct peers.
func loginKeys(r *http.Request, username string) []string {
	host := remoteHost(r.RemoteAddr)
	return loginKeysForHost(host, username)
}

func loginKeysForHost(host, username string) []string {
	if host == "" {
		host = "unknown"
	}
	name := strings.ToLower(strings.TrimSpace(username))
	return []string{"source:" + host, "pair:" + host + "|" + name}
}

func remoteHost(remoteAddr string) string {
	host := strings.TrimSpace(remoteAddr)
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	return strings.TrimSpace(host)
}

func (s *Server) trustedProxyIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, raw := range s.TrustedProxyCIDRs {
		_, network, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
		if ip4 := ip.To4(); ip4 != nil && network.Contains(ip4) {
			return true
		}
	}
	return false
}

func (s *Server) trustedProxy(r *http.Request) bool {
	return s.trustedProxyIP(net.ParseIP(remoteHost(r.RemoteAddr)))
}

// clientHost trusts X-Forwarded-For only when the immediate peer belongs to a
// configured proxy network. It walks from the application outward and chooses
// the first untrusted address, preventing a direct client from spoofing the
// source used for login/recovery throttles.
func (s *Server) clientHost(r *http.Request) string {
	host := remoteHost(r.RemoteAddr)
	peer := net.ParseIP(host)
	if !s.trustedProxyIP(peer) {
		if host == "" {
			return "unknown"
		}
		return host
	}
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	leftmost := ""
	for i := len(forwarded) - 1; i >= 0; i-- {
		candidate := net.ParseIP(strings.TrimSpace(forwarded[i]))
		if candidate == nil {
			continue
		}
		leftmost = candidate.String()
		if !s.trustedProxyIP(candidate) {
			return candidate.String()
		}
	}
	if leftmost != "" {
		return leftmost
	}
	if host == "" {
		return "unknown"
	}
	return host
}

func publicUser(u store.User) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Username, "role": u.Role, "enabled": u.Enabled, "two_factor_enabled": u.TOTPSecret != ""}
}
