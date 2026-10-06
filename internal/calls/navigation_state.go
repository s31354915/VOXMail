package calls

import "github.com/voxmail/voxmail/internal/ivr"

func backByContractLocked(sess *session, fallback string) {
	if sess == nil {
		return
	}
	if back, ok := ivr.Back(stateLocked(sess)); ok {
		if sess.Flow != nil {
			sess.Flow.State = back
		}
		sess.State = string(back)
		return
	}
	if sess.Flow != nil {
		sess.Flow.State = ivr.State(fallback)
	}
	sess.State = fallback
}

// ensureFlowLocked keeps sessions created by older callers and tests
// compatible while giving live calls one formal navigation history.
func ensureFlowLocked(sess *session) {
	if sess == nil {
		return
	}
	if sess.Flow == nil {
		sess.Flow = ivr.NewSession(sess.CallID)
		if sess.State != "" {
			sess.Flow.State = ivr.State(sess.State)
		}
	}
	// Flow.State is the authoritative typed state. State is retained as a
	// compatibility mirror for older tests and diagnostics, never as an input
	// once the formal flow exists.
	sess.State = string(sess.Flow.State)
}

// stateLocked returns the one authoritative state used by call handlers.
// Callers hold Service.mu (or the equivalent session ownership lock).
func stateLocked(sess *session) ivr.State {
	if sess == nil {
		return ""
	}
	ensureFlowLocked(sess)
	if sess.Flow == nil {
		return ivr.State(sess.State)
	}
	return sess.Flow.State
}

func backUsingFlowLocked(sess *session, fallback string) {
	if sess == nil {
		return
	}
	ensureFlowLocked(sess)
	if sess.Flow != nil {
		if err := sess.Flow.Back(); err == nil {
			sess.State = string(sess.Flow.State)
			return
		}
	}
	backByContractLocked(sess, fallback)
}

// transitionLocked is the only state-changing primitive used by call
// handlers. It keeps the legacy string used by the existing handlers in sync
// with the formal IVR session and records the previous state for #.
func transitionLocked(sess *session, next string) {
	if sess == nil || stateLocked(sess) == ivr.State(next) {
		return
	}
	ensureFlowLocked(sess)
	if sess.Flow != nil {
		sess.Flow.Enter(ivr.State(next))
	}
	sess.State = next
}
