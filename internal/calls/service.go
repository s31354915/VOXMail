package calls

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/voxmail/voxmail/internal/bridge"
	"github.com/voxmail/voxmail/internal/ivr"
	"github.com/voxmail/voxmail/internal/keypad"
	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/speech"
	"github.com/voxmail/voxmail/internal/store"
)

type Service struct {
	Socket    string
	Store     *store.Store
	Log       *slog.Logger
	MaxCalls  int
	Media     *PromptPlayer
	Recorder  *VoiceRecorder
	DataRoot  string
	Secrets   *secret.Box
	Refresher AccountRefresher
	// RegistrationState receives native Baresip registration phases without
	// coupling the call bridge to the SIP supervisor package.
	RegistrationState func(string, string)
	// IMAPEndpointResolver returns the exact numeric endpoint that was
	// policy-checked and dialed. Production defaults to netguard; the seam
	// lets tests prove the address is passed into every authenticated IMAP
	// connection without requiring a public provider.
	IMAPEndpointResolver func(context.Context, string, int) (string, error)
	// DraftAppender is an optional seam for the remote Drafts append operation;
	// production uses the authenticated IMAP client, while integration tests
	// can verify the round-trip without requiring a live provider.
	DraftAppender func(context.Context, store.Account, string, []byte) error
	// SubmitMail is an optional seam for the SMTP submission path. Production
	// uses mailer.SendWithOutcome; tests can verify journaling without a live
	// SMTP peer.
	SubmitMail        func(mailer.Config, []string, []byte) mailer.SendResult
	InactivityTimeout time.Duration
	// InactivityHangupDelay allows the goodbye prompt to finish before the
	// bridge receives hangup. Zero uses the production default.
	InactivityHangupDelay time.Duration
	mu                    sync.Mutex
	sessions              map[string]*session
	client                *bridge.Client
	pending               map[string]DialRequest
	ctx                   context.Context

	pinMu       sync.Mutex
	pinLock     map[string]time.Time
	taskMu      sync.Mutex
	taskWG      sync.WaitGroup
	tasksClosed bool
	sipApplying bool
}

// startTask owns asynchronous call work so shutdown can wait for every
// goroutine that may still touch a session, bridge, speech runtime, or store.
func (s *Service) startTask(fn func()) {
	if s == nil || fn == nil {
		return
	}
	s.taskMu.Lock()
	if s.tasksClosed {
		s.taskMu.Unlock()
		return
	}
	s.taskWG.Add(1)
	s.taskMu.Unlock()
	go func() {
		defer s.taskWG.Done()
		fn()
	}()
}

// startDelayedTask keeps timer callbacks inside the service lifecycle. The
// returned cancellation function is used when the operation fails before its
// timeout; the service parent context cancels the timer during shutdown.
func (s *Service) startDelayedTask(delay time.Duration, fn func()) context.CancelFunc {
	parent := s.baseContext()
	ctx, cancel := context.WithCancel(parent)
	s.startTask(func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			fn()
		}
	})
	return cancel
}

// StopTasks closes the asynchronous-work admission gate. It must be called
// after the service has stopped receiving bridge events and before Wait so a
// late event cannot race WaitGroup.Add with shutdown.
func (s *Service) StopTasks() {
	if s == nil {
		return
	}
	s.taskMu.Lock()
	s.tasksClosed = true
	s.taskMu.Unlock()
}

// Wait waits for asynchronous call tasks after Run has stopped admitting
// bridge events. The caller must cancel the service context first.
func (s *Service) Wait() {
	if s == nil {
		return
	}
	s.taskWG.Wait()
}

// AccountRefresher is implemented by the mail synchronization service.  It is
// deliberately a small interface so the call/IVR layer can request a manual
// refresh without importing the synchronization package or coupling the two
// service lifecycles.
type AccountRefresher interface {
	RefreshAccount(context.Context, string, string) error
}

// AlertOutcome is the externally useful lifecycle of an outgoing alert call.
// The existing Done channel remains the terminal success/failure signal; this
// callback adds the intermediate states without changing existing callers.
type AlertOutcome string

