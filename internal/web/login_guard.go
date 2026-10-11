package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/auth"
)

const (
	loginHashConcurrency    = 4
	loginAccountConcurrency = 2
	loginFailureLimit       = 5
	loginCooldown           = 5 * time.Minute
	loginMaxStates          = 4096
	loginStateLifetime      = time.Hour
	loginReapInterval       = time.Minute
	loginGlobalWindow       = time.Minute
	loginGlobalLimit        = 120
)

type loginAttempt struct {
	server   *Server
	states   []*loginState
	account  string
	finished bool
}

func (s *Server) loginTime() time.Time {
	if s.loginNow != nil {
		return s.loginNow()
	}
	return time.Now()
}

func (s *Server) checkLoginPassword(hash, password string) bool {
	if s.loginCheck != nil {
		return s.loginCheck(hash, password)
	}
	return auth.Check(hash, password)
}

// Reserve before any credential verification. Pending attempts count toward
// source budgets, so simultaneous requests cannot all pass a separate check.
// Accounts have only an in-flight cap: failed requests from another source
// never install an account-wide cooldown or evict a legitimate session.
func (s *Server) reserveLoginAttempt(host, username string) (*loginAttempt, time.Duration) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	now := s.loginTime()
	if s.logins == nil {
		s.logins = make(map[string]*loginState)
		s.loginAccounts = make(map[string]int)
	}
	if !now.Before(s.loginLastReap.Add(loginReapInterval)) {
		s.loginLastReap = now
		for key, state := range s.logins {
			if state.Pending == 0 && !now.Before(state.Seen.Add(loginStateLifetime)) && !now.Before(state.Until) {
				delete(s.logins, key)
			}
		}
	}
	if !now.Before(s.loginWindow.Add(loginGlobalWindow)) {
		s.loginWindow, s.loginTotal = now, 0
	}
	if s.loginTotal >= loginGlobalLimit {
		return nil, s.loginWindow.Add(loginGlobalWindow).Sub(now)
	}
	keys := loginKeysForHost(host, username)
	missing := 0
	for _, key := range keys {
		state := s.logins[key]
		if state == nil {
			missing++
			continue
		}
		if now.Before(state.Until) {
			return nil, state.Until.Sub(now)
		}
		if state.Failures+state.Pending >= loginFailureLimit {
			return nil, time.Second
		}
	}
	if len(s.logins)+missing > loginMaxStates {
		return nil, loginReapInterval
	}
	account := strings.ToLower(strings.TrimSpace(username))
	if s.loginActive >= loginHashConcurrency || s.loginAccounts[account] >= loginAccountConcurrency {
		return nil, time.Second
	}
	attempt := &loginAttempt{server: s, account: account}
	for _, key := range keys {
		state := s.logins[key]
		if state == nil {
			state = &loginState{}
			s.logins[key] = state
		}
		state.Pending++
		state.Seen = now
		attempt.states = append(attempt.states, state)
	}
	s.loginActive++
	s.loginAccounts[account]++
	s.loginTotal++
	return attempt, 0
}

func (a *loginAttempt) finish(valid bool) {
	s := a.server
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if a.finished {
		return
	}
	a.finished = true
	now := s.loginTime()
	for index, state := range a.states {
		state.Pending--
		state.Seen = now
		if valid {
			// A valid account can clear its own source/account failures,
			// but cannot reset the source's budget for other usernames.
			if index == 1 {
				state.Failures = 0
			}
			continue
		}
		state.Failures++
		if state.Failures >= loginFailureLimit {
			state.Until = now.Add(loginCooldown)
			state.Failures = 0
		}
	}
	s.loginActive--
	s.loginAccounts[a.account]--
	if s.loginAccounts[a.account] == 0 {
		delete(s.loginAccounts, a.account)
	}
}

func writeLoginRateLimit(w http.ResponseWriter, retry time.Duration) {
	seconds := int((retry + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests, "too many sign-in attempts; try again later")
}
