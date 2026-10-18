package calls

import (
	"errors"
	"net"
	"time"

	"github.com/voxmail/voxmail/internal/bridge"
	"github.com/voxmail/voxmail/internal/ivr"
	"github.com/voxmail/voxmail/internal/lifecycle"
	"github.com/voxmail/voxmail/internal/phone"
)

func (s *Service) admit(conn net.Conn, message bridge.Message) error {
	if s.Store == nil {
		return s.sendBridge(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 500, Reason: "server unavailable"})
	}
	phone := phone.Normalize(message.From)
	if locked := s.pinLockedUntil(phone); !locked.IsZero() {
		return s.sendBridge(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 603, Reason: "caller is temporarily locked out"})
	}
	user, err := s.Store.UserByPhone(s.baseContext(), phone)
	if err != nil || !user.Enabled {
		return s.sendBridge(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 603, Reason: "caller not authorized"})
	}
	s.mu.Lock()
	/* Baresip can report a lifecycle event more than once. A call ID is the
	 * idempotency key: a duplicate call_incoming must not replace the
	 * admitted session, because doing so reset the greeting guard and
	 * re-spoke the welcome prompt. */
	if existing := s.sessions[message.CallID]; existing != nil && !existing.Closed {
		s.mu.Unlock()
		return nil
	}
	shuttingDown := s.shuttingDown
	busy := s.sipApplying || (s.MaxCalls > 0 && len(s.sessions) >= s.MaxCalls)
	if !busy && !shuttingDown {
		flow := ivr.NewSession(message.CallID)
		flow.State = ivr.StatePIN
		s.sessions[message.CallID] = &session{CallID: message.CallID, UserID: user.ID, Phone: phone, State: "pin", Flow: flow, TxPath: message.TxPath, RxPath: message.RxPath}
		s.armInactivityTimeoutLocked(s.sessions[message.CallID])
	}
	s.mu.Unlock()
	if shuttingDown {
		return s.sendBridge(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 503, Reason: "server is shutting down"})
	}
	if busy {
		return s.sendBridge(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 486, Reason: "all lines are busy"})
	}
	if err := s.sendBridge(conn, bridge.Message{Type: "answer", CallID: message.CallID, UserID: user.ID}); err != nil {
		return err
	}
	s.mu.Lock()
	// The session can disappear between the answer and this lookup (the call
	// closed, or the user's sessions were invalidated); guard the nil case
	// instead of panicking inside the bridge event loop.
	if sess := s.sessions[message.CallID]; sess != nil && s.Media != nil {
		sess.Runtime, sess.Lease, sess.EmailRuntime, sess.EmailLease = s.Media.ActivateForUserSpeeds(user.ID)
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) pinLockedUntil(phone string) time.Time {
	if phone == "" {
		return time.Time{}
	}
	now := time.Now()
	s.pinMu.Lock()
	until := time.Time{}
	if s.pinLock != nil {
		until = s.pinLock[phone]
	}
	if until.IsZero() {
		// An admitted caller was very recently confirmed unlocked; skip the
		// per-call store lookup instead of querying the database for every
		// incoming call.
		if checked, ok := s.pinNoLock[phone]; ok && now.Sub(checked) < 30*time.Second {
			s.pinMu.Unlock()
			return time.Time{}
		}
	}
	s.pinMu.Unlock()
	if until.After(now) {
		return until
	}
	if !until.IsZero() {
		s.pinMu.Lock()
		delete(s.pinLock, phone)
		s.pinMu.Unlock()
	}
	if s.Store != nil {
		cleanupCtx, cleanupCancel := lifecycle.CleanupContext(s.baseContext())
		persisted, err := s.Store.PinLockout(cleanupCtx, phone)
		cleanupCancel()
		if err == nil && persisted.After(now) {
			s.pinMu.Lock()
			if s.pinLock == nil {
				s.pinLock = make(map[string]time.Time)
			}
			s.pinLock[phone] = persisted
			s.pinMu.Unlock()
			return persisted
		}
		if err == nil {
			s.pinMu.Lock()
			if s.pinNoLock == nil {
				s.pinNoLock = make(map[string]time.Time)
			}
			s.pinNoLock[phone] = now
			s.pinMu.Unlock()
		}
	}
	return time.Time{}
}

func (s *Service) recordPinFailure(phone string) {
	if phone == "" {
		return
	}
	until := time.Now().Add(pinCooldown)
	s.pinMu.Lock()
	if s.pinLock == nil {
		s.pinLock = make(map[string]time.Time)
	}
	s.pinLock[phone] = until
	s.pinMu.Unlock()
	if s.Store != nil {
		cleanupCtx, cleanupCancel := lifecycle.CleanupContext(s.baseContext())
		_ = s.Store.SetPinLockout(cleanupCtx, phone, until)
		cleanupCancel()
	}
}

func (s *Service) clearPinFailure(phone string) {
	if phone == "" {
		return
	}
	s.pinMu.Lock()
	delete(s.pinLock, phone)
	s.pinMu.Unlock()
	if s.Store != nil {
		cleanupCtx, cleanupCancel := lifecycle.CleanupContext(s.baseContext())
		_ = s.Store.ClearPinLockout(cleanupCtx, phone)
		cleanupCancel()
	}
}