const (
	AlertAccepted          AlertOutcome = "accepted"
	AlertRinging           AlertOutcome = "ringing"
	AlertAnswered          AlertOutcome = "answered"
	AlertPlaybackCompleted AlertOutcome = "playback_completed"
	AlertFailed            AlertOutcome = "failed"
)

// pinCooldown is how long a caller is locked out after failing the IVR PIN
// three times, across separate sessions from the same number.
const pinCooldown = 15 * time.Minute

// DialRequest describes an outgoing call. Outgoing calls back a subscribed
// phone number instead of admitting a caller, so the session is attached to
// the caller's account and plays Text once the far end answers.
type DialRequest struct {
	RequestID string             `json:"request_id"`
	URI       string             `json:"uri"`
	UserID    string             `json:"user_id"`
	AccountID string             `json:"account_id"`
	Text      string             `json:"text"`
	Done      chan<- bool        `json:"-"`
	Outcome   func(AlertOutcome) `json:"-"`
}

// Dial asks baresip to place an outgoing call to URI. The request is queued
// until the matching call_outgoing event arrives, which then becomes an
// established session with Text played once the far end picks up.
func (s *Service) Dial(ctx context.Context, req DialRequest) error {
	s.mu.Lock()
	c := s.client
	sipApplying := s.sipApplying
	s.mu.Unlock()
	if c == nil {
		return errors.New("bridge is not connected")
	}
	if sipApplying {
		return errors.New("SIP settings are being applied")
	}
	if strings.TrimSpace(req.URI) == "" {
		return errors.New("dial uri is required")
	}
	if req.RequestID == "" {
		requestID, err := bridge.NewRequestID()
		if err != nil {
			return err
		}
		req.RequestID = requestID
	}
	// Register the request before writing to the bridge. A local shim can emit
	// call_outgoing immediately, so registering after Send loses fast events.
	s.mu.Lock()
	if s.pending == nil {
		s.pending = make(map[string]DialRequest)
	}
	s.pending[req.RequestID] = req
	s.mu.Unlock()
	requestID := req.RequestID
	timeoutCancel := s.startDelayedTask(callTimeout, func() { s.expirePending(requestID) })
	dialCtx, cancel := context.WithTimeout(ctx, bridgeWriteTimeout)
	err := c.DialWithRequestID(dialCtx, req.RequestID, strings.TrimSpace(req.URI))
	cancel()
	if err != nil {
		timeoutCancel()
		s.mu.Lock()
		if s.pending != nil {
			delete(s.pending, req.RequestID)
		}
		s.mu.Unlock()
		reportDialOutcome(req.Outcome, AlertFailed)
		return err
	}
	reportDialOutcome(req.Outcome, AlertAccepted)
	if s.Log != nil {
		s.Log.Info("outgoing call dialed", "request_id", req.RequestID, "user_id", req.UserID, "account_id", req.AccountID)
	}
	return nil
}

func (s *Service) expirePending(requestID string) {
	s.failPending(requestID, "timed out")
}

func (s *Service) failPending(requestID, reason string) {
	s.mu.Lock()
	pending, ok := s.pending[requestID]
	if ok {
		delete(s.pending, requestID)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	if s.Log != nil {
		s.Log.Warn("outgoing call request failed before baresip created a call", "request_id", requestID, "user_id", pending.UserID, "account_id", pending.AccountID, "reason", reason)
	}
	reportDialOutcome(pending.Outcome, AlertFailed)
	signalAlert(pending.Done, false)
}

func reportDialOutcome(callback func(AlertOutcome), outcome AlertOutcome) {
	if callback != nil {
		callback(outcome)
	}
}

func (s *Service) hangup(callID string) error {
	return s.sendBridge(nil, bridge.Message{Type: "hangup", CallID: callID})
}

// sendBridge is the single response path for the call event loop. Production
// connections use bridge.Client so writes are serialized and deadline-bound;
// the net.Conn fallback keeps the small admission helpers directly testable
// before Service.Run has installed its shared client.
func (s *Service) sendBridge(fallback net.Conn, message bridge.Message) error {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if c != nil {
		ctx, cancel := context.WithTimeout(s.baseContext(), bridgeWriteTimeout)
		defer cancel()
		return c.SendContext(ctx, message)
	}
	if fallback == nil {
		return errors.New("bridge is not connected")
	}
	ctx, cancel := context.WithTimeout(s.baseContext(), bridgeWriteTimeout)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { _ = fallback.Close() })
	defer stopClose()
	if deadline, ok := ctx.Deadline(); ok {
		if err := fallback.SetWriteDeadline(deadline); err != nil {
			return err
		}
		defer fallback.SetWriteDeadline(time.Time{})
	}
	if err := bridge.Encode(fallback, message); err != nil {
		_ = fallback.Close()
		return err
	}
	return nil
}

