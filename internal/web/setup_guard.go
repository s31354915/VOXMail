package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/auth"
)

const (
	setupHashConcurrency = 2
	setupAttemptLimit    = 5
	setupRateWindow      = 15 * time.Minute
	setupRateMaxSources  = 4096
)

type setupAttemptState struct {
	Started time.Time
	Count   int
}

func (s *Server) initSetupGuard() {
	s.setupOnce.Do(func() {
		s.setupHashSem = make(chan struct{}, setupHashConcurrency)
		s.setupAttempts = make(map[string]setupAttemptState)
	})
}

func (s *Server) hashSetupCredential(value string) (string, error) {
	if s.setupHash != nil {
		return s.setupHash(value)
	}
	return auth.Hash(value)
}

func (s *Server) rejectCompletedSetup(w http.ResponseWriter, r *http.Request) bool {
	if s.Store == nil {
		serverError(w, errors.New("store unavailable"))
		return true
	}
	completed, err := s.Store.SetupCompleted(r.Context())
	if err != nil {
		serverError(w, err)
		return true
	}
	if !completed {
		return false
	}
	writeError(w, http.StatusConflict, "setup has already been completed")
	return true
}

// reserveSetupAttempt limits first-run credential work by the trusted client
// address. The map has a hard ceiling so spoofed or distributed source
// cardinality cannot grow process memory without bound.
func (s *Server) reserveSetupAttempt(host string, now time.Time) time.Duration {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.setupAttempts == nil {
		s.setupAttempts = make(map[string]setupAttemptState)
	}
	if len(s.setupAttempts) >= 256 {
		for key, state := range s.setupAttempts {
			if !now.Before(state.Started.Add(setupRateWindow)) {
				delete(s.setupAttempts, key)
			}
		}
	}
	key := strings.TrimSpace(host)
	if key == "" {
		key = "unknown"
	}
	state, exists := s.setupAttempts[key]
	if exists && now.Before(state.Started.Add(setupRateWindow)) {
		if state.Count >= setupAttemptLimit {
			return state.Started.Add(setupRateWindow).Sub(now)
		}
	} else {
		if !exists && len(s.setupAttempts) >= setupRateMaxSources {
			return setupRateWindow
		}
		state = setupAttemptState{Started: now}
	}
	state.Count++
	s.setupAttempts[key] = state
	return 0
}

func writeSetupRateLimit(w http.ResponseWriter, retry time.Duration, message string) {
	seconds := int((retry + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests, message)
}
