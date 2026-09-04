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
