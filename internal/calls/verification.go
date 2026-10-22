package calls

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/store"
)

// DialVerification places one outgoing verification call that speaks the
// number-ownership code once and hangs up. The web console uses it when the
// administrator verification policy points at outgoing calls.
func (s *Service) DialVerification(ctx context.Context, uri, userID, code string) error {
	spaced := strings.Join(strings.Split(code, ""), " ")
	done := make(chan bool, 1)
	return s.Dial(ctx, DialRequest{
		URI:    uri,
		UserID: userID,
		Text:   "Your VOXMail verification code is " + spaced + ". You can hang up now. Goodbye.",
		Done:   done,
	})
}

// speakAccountChoice reads the account menu when one number reaches several
// accounts. The number's accounts are ordered by their binding position, and
// the caller picks one before any PIN is requested.
func (s *Service) speakAccountChoice(sess *session) {
	if sess == nil {
		return
	}
	s.mu.Lock()
	choices := sess.Choices
	s.mu.Unlock()
	if len(choices) == 0 {
		s.mu.Lock()
		transitionLocked(sess, "closed")
		sess.Closed = true
		s.mu.Unlock()
		_ = s.hangup(sess.CallID)
		return
	}
	s.mu.Lock()
	transitionLocked(sess, "account_choice")
	s.mu.Unlock()
	s.prompt(sess, accountChoicePrompt(choices))
}

// accountChoicePrompt names every reachable account with its menu key. A
// single username behind the number keeps the spec wording; mixed usernames
// announce the owning user with each press.
func accountChoicePrompt(choices []store.CallerRoute) string {
	parts := make([]string, 0, len(choices))
	for index, route := range choices {
		key := strconv.Itoa(index + 1)
		if allOneUser(choices) {
			parts = append(parts, "press "+key+" for "+route.AccountName)
		} else {
			parts = append(parts, "for "+route.Username+", press "+key+" for "+route.AccountName)
		}
	}
	return "Welcome to VOXMail. Please choose the VOXMail account for " + choices[0].Username + ": " + strings.Join(parts, ", ") + "."
}

func allOneUser(choices []store.CallerRoute) bool {
	for _, route := range choices {
		if route.Username != choices[0].Username {
			return false
		}
	}
	return true
}

// chooseCallerAccount maps the pressed digit onto a route, moves the session
// to the PIN step, and skips the usual welcome greeting: the menu already
// welcomed the caller by name.
func (s *Service) chooseCallerAccount(conn net.Conn, sess *session, digit string) error {
	s.mu.Lock()
	var chosen store.CallerRoute
	var found bool
	if n, err := strconv.Atoi(strings.TrimSpace(digit)); err == nil && n >= 1 && n <= len(sess.Choices) {
		chosen, found = sess.Choices[n-1], true
	}
	if !found {
		s.mu.Unlock()
		s.speakAccountChoice(sess)
		return nil
	}
	sess.UserID = chosen.UserID
	sess.Choices = nil
	transitionLocked(sess, "pin")
	s.mu.Unlock()
	s.prompt(sess, "Welcome to VOXMail, "+chosen.Username+". Enter your PIN, then press pound.")
	return nil
}

// speakVerification reads a pending number-ownership code to the answered
// caller and hangs up. Digits are spaced so speech reads them individually.
func (s *Service) speakVerification(sess *session) {
	if sess == nil {
		return
	}
	s.mu.Lock()
	code := sess.VerificationCode
	s.mu.Unlock()
	if code == "" {
		s.mu.Lock()
		transitionLocked(sess, "closed")
		sess.Closed = true
		s.mu.Unlock()
		_ = s.hangup(sess.CallID)
		return
	}
	s.mu.Lock()
	transitionLocked(sess, "verification")
	s.mu.Unlock()
	spaced := strings.Join(strings.Split(code, ""), " ")
	if err := s.prompt(sess, "Your VOXMail verification code is "+spaced+"."); err != nil {
		_ = s.hangup(sess.CallID)
		return
	}
	s.startDelayedTask(3*time.Second, func() {
		_ = s.hangup(sess.CallID)
	})
}