// InvalidateUserSessions immediately ends every active call belonging to a
// user. It is used when the phone PIN changes so an already-authenticated call
// cannot remain usable with credentials that have just been rotated.
func (s *Service) InvalidateUserSessions(userID string) {
	s.invalidateUserSessions(userID, "PIN changed")
}

// InvalidateUserSessionsForDeletion revokes active and not-yet-established
// calls for a user before account/user metadata is removed.
func (s *Service) InvalidateUserSessionsForDeletion(userID string) {
	s.invalidateUserSessions(userID, "user deleted")
}

func (s *Service) invalidateUserSessions(userID, reason string) {
	if s == nil || strings.TrimSpace(userID) == "" {
		return
	}
	s.mu.Lock()
	var revoked []*session
	var pending []DialRequest
	for requestID, req := range s.pending {
		if req.UserID != userID {
			continue
		}
		delete(s.pending, requestID)
		pending = append(pending, req)
	}
	for callID, sess := range s.sessions {
		if sess == nil || sess.UserID != userID {
			continue
		}
		sess.Closed = true
		if sess.Flow != nil {
			sess.Flow.Close()
		}
		if sess.eventCancel != nil {
			sess.eventCancel()
			sess.eventCancel = nil
		}
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
		delete(s.sessions, callID)
		revoked = append(revoked, sess)
	}
	s.mu.Unlock()
	for _, req := range pending {
		reportDialOutcome(req.Outcome, AlertFailed)
		signalAlert(req.Done, false)
	}
	for _, sess := range revoked {
		_ = s.sendBridge(nil, bridge.Message{Type: "hangup", CallID: sess.CallID, Code: 603, Reason: reason})
		s.reportAlertOutcome(sess, AlertFailed)
		s.notifyAlert(sess, false)
		s.releaseSessionSpeech(sess)
	}
}

type session struct {
	CallID               string
	UserID               string
	Phone                string
	PIN                  string
	Failures             int
	Authenticated        bool
	State                string
	AlertText            string
	Messages             []store.MailSummary
	Cursor               int
	Accounts             []store.Account
	AccountPage          int
	Contacts             []store.Contact
	ContactPage          int
	Folders              []string
	FolderPage           int
	FolderPrefix         string
	SelectedFolder       string
	MoveFolders          []string
	MovePage             int
	AlertFolders         []string
	AlertFolderSelection map[string]bool
	AlertFolderPage      int
	CurrentAttachments   []mailparse.Attachment
	AttachmentPage       int
	AttachmentCancel     context.CancelFunc
	RecordCancel         context.CancelFunc
	RecordStop           chan struct{}
	AlertDone            chan<- bool
	AlertOutcome         func(AlertOutcome)
	LastAlertOutcome     AlertOutcome
	AlertOutcomeTerminal bool
	ActiveAccount        string
	Editor               *keypad.MultiTap
	Draft                draft
	Drafts               []store.DraftRecord
	DraftPage            int
	RecipientKind        string
	PendingContactName   string
	PendingContactEmail  string
	ConfirmValue         string
	ConfirmState         string
	ConfirmRecipientKind string
	EditingDraft         bool
	TxPath               string
	RxPath               string
	Lease                *speech.Lease
	Runtime              *speech.Runtime
	EmailLease           *speech.Lease
	EmailRuntime         *speech.Runtime
	SendInFlight         bool
	Flow                 *ivr.Session
	Closed               bool
	eventCtx             context.Context
	eventCancel          context.CancelFunc
	inactivityCancel     context.CancelFunc
	inactivityGeneration uint64
	promptMu             sync.Mutex
	playCancel           context.CancelFunc
	playSequence         uint64
}

