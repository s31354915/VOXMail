package calls

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/voxmail/voxmail/internal/bridge"
)

func (s *Service) Run(ctx context.Context) error {
	s.mu.Lock()
	s.ctx = ctx
	s.pinLock = make(map[string]time.Time)
	s.mu.Unlock()
	connected := false
	for {
		if err := s.runConnection(ctx); err != nil && !errors.Is(err, context.Canceled) {
			if s.Log != nil {
				message := "baresip bridge connection failed"
				if connected {
					message = "baresip bridge disconnected"
				}
				s.Log.Warn(message, "error", err)
			}
			connected = false
		} else if err == nil {
			connected = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// baseContext returns the service context (cancelled during shutdown) or
// context.Background when the service has not been started yet.
func (s *Service) baseContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func (s *Service) runConnection(ctx context.Context) error {
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = make(map[string]*session)
	}
	s.mu.Unlock()
	conn, err := net.DialTimeout("unix", s.Socket, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	stopConn := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopConn()
	s.mu.Lock()
	s.client = bridge.NewClient(conn)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.client = nil
		pending := make([]DialRequest, 0, len(s.pending))
		for requestID, req := range s.pending {
			pending = append(pending, req)
			delete(s.pending, requestID)
		}
		sessions := s.sessions
		s.sessions = make(map[string]*session)
		s.mu.Unlock()
		for _, req := range pending {
			reportDialOutcome(req.Outcome, AlertFailed)
			signalAlert(req.Done, false)
		}
		for _, sess := range sessions {
			if sess == nil {
				continue
			}
			s.mu.Lock()
			sess.Closed = true
			if sess.inactivityCancel != nil {
				sess.inactivityCancel()
				sess.inactivityCancel = nil
			}
			if sess.playCancel != nil {
				sess.playCancel()
				sess.playCancel = nil
			}
			if sess.AttachmentCancel != nil {
				sess.AttachmentCancel()
				sess.AttachmentCancel = nil
			}
			if sess.RecordCancel != nil {
				sess.RecordCancel()
				sess.RecordCancel = nil
			}
			s.mu.Unlock()
			s.notifyAlert(sess, false)
			s.reportAlertOutcome(sess, AlertFailed)
			s.releaseSessionSpeech(sess)
		}
	}()
	dispatcher := newCallEventDispatcher(ctx, s, conn)
	defer dispatcher.close()
	reader := bufio.NewReader(conn)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		message, err := bridge.Decode(reader)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if message.Type == "registration" {
			if s.RegistrationState != nil {
				s.RegistrationState(message.Phase, message.Reason)
			}
			continue
		}
		if message.Type == "command_error" {
			if message.RequestID != "" {
				s.failPending(message.RequestID, message.Reason)
			}
			if message.CallID != "" {
				s.closeCall(message)
			}
			continue
		}
		if err := dispatcher.dispatch(message); err != nil {
			return err
		}
		select {
		case err := <-dispatcher.errs:
			return err
		default:
		}
	}
}

func (s *Service) bindEventContext(callID string, ctx context.Context, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[callID]; sess != nil && !sess.Closed {
		sess.eventCtx = ctx
		if cancel != nil {
			sess.eventCancel = cancel
		}
	}
}

func (s *Service) closeCall(message bridge.Message) {
	s.mu.Lock()
	sess := s.sessions[message.CallID]
	var alertDone chan<- bool
	if sess != nil {
		sess.Closed = true
		if sess.eventCancel != nil {
			sess.eventCancel()
			sess.eventCancel = nil
		}
		if sess.inactivityCancel != nil {
			sess.inactivityCancel()
			sess.inactivityCancel = nil
		}
		alertDone = sess.AlertDone
		sess.AlertDone = nil
		if sess.playCancel != nil {
			sess.playCancel()
			sess.playCancel = nil
		}
		if sess.AttachmentCancel != nil {
			sess.AttachmentCancel()
			sess.AttachmentCancel = nil
		}
		if sess.RecordCancel != nil {
			sess.RecordCancel()
			sess.RecordCancel = nil
		}
	}
	delete(s.sessions, message.CallID)
	s.mu.Unlock()
	if alertDone != nil {
		signalAlert(alertDone, false)
	}
	s.reportAlertOutcome(sess, AlertFailed)
	s.releaseSessionSpeech(sess)
	if s.Log != nil {
		s.Log.Info("call closed", "call_id", message.CallID, "reason", message.Reason)
	}
}

// onEstablished triggers playback that must follow media establishment. RTP
// flows only once the audio stream starts, so greetings and alert text are
// deferred from admission/dial to this point rather than written into a FIFO
// that baresip has not opened yet.
func (s *Service) onEstablished(message bridge.Message) {
	s.mu.Lock()
	sess := s.sessions[message.CallID]
	sessState := ""
	if sess != nil {
		sessState = string(stateLocked(sess))
	}
	s.mu.Unlock()
	if sess == nil {
		return
	}
	switch sessState {
	case "outgoing":
		s.startTask(func() { s.playOutgoingAlert(sess) })
	default:
		s.startTask(func() { s.greet(sess) })
	}
}

// Healthy reports whether the call bridge is currently connected to baresip.
func (s *Service) Healthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return false
	}
	if s.ctx != nil && s.ctx.Err() != nil {
		return false
	}
	return true
}

// BeginSIPApply reserves a quiet window for a supervisor restart. Existing
// calls and pending outgoing alerts are rejected rather than interrupted, and
// new admissions are refused until the returned release function runs.
func (s *Service) BeginSIPApply(ctx context.Context) (func(), error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	if len(s.sessions) != 0 || len(s.pending) != 0 {
		s.mu.Unlock()
		return nil, errors.New("active calls are in progress")
	}
	s.sipApplying = true
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.sipApplying = false
			s.mu.Unlock()
		})
	}, nil
}

// ActiveCalls reports admitted and pending calls for status and operational
// decisions without exposing the session map.
func (s *Service) ActiveCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions) + len(s.pending)
}
