package web

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/store"
)

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

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, userID string) {
	token := newID()
	csrf := newID()
	s.Sessions.Put(token, Session{UserID: userID, CSRF: csrf, Expires: time.Now().Add(12 * time.Hour)})
	secure := r.TLS != nil || (s.trustedProxy(r) && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https"))
	http.SetCookie(w, &http.Cookie{Name: "voxmail_session", Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	w.Header().Set("X-CSRF-Token", csrf)
}
func sessionToken(r *http.Request) string {
	c, err := r.Cookie("voxmail_session")
	if err != nil {
		return ""
	}
	return c.Value
}
func (s *SessionStore) Put(token string, session Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byToken[token] = session
	s.reapLocked()
}
func (s *SessionStore) Get(token string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.byToken[token]
	if !ok || time.Now().After(session.Expires) {
		delete(s.byToken, token)
		return Session{}, false
	}
	s.reapLocked()
	return session, true
}
func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byToken, token)
}

func (s *SessionStore) DeleteUser(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, session := range s.byToken {
		if session.UserID == userID {
			delete(s.byToken, token)
		}
	}
}
func (s *SessionStore) reapLocked() {
	if len(s.byToken) < 256 || time.Since(s.lastReap) < time.Minute {
		return
	}
	s.lastReap = time.Now()
	now := time.Now()
	for token, session := range s.byToken {
		if now.After(session.Expires) {
			delete(s.byToken, token)
		}
	}
}

func (s *Server) loginBlocked(key string) time.Duration {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.reapLoginsLocked()
	state := s.logins[key]
	if time.Now().Before(state.Until) {
		return time.Until(state.Until)
	}
	return 0
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
	return []string{host + "|*", host + "|" + name}
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

func (s *Server) loginFailure(key string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.reapLoginsLocked()
	if s.logins == nil {
		s.logins = make(map[string]loginState)
	}
	state := s.logins[key]
	state.Failures++
	state.Seen = time.Now()
	if state.Failures >= 5 {
		state.Until = time.Now().Add(5 * time.Minute)
		state.Failures = 0
	}
	s.logins[key] = state
}

func (s *Server) loginSuccess(key string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	delete(s.logins, key)
}

func (s *Server) reapLoginsLocked() {
	if len(s.logins) < 256 {
		return
	}
	cutoff := time.Now().Add(-1 * time.Hour)
	for key, state := range s.logins {
		if state.Seen.Before(cutoff) && !time.Now().Before(state.Until) {
			delete(s.logins, key)
		}
	}
}
func publicUser(u store.User) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Username, "role": u.Role, "enabled": u.Enabled, "two_factor_enabled": u.TOTPSecret != ""}
}