type draft struct {
	ID                string
	To                string
	AdditionalTo      []string
	Cc                []string
	Bcc               []string
	Subject           string
	Body              string
	Attachments       []mailer.Attachment
	ForwardOriginal   bool
	OriginalMessageID *int64
}

const callTimeout = 45 * time.Second

const defaultInactivityTimeout = 2 * time.Minute

const defaultInactivityHangupDelay = 3 * time.Second

const (
	maxIVRRecipientRunes   = 254
	maxIVRSubjectRunes     = 998
	maxIVRBodyRunes        = 12000
	maxIVRContactNameRunes = 128
)

// bridgeWriteTimeout bounds the synchronous part of placing an outbound
// call. The call itself may remain active for callTimeout, but an unavailable
// baresip socket must fail promptly.
const bridgeWriteTimeout = 5 * time.Second

func (s *Service) armInactivityTimeoutLocked(sess *session) {
	if sess == nil || sess.Closed {
		return
	}
	if sess.inactivityCancel != nil {
		sess.inactivityCancel()
	}
	timeout := s.InactivityTimeout
	if timeout <= 0 {
		timeout = defaultInactivityTimeout
	}
	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	sess.inactivityCancel = cancel
	sess.inactivityGeneration++
	generation := sess.inactivityGeneration
	callID := sess.CallID
	s.startTask(func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.mu.Lock()
		if sess.Closed || sess.inactivityGeneration != generation {
			s.mu.Unlock()
			return
		}
		sess.inactivityCancel = nil
		sess.inactivityGeneration++
		s.mu.Unlock()
		s.startTask(func() { s.prompt(sess, "No input was received. Goodbye.") })
		delay := s.InactivityHangupDelay
		if delay <= 0 {
			delay = defaultInactivityHangupDelay
		}
		s.startDelayedTask(delay, func() { _ = s.hangup(callID) })
	})
}

// backByContractLocked applies the formal IVR navigation contract. Recursive
// folder and editor states can layer their data-dependent behavior on top of
// this helper, but ordinary menu states do not need a second hard-coded back
// map in the call service.
func (s *Service) greet(sess *session) {
	if sess == nil || s.Media == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.eventContext(sess), 45*time.Second)
	defer cancel()
	if s.Media.GreetingPath != "" {
		if err := s.Media.PlayGreetingWithRuntime(ctx, sess.Runtime, sess.TxPath); err == nil {
			return
		} else if s.Log != nil {
			s.Log.Warn("static greeting unavailable; falling back to synthesis", "error", err)
		}
	}
	s.prompt(sess, "Welcome to VOXMail. Enter your PIN, then press pound.")
}

func (s *Service) prompt(sess *session, text string) error {
	if s.Media == nil || sess == nil || sess.TxPath == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.eventContext(sess), 45*time.Second)
	s.mu.Lock()
	if sess.playCancel != nil {
		sess.playCancel()
	}
	sess.playSequence++
	sequence := sess.playSequence
	sess.playCancel = cancel
	s.mu.Unlock()
	sess.promptMu.Lock()
	err := s.Media.PlayWithRuntimeForUser(ctx, s.promptRuntime(sess), sess.UserID, sess.TxPath, text)
	sess.promptMu.Unlock()
	cancel()
	s.mu.Lock()
	if sess.playSequence == sequence {
		sess.playCancel = nil
	}
	s.mu.Unlock()
	if err != nil && ctx.Err() == nil && s.Log != nil {
		s.Log.Warn("ivr prompt failed", "error", err)
	}
	return err
}

func (s *Service) eventContext(sess *session) context.Context {
	parent := s.baseContext()
	s.mu.Lock()
	if sess != nil && sess.eventCtx != nil {
		parent = sess.eventCtx
	}
	s.mu.Unlock()
	return parent
}

func (s *Service) promptRuntime(sess *session) *speech.Runtime {
	if sess == nil {
		return nil
	}
	s.mu.Lock()
	state := stateLocked(sess)
	emailRuntime := sess.EmailRuntime
	menuRuntime := sess.Runtime
	s.mu.Unlock()
	switch state {
	case "list", "read", "attachment_menu", "attachment_playback", "move_menu", "more_options", "compose", "recipient_menu", "recipient_input", "subject_method", "subject", "body_method", "body", "review", "forward_options", "audio_recording", "recording", "recording_subject", "contact_name", "contact_confirm", "draft_menu", "draft_list", "draft_edit", "recipient_edit", "attachment_edit":
		if emailRuntime != nil {
			return emailRuntime
		}
	}
	return menuRuntime
}

