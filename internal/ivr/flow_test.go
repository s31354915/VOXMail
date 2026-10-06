package ivr

import "testing"

func TestBackTracksOneState(t *testing.T) {
	s := NewSession("c1")
	s.Enter(StatePIN)
	s.Enter(StateMain)
	if err := s.Back(); err != nil || s.State != StatePIN {
		t.Fatalf("back gave state=%s err=%v", s.State, err)
	}
}

func TestPINLocksAfterThreeFailures(t *testing.T) {
	s := NewSession("c1")
	for i := 0; i < 3; i++ {
		if closed := s.FailPIN(); closed != (i == 2) {
			t.Fatal("unexpected lock behavior")
		}
	}
	if s.State != StateClosed {
		t.Fatal("session was not closed")
	}
}

func TestHistoryIsBoundedAndCloseRevokesAuthentication(t *testing.T) {
	s := NewSession("c1")
	s.HistoryLimit = 2
	s.Authenticated = true
	for _, state := range []State{StatePIN, StateMain, StateUnread} {
		s.Enter(state)
	}
	if len(s.Previous) != 2 || s.Previous[0] != StatePIN || s.Previous[1] != StateMain {
		t.Fatalf("history=%v, want the newest two states", s.Previous)
	}
	s.Close()
	if s.State != StateClosed || s.Authenticated || len(s.Previous) != 0 {
		t.Fatalf("closed session=%+v, want terminal unauthenticated state", s)
	}
	s.Close()
	if s.State != StateClosed || s.Authenticated {
		t.Fatal("second close was not idempotent")
	}
}
