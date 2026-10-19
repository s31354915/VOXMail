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
	remoteimap "github.com/voxmail/voxmail/internal/imap"
	"github.com/voxmail/voxmail/internal/ivr"
	"github.com/voxmail/voxmail/internal/keypad"
	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/process"
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
	promptFn          func(*session, string) error
	InactivityTimeout time.Duration
	// InactivityHangupDelay allows the goodbye prompt to finish before the
	// bridge receives hangup. Zero uses the production default.
	InactivityHangupDelay time.Duration
	mu                    sync.Mutex
	sessions              map[string]*session
	client                *bridge.Client
	pending               map[string]DialRequest
	ctx                   context.Context

	pinMu        sync.Mutex
	pinLock      map[string]time.Time
	pinNoLock    map[string]time.Time
	taskMu       sync.Mutex
	taskWG       sync.WaitGroup
	tasksClosed  bool
	sipApplying  bool
	shuttingDown bool
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
		// Release the timer's context whether the timer fires, is canceled
		// early, or the service shuts down. Without this the context lived
		// until service shutdown and leaked for every completed task.
		defer cancel()
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
	shuttingDown := s.shuttingDown
	s.mu.Unlock()
	if c == nil {
		return errors.New("bridge is not connected")
	}
	if shuttingDown {
		return errors.New("call service is shutting down")
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
	FolderRolePaths      map[string]string
	FolderLabels         map[string]string
	SelectedFolder       string
	MoveFolders          []string
	MoveFolderLabels     map[string]string
	MovePage             int
	AlertFolders         []string
	AlertFolderSelection map[string]bool
	AlertFolderPage      int
	CurrentAttachments   []mailparse.Attachment
	AttachmentPage       int
	AttachmentCancel     context.CancelFunc
	RecordCancel         context.CancelFunc
	RecordStop           chan struct{}
	RecordGeneration     uint64
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
	navigationGeneration uint64
	Closed               bool
	Established          bool
	mediaPlays           int
	attachmentSequence   uint64
	eventCtx             context.Context
	eventCancel          context.CancelFunc
	inactivityCancel     context.CancelFunc
	inactivityGeneration uint64
	promptMu             sync.Mutex
	playCancel           context.CancelFunc
	playSequence         uint64
	// playInterrupted is set when a keypress cancels the audio that is
	// playing, and cleared when the next prompt starts. Handlers check it to
	// skip what would follow the interrupted audio.
	playInterrupted bool
	greetAttempts   int
	// PendingAttachmentRemove tracks the two-press confirmation before an
	// attachment is removed in attachment_edit.
	PendingAttachmentRemove bool
	// PendingCancelConfirm tracks the two-press confirmation before the
	// review menu's cancel discards the draft.
	PendingCancelConfirm bool
	// RemoteMu guards the per-call reuse of one authenticated IMAP
	// connection for remote mutations (seen flags, moves). The connection is
	// closed when the call tears down or when a mutation reports an error.
	RemoteMu        sync.Mutex
	RemoteConn      *remoteimap.Client
	RemoteAccountID string
	// FolderRoleCache memoizes role lookups for this call so repeated
	// delete/move/send operations stop re-querying the store.
	FolderRoleCache map[string]store.FolderRole
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
	if sess == nil || sess.Closed || sess.mediaPlays > 0 {
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
	// The welcome prompt is spoken at most once per call: a failed or
	// interrupted greeting must never be re-spoken, which is how repeated
	// welcome loops used to start.
	s.mu.Lock()
	sess.greetAttempts++
	first := sess.greetAttempts == 1
	s.mu.Unlock()
	if !first {
		return
	}
	if s.Media.GreetingPath != "" {
		err := s.playPrompt(sess, func(ctx context.Context) error {
			return s.Media.PlayGreetingWithRuntime(ctx, sess.Runtime, sess.TxPath)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			if s.Log != nil {
				s.Log.Warn("static greeting unavailable; falling back to the pin prompt", "error", err)
			}
			// The greeting may have failed after part of it already played, and
			// the greet guard allows only one welcome per call. Never re-speak
			// the full welcome: the PIN instruction is the useful part.
			s.prompt(sess, menuPrompt("pin"))
			return
		}
		return
	}
	s.prompt(sess, speech.StaticWelcomeText)
}

func (s *Service) prompt(sess *session, text string) error {
	if s == nil || sess == nil {
		return nil
	}
	if s.promptFn != nil {
		return s.promptFn(sess, text)
	}
	if s.Media == nil || sess.TxPath == "" {
		return nil
	}
	return s.playPrompt(sess, func(ctx context.Context) error {
		return s.Media.PlayWithRuntimeForUser(ctx, s.promptRuntime(sess), sess.UserID, sess.TxPath, text)
	})
}

// promptMenu speaks a menu prompt at the menu voice even while the session is
// in an email state, where promptRuntime would read it at email speed and
// burn airtime on a long menu.
func (s *Service) promptMenu(sess *session, text string) error {
	if s == nil || sess == nil {
		return nil
	}
	if s.promptFn != nil {
		return s.promptFn(sess, text)
	}
	if s.Media == nil || sess.TxPath == "" {
		return nil
	}
	s.mu.Lock()
	runtime := sess.Runtime
	s.mu.Unlock()
	if runtime == nil {
		runtime = s.promptRuntime(sess)
	}
	return s.playPrompt(sess, func(ctx context.Context) error {
		return s.Media.PlayWithRuntimeForUser(ctx, runtime, sess.UserID, sess.TxPath, text)
	})
}

// wasInterrupted reports whether a keypress canceled the audio that just
// finished playing, so a handler can skip what would follow it (the menu
// after an email body) and let the pressed key take effect right away.
func (s *Service) wasInterrupted(sess *session) bool {
	if s == nil || sess == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return sess.playInterrupted
}

// All greetings, menus and email speech share cancellation and FIFO ownership.
// Synthesis has its own bound; playback must not inherit the 45s dial timeout.
func (s *Service) playPrompt(sess *session, play func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(s.eventContext(sess), process.PiperTimeout+process.PlaybackTimeout)
	s.mu.Lock()
	if sess.playCancel != nil {
		sess.playCancel()
	}
	sess.playSequence++
	sequence := sess.playSequence
	sess.playCancel = cancel
	sess.playInterrupted = false
	s.beginMediaLocked(sess)
	s.mu.Unlock()
	sess.promptMu.Lock()
	err := play(ctx)
	sess.promptMu.Unlock()
	playbackContextErr := ctx.Err()
	cancel()
	s.mu.Lock()
	if sess.playSequence == sequence {
		sess.playCancel = nil
	}
	s.endMediaLocked(sess)
	s.mu.Unlock()
	if err != nil && playbackContextErr != context.Canceled && s.Log != nil {
		s.Log.Warn("ivr prompt failed", "error", err)
	}
	return err
}

func (s *Service) beginMediaLocked(sess *session) {
	sess.mediaPlays++
	if sess.inactivityCancel != nil {
		sess.inactivityCancel()
		sess.inactivityCancel = nil
	}
	sess.inactivityGeneration++
}
func (s *Service) endMediaLocked(sess *session) {
	sess.mediaPlays--
	if sess.mediaPlays == 0 && !sess.Closed && stateLocked(sess) != "announce" && stateLocked(sess) != "outgoing" {
		s.armInactivityTimeoutLocked(sess)
	}
}

// interruptPrompt lets the bridge reader stop the current menu audio before
// its ordered event worker handles a completed DTMF key. Prompt playback is
// synchronous on that worker, so waiting to cancel from handleMenu would
// otherwise make every key arrive only after the prompt finishes.
func (s *Service) interruptPrompt(callID string) {
	if s == nil || callID == "" {
		return
	}
	s.mu.Lock()
	sess := s.sessions[callID]
	var cancel context.CancelFunc
	if sess != nil && !sess.Closed {
		cancel = sess.playCancel
		sess.playInterrupted = true
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
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
	var contextual string
	switch state {
	case "accounts":
		contextual = "Email. Press 0 for all unread mail. " + accountPrompt(sess.Accounts, sess.AccountPage)
	case "contacts":
		contextual = contactPrompt(sess.Contacts, sess.ContactPage)
	case "folders":
		contextual = folderPromptFor(account, sess.Folders, sess.FolderRolePaths, sess.FolderPrefix, sess.FolderPage, sess.FolderLabels)
	case "list":
		contextual = "There are no messages here. Press pound to go back."
		if sess.Cursor >= 0 && sess.Cursor < len(sess.Messages) {
			contextual = listPrompt(sess.Messages[sess.Cursor], sess.Cursor, len(sess.Messages))
		}
	case "recipient_menu":
		contextual = recipientPrompt(sess.Draft)
	case "field_confirm":
		contextual = fieldConfirmPrompt(sess.ConfirmState, sess.ConfirmValue)
	case "review":
		if sess.PendingCancelConfirm {
			contextual = "Press 3 again to cancel this email, or pound to keep editing."
		}
	case "attachment_edit":
		if sess.PendingAttachmentRemove {
			contextual = "Press 2 again to remove the last attachment, or pound to keep it."
		}
	}
	s.mu.Unlock()
	if contextual != "" {
		return contextual
	}
	if state == "settings" {
		return s.settingsPrompt(sess)
	}
	if state == "account_menu" && account.ID != "" {
		return s.accountMenuPrompt(sess, account)
	}
	return menuPrompt(state)
}

// repeatCurrentPrompt repeats the actual page for dynamic menus, including
// selected alert folders, rather than substituting generic instructions.
func (s *Service) repeatCurrentPrompt(sess *session) {
	s.mu.Lock()
	state := string(stateLocked(sess))
	s.mu.Unlock()
	switch state {
	case "draft_list":
		s.promptDraftList(sess)
	case "move_menu":
		s.promptMoveMenu(sess)
	case "attachment_menu":
		s.attachmentMenu(sess)
	case "account_alert_folders":
		s.promptAccountAlertFolders(sess)
	default:
		s.prompt(sess, s.promptForState(sess))
	}
}

func menuPrompt(state string) string {
	if prompt, ok := ivr.Prompt(ivr.State(state)); ok {
		return prompt
	}
	return "Press pound to go back or star to repeat."
}

func listPrompt(m store.MailSummary, cursor, total int) string {
	return fmt.Sprintf("Message %d of %d. From %s. Subject %s. %s", cursor+1, total, spokenHeader(m.Sender, "unknown sender", 240), spokenHeader(m.Subject, "no subject", 300), menuPrompt("list"))
}

func spokenHeader(value, empty string, limit int) string {
	if strings.TrimSpace(value) == "" {
		return empty
	}
	return speakable(value, limit)
}