func (s *Service) alertsAvailable(sess *session) bool {
	if s.Store == nil || sess == nil {
		return false
	}
	available, err := s.Store.AlertsAvailable(s.baseContext())
	return err == nil && available
}

func (s *Service) settingsPrompt(sess *session) string {
	if s.alertsAvailable(sess) {
		return "Press 1 for voice settings, 2 to toggle call alerts, 3 for contacts, or pound to go back."
	}
	return "Press 1 for voice settings, 3 for contacts, or pound to go back."
}

func (s *Service) promptForState(sess *session) string {
	s.mu.Lock()
	state := string(stateLocked(sess))
	accountID := sess.ActiveAccount
	var account store.Account
	for _, candidate := range sess.Accounts {
		if candidate.ID == accountID {
			account = candidate
			break
		}
	}
	s.mu.Unlock()
	if state == "settings" {
		return s.settingsPrompt(sess)
	}
	if state == "account_menu" && account.ID != "" {
		return s.accountMenuPrompt(sess, account)
	}
	return menuPrompt(state)
}

func menuPrompt(state string) string {
	if prompt, ok := ivr.Prompt(ivr.State(state)); ok {
		return prompt
	}
	switch state {
	case "main":
		return "Press 1 for email, 2 for settings, or 3 for information and instructions."
	case "accounts":
		return "Press 0 for all unread mail, choose an account, or press pound to go back."
	case "account_menu":
		return "Press 1 to listen to mail, 2 to send an email, 3 to refresh, or 4 for account settings."
	case "folders":
		return "Choose a folder, press 0 for Inbox at the root, or press pound to go back."
	case "folder_action":
		return "Press 1 to listen to messages here, or 2 to open nested folders."
	case "contacts":
		return "Choose a contact or press pound to go back."
	case "settings":
		return "Press 1 for voice settings, 2 to toggle call alerts, 3 for contacts, or pound to go back."
	case "list":
		return "Press 1 to read, 2 for next, 3 for previous, 4 to delete, 5 to reply, or pound to go back."
	case "read":
		return "Press 1 to listen, 2 to mark read or unread, 3 to reply, 4 to reply all, 5 to forward, 6 to delete, 7 to move, 8 for attachments, 9 for more options, 0 for next, or pound to return."
	case "more_options":
		return "More options. Press 1 to add the sender to contacts, 2 for the date, 3 for recipients, 4 for the subject, or pound to return."
	case "contact_name":
		return "Enter a contact name using the keypad, then press pound."
	case "contact_confirm":
		return "Press 1 to save this contact or 2 to cancel."
	case "review":
		return "Press 1 to send, 2 to save as draft, 3 to cancel, 4 to edit the message, 5 to add an audio attachment, or pound to go back."
	case "compose":
		return "Enter the recipient using multi tap, then press pound."
	case "recipient_menu":
		return "Press 1 to add To, 2 to add Cc, 3 to add Bcc, or 4 to continue."
	case "recipient_input":
		return "Enter the email address using multi tap, then press pound."
	case "subject_method":
		return "For the subject, press 1 to type with the keypad or 2 to speak."
	case "subject":
		return "Enter the subject using multi tap, then press pound."
	case "body_method":
		return "For the message body, press 1 to type with the keypad or 2 to speak."
	case "body":
		return "Enter the message using multi tap, then press pound."
	case "attachment_menu":
		return "Choose an attachment, press 0 for more, or pound to return."
	case "attachment_playback":
		return "Playing the attachment. Press pound to stop."
	case "move_menu":
		return "Choose a destination folder, or press pound to return."
	case "forward_options":
		return "Forward the original message with its attachments? Press 1 for yes or 2 for no."
	}
	return "Press pound to go back or star to repeat."
}
func listPrompt(m store.MailSummary, cursor, total int) string {
	return fmt.Sprintf("Message %d of %d. From %s. Subject %s. Press 1 to read.", cursor+1, total, m.Sender, m.Subject)
}