// startOutgoing turns a call_outgoing event into a session. The pending dial
// request supplies the callee's account and the text to read once the far end
// answers. Baresip rings until the remote phone picks up; an unanswered
// outgoing session is torn down after the call timeout.
func (s *Service) startOutgoing(conn net.Conn, message bridge.Message) error {
	s.mu.Lock()
	if message.CallID == "" {
		s.mu.Unlock()
		return errors.New("outgoing call ID is required")
	}
	/* Baresip can report a lifecycle event more than once during teardown.
	 * A call ID is the idempotency key: once it has a session, a duplicate
	 * call_outgoing must not consume another pending alert or replace the
	 * existing user's session. */
	if existing := s.sessions[message.CallID]; existing != nil {
		s.mu.Unlock()
		return nil
	}
	var req DialRequest
	if message.RequestID != "" && s.pending != nil {
		if candidate, ok := s.pending[message.RequestID]; ok {
			req = candidate
			delete(s.pending, message.RequestID)
		}
	}
	// Protocol-v1 shims did not carry request IDs. Keep a compatibility
	// fallback for a single outstanding request, but never silently choose from
	// several concurrent requests.
	if req.UserID == "" && message.RequestID == "" && len(s.pending) == 1 {
		for requestID, candidate := range s.pending {
			req = candidate
			delete(s.pending, requestID)
			break
		}
	}
	if req.UserID == "" {
		s.mu.Unlock()
		if s.Log != nil {
			s.Log.Warn("outgoing call without a pending request; hanging up", "call_id", message.CallID)
		}
		/* Late/duplicate events are rejected without creating a tombstone
		 * session. This keeps an unknown call from being associated with a
		 * future alert and makes the rejection itself idempotent. */
		return s.sendBridge(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 603, Reason: "unknown outgoing call"})
	}
	sess := &session{CallID: message.CallID, UserID: req.UserID, State: "outgoing", AlertText: req.Text, AlertDone: req.Done, AlertOutcome: req.Outcome, TxPath: message.TxPath, RxPath: message.RxPath}
	s.sessions[message.CallID] = sess
	if s.Media != nil && sess.UserID != "" {
		sess.Runtime, sess.Lease, sess.EmailRuntime, sess.EmailLease = s.Media.ActivateForUserSpeeds(sess.UserID)
	}
	s.mu.Unlock()
	if s.Log != nil {
		s.Log.Info("outgoing call session", "call_id", message.CallID, "user_id", sess.UserID, "tx_path", message.TxPath)
	}
	s.startDelayedTask(callTimeout, func() {
		s.mu.Lock()
		current := s.sessions[message.CallID]
		timeout := current == sess && stateLocked(sess) == "outgoing"
		s.mu.Unlock()
		if timeout {
			if s.Log != nil {
				s.Log.Warn("outgoing call timed out", "call_id", message.CallID)
			}
			s.reportAlertOutcome(sess, AlertFailed)
			s.notifyAlert(sess, false)
			_ = s.hangup(message.CallID)
		}
	})
	return nil
}

// playOutgoingAlert reads the alert text to the answered phone and then hangs
// up. It is the established-phase counterpart of greet for outbound calls.
func (s *Service) playOutgoingAlert(sess *session) {
	if sess == nil {
		return
	}
	s.mu.Lock()
	transitionLocked(sess, "announce")
	s.mu.Unlock()
	success := false
	if s.Media != nil && sess.AlertText != "" && sess.TxPath != "" {
		success = s.prompt(sess, sess.AlertText) == nil
	}
	if success {
		s.reportAlertOutcome(sess, AlertPlaybackCompleted)
	} else {
		s.reportAlertOutcome(sess, AlertFailed)
	}
	s.notifyAlert(sess, success)
	if err := s.hangup(sess.CallID); err != nil && s.Log != nil {
		s.Log.Warn("alert hangup failed", "call_id", sess.CallID, "error", err)
	}
}

func (s *Service) reportAlertOutcome(sess *session, outcome AlertOutcome) {
	if s == nil || sess == nil || sess.AlertOutcome == nil {
		return
	}
	if outcome == "" {
		return
	}
	rank := func(value AlertOutcome) int {
		switch value {
		case AlertAccepted:
			return 1
		case AlertRinging:
			return 2
		case AlertAnswered:
			return 3
		case AlertPlaybackCompleted, AlertFailed:
			return 4
		default:
			return 0
		}
	}
	s.mu.Lock()
	if sess.AlertOutcomeTerminal || rank(outcome) == 0 || rank(outcome) <= rank(sess.LastAlertOutcome) {
		s.mu.Unlock()
		return
	}
	callback := sess.AlertOutcome
	sess.LastAlertOutcome = outcome
	if outcome == AlertPlaybackCompleted || outcome == AlertFailed {
		sess.AlertOutcomeTerminal = true
	}
	s.mu.Unlock()
	callback(outcome)
}

func (s *Service) reportCallAlertOutcome(callID string, outcome AlertOutcome) {
	s.mu.Lock()
	sess := s.sessions[callID]
	s.mu.Unlock()
	s.reportAlertOutcome(sess, outcome)
}

func (s *Service) notifyAlert(sess *session, success bool) {
	if sess == nil {
		return
	}
	s.mu.Lock()
	done := sess.AlertDone
	sess.AlertDone = nil
	s.mu.Unlock()
	signalAlert(done, success)
}

func signalAlert(done chan<- bool, success bool) {
	if done == nil {
		return
	}
	select {
	case done <- success:
	default:
	}
}

func (s *Service) releaseSessionSpeech(sess *session) {
	if sess == nil {
		return
	}
	if sess.Lease != nil {
		sess.Lease.Release()
		sess.Lease = nil
	}
	if sess.EmailLease != nil {
		sess.EmailLease.Release()
		sess.EmailLease = nil
	}
}

// armInactivityTimeoutLocked resets the call-level IVR timer. It must be
// called with s.mu held; generation numbers prevent an older timer from
// hanging up a session after a newer DTMF event has reset it.
