package ivr

import "fmt"

const DefaultHistoryLimit = 64

type State string

const (
	StateWelcome State = "welcome"
	StatePIN     State = "pin"
	StateMain    State = "main"
	StateUnread  State = "unread"
	StateMessage State = "message"
	StateCompose State = "compose"
	StateReview  State = "review"
	StateClosed  State = "closed"
)

type Session struct {
	CallID        string
	UserID        string
	State         State
	Previous      []State
	PINFailures   int
	MaxPINTries   int
	Authenticated bool
	HistoryLimit  int
}

func NewSession(callID string) *Session {
	return &Session{CallID: callID, State: StateWelcome, MaxPINTries: 3, HistoryLimit: DefaultHistoryLimit}
}

func (s *Session) Enter(next State) {
	if s.State != next {
		s.Previous = append(s.Previous, s.State)
		limit := s.HistoryLimit
		if limit <= 0 {
			limit = DefaultHistoryLimit
			s.HistoryLimit = limit
		}
		if len(s.Previous) > limit {
			offset := len(s.Previous) - limit
			copy(s.Previous, s.Previous[offset:])
			s.Previous = s.Previous[:limit]
		}
		s.State = next
	}
}

func (s *Session) Back() error {
	if len(s.Previous) == 0 {
		return fmt.Errorf("already at root state")
	}
	last := len(s.Previous) - 1
	s.State, s.Previous = s.Previous[last], s.Previous[:last]
	return nil
}

func (s *Session) FailPIN() bool {
	s.PINFailures++
	if s.PINFailures >= s.MaxPINTries {
		s.State = StateClosed
		return true
	}
	return false
}

// Close makes a formal IVR session terminal and revokes its authenticated
// state. It is idempotent so shutdown, timeout, and credential revocation can
// share the same transition safely.
func (s *Session) Close() {
	if s == nil {
		return
	}
	s.State = StateClosed
	s.Authenticated = false
	s.Previous = nil
}
