package web

import (
	"net/http"
	"strings"
	"time"
)

const (
	sessionMaxTotal   = 4096
	sessionMaxPerUser = 16
	sessionReapPeriod = time.Minute
	// Sessions live half a day and slide on use: an actively used session
	// is renewed so a long working session is not cut off mid-task.
	sessionLifetime = 12 * time.Hour
)

type sessionGrant struct {
	server *Server
	store  *SessionStore
	token  string
	csrf   string
	issued bool
}

// Reserve capacity before consuming a one-use security code. The token is
// undisclosed until commit; failed verification releases the reservation.
func (s *Server) reserveSession(w http.ResponseWriter, userID string) (*sessionGrant, bool) {
	token, csrf := newID(), newID()
	if !s.Sessions.Put(token, Session{UserID: userID, CSRF: csrf, Expires: s.Sessions.currentTime().Add(sessionLifetime)}) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "session limit reached; sign out of another session or try again later")
		return nil, false
	}
	return &sessionGrant{server: s, store: s.Sessions, token: token, csrf: csrf}, true
}

func (g *sessionGrant) cancel() {
	if g != nil && !g.issued {
		g.store.Delete(g.token)
	}
}

func (g *sessionGrant) commit(w http.ResponseWriter, r *http.Request) bool {
	if _, exists := g.store.Get(g.token); !exists {
		writeError(w, http.StatusUnauthorized, "session revoked during authentication; sign in again")
		return false
	}
	g.issued = true
	secure := g.server.secureCookie(r)
	http.SetCookie(w, &http.Cookie{Name: "voxmail_session", Value: g.token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionLifetime / time.Second)})
	w.Header().Set("X-CSRF-Token", g.csrf)
	return true
}

// Rotate the user's sessions under one lock. Releasing their quota before
// allocating the replacement lets other concurrent logins steal that space.
func (s *Server) replaceUserSession(w http.ResponseWriter, r *http.Request, userID string) bool {
	token, csrf := newID(), newID()
	session := Session{UserID: userID, CSRF: csrf, Expires: s.Sessions.currentTime().Add(sessionLifetime)}
	if !s.Sessions.replaceUser(token, session) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "session limit reached; sign out of another session or try again later")
		return false
	}
	grant := &sessionGrant{server: s, store: s.Sessions, token: token, csrf: csrf}
	return grant.commit(w, r)
}

// secureCookie mirrors the login cookie's transport rules for refreshed cookies.
func (s *Server) secureCookie(r *http.Request) bool {
	return r.TLS != nil || (s.trustedProxy(r) && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https"))
}

func (s *SessionStore) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Put rejects overflow rather than evicting other users' active sessions.
func (s *SessionStore) Put(token string, session Session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	old, exists := s.byToken[token]
	userCount := s.byUser[session.UserID]
	if exists && old.UserID == session.UserID {
		userCount--
	}
	if (!exists && len(s.byToken) >= sessionMaxTotal) || userCount >= sessionMaxPerUser {
		return false
	}
	if exists {
		s.deleteLocked(token)
	}
	s.byToken[token] = session
	s.byUser[session.UserID]++
	return true
}

func (s *SessionStore) Get(token string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.byToken[token]
	if !ok || !s.currentTime().Before(session.Expires) {
		s.deleteLocked(token)
		return Session{}, false
	}
	s.reapLocked()
	return session, true
}

// GetRenew returns the live session for the token, sliding its expiry forward
// when more than half of the session lifetime has elapsed so an actively used
// session is not cut off mid-task. The second result reports a renewal.
func (s *SessionStore) GetRenew(token string) (Session, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.byToken[token]
	if !ok || !s.currentTime().Before(session.Expires) {
		s.deleteLocked(token)
		return Session{}, false, false
	}
	renewed := false
	if s.currentTime().Add(sessionLifetime / 2).After(session.Expires) {
		session.Expires = s.currentTime().Add(sessionLifetime)
		s.byToken[token] = session
		renewed = true
	}
	s.reapLocked()
	return session, renewed, true
}

func (s *SessionStore) deleteLocked(token string) {
	if session, ok := s.byToken[token]; ok {
		delete(s.byToken, token)
		s.byUser[session.UserID]--
		if s.byUser[session.UserID] == 0 {
			delete(s.byUser, session.UserID)
		}
	}
}

func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteLocked(token)
}

func (s *SessionStore) DeleteUser(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, session := range s.byToken {
		if session.UserID == userID {
			s.deleteLocked(token)
		}
	}
}

func (s *SessionStore) replaceUser(token string, session Session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	if len(s.byToken)-s.byUser[session.UserID] >= sessionMaxTotal {
		return false
	}
	for existing, old := range s.byToken {
		if old.UserID == session.UserID {
			s.deleteLocked(existing)
		}
	}
	// Token generation is private; account for any collision nevertheless.
	s.deleteLocked(token)
	s.byToken[token] = session
	s.byUser[session.UserID]++
	return true
}

func (s *SessionStore) reapLocked() {
	now := s.currentTime()
	if now.Before(s.lastReap.Add(sessionReapPeriod)) {
		return
	}
	s.lastReap = now
	for token, session := range s.byToken {
		if !now.Before(session.Expires) {
			s.deleteLocked(token)
		}
	}
}
