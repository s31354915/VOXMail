package calls

import (
	"fmt"
	"time"

	"github.com/voxmail/voxmail/internal/diagnostics"
	"log/slog"
	"net"
	"strings"

	"github.com/voxmail/voxmail/internal/auth"
	"github.com/voxmail/voxmail/internal/bridge"
	"github.com/voxmail/voxmail/internal/ivr"
	"github.com/voxmail/voxmail/internal/store"
)

func (s *Service) handleDTMF(conn net.Conn, message bridge.Message) error {
	s.mu.Lock()
	sess := s.sessions[message.CallID]
	if sess == nil || sess.Closed {
		s.mu.Unlock()
		return nil
	}
	if diagnostics.Enabled() {
		slog.Debug("DTMF received", "call_id", message.CallID, "authenticated", sess.Authenticated, "state", sess.State, "phase", message.Phase)
	}
	s.armInactivityTimeoutLocked(sess)
	if sess.Authenticated {
		s.mu.Unlock()
		return s.handleMenu(conn, sess, message)
	}
	// A verification call listens for nothing; keep the line open while the
	// code is spoken and ignore every key.
	if sess.State == "verification" {
		s.mu.Unlock()
		return nil
	}
	if sess.State == "account_choice" {
		s.mu.Unlock()
		return s.chooseCallerAccount(conn, sess, message.Digit)
	}
	if message.Digit == "*" {
		hadInput := sess.PIN != ""
		sess.PIN = ""
		s.mu.Unlock()
		// Clearing the PIN used to be silent; a caller could not tell
		// whether their keys registered at all.
		if hadInput {
			s.prompt(sess, "PIN cleared. Enter your PIN, then press pound.")
		}
		return nil
	}
	if message.Digit != "#" {
		if len(sess.PIN) < 12 && len(message.Digit) == 1 && message.Digit[0] >= '0' && message.Digit[0] <= '9' {
			sess.PIN += message.Digit
		}
		s.mu.Unlock()
		return nil
	}
	userID, pin := sess.UserID, sess.PIN
	s.mu.Unlock()
	user, err := s.Store.UserByID(s.baseContext(), userID)
	if err != nil {
		// A transient store failure is not a wrong PIN. Spending one of the
		// caller's three attempts on a database hiccup, or locking the line on
		// three of them, punishes callers for an outage they did not cause.
		if s.Log != nil {
			s.Log.Warn("ivr PIN verification could not load the account", "call_id", message.CallID, "error", err)
		}
		s.mu.Lock()
		sess.PIN = ""
		s.mu.Unlock()
		s.prompt(sess, "I couldn't check that just now. Please try again in a moment.")
		return nil
	}
	if !auth.Check(user.PINHash, pin) {
		var remaining int
		s.mu.Lock()
		sess.PIN = ""
		sess.Failures++
		remaining = 3 - sess.Failures
		if sess.Failures >= 3 {
			phone := sess.Phone
			s.mu.Unlock()
			s.recordPinFailure(phone)
			if s.Log != nil {
				s.Log.Warn("ivr PIN locked out", "call_id", message.CallID, "phone", phone)
			}
			// Never drop a locked-out caller in silence: say why, let the
			// goodbye finish, then hang up.
			callID := message.CallID
			s.prompt(sess, "Too many PIN attempts. Goodbye.")
			s.startDelayedTask(2*time.Second, func() {
				_ = s.sendBridge(conn, bridge.Message{Type: "hangup", CallID: callID, Code: 603, Reason: "PIN verification failed"})
			})
			return nil
		}
		s.mu.Unlock()
		s.prompt(sess, pinFailureNotice(remaining))
		return nil
	}
	s.mu.Lock()
	sess.Authenticated = true
	sess.Failures = 0
	if sess.Flow == nil {
		sess.Flow = ivr.NewSession(sess.CallID)
		if sess.State != "" {
			sess.Flow.State = ivr.State(sess.State)
		}
	}
	transitionLocked(sess, "main")
	phone := sess.Phone
	s.mu.Unlock()
	s.clearPinFailure(phone)
	if s.Log != nil {
		s.Log.Info("ivr PIN accepted", "call_id", message.CallID, "user_id", sess.UserID)
	}
	s.prompt(sess, s.signedInPrompt(sess))
	return nil
}

func (s *Service) signedInPrompt(sess *session) string {
	accounts, err := s.Store.ListAccounts(s.baseContext(), sess.UserID)
	if err != nil {
		return "You are signed in. " + menuPrompt("main")
	}
	total := 0
	unchecked := 0
	parts := make([]string, 0, len(accounts))
	for _, account := range accounts {
		count, listErr := s.Store.CountUnreadForAccount(s.baseContext(), sess.UserID, account.ID)
		if listErr != nil {
			unchecked++
			continue
		}
		total += count
		if count > 0 {
			parts = append(parts, fmt.Sprintf("%d in %s", count, account.CanonicalName))
		}
	}
	if len(parts) == 0 && unchecked == 0 {
		return "You have no unread email. " + menuPrompt("main")
	}
	summary := fmt.Sprintf("You have %d unread emails", total)
	if total == 1 {
		summary = "You have 1 unread email"
	}
	if unchecked > 0 {
		if total == 1 {
			summary = "You have at least 1 unread email"
		} else if total > 1 {
			summary = fmt.Sprintf("You have at least %d unread emails", total)
		} else {
			summary = "Some accounts could not be checked for unread email"
		}
	}
	if len(parts) > 0 {
		summary += ", " + strings.Join(parts, ", ")
	}
	return summary + ". " + menuPrompt("main")
}

func (s *Service) handleMenu(conn net.Conn, sess *session, message bridge.Message) error {
	key := message.Digit
	if key == "" {
		return nil
	}
	s.mu.Lock()
	if sess == nil || sess.Closed || !sess.Authenticated {
		s.mu.Unlock()
		return nil
	}
	ensureFlowLocked(sess)
	previousState := string(stateLocked(sess))
	editing := previousState == "compose" || previousState == "recipient_input" || previousState == "subject" || previousState == "body" || previousState == "contact_name"
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		nextState := string(stateLocked(sess))
		if diagnostics.Enabled() {
			slog.Debug("IVR menu completed", "call_id", message.CallID, "previous_state", previousState, "state", nextState)
		}
		if nextState != previousState {
			ensureFlowLocked(sess)
		}
		s.mu.Unlock()
	}()
	if editing {
		s.handleCompose(sess, key)
		return nil
	}
	if key == "*" {
		s.mu.Lock()
		state := string(stateLocked(sess))
		cancelNotice := ""
		if state == "attachment_playback" {
			if sess.AttachmentCancel != nil {
				sess.AttachmentCancel()
				sess.AttachmentCancel = nil
			}
			transitionLocked(sess, "read")
		}
		if state == "audio_recording" {
			sess.RecordGeneration++
			if sess.RecordCancel != nil {
				sess.RecordCancel()
			}
			sess.RecordCancel = nil
			sess.RecordStop = nil
			backUsingFlowLocked(sess, "review")
			cancelNotice = "Audio recording canceled. "
		}
		if state == "recording" || state == "recording_subject" {
			sess.RecordGeneration++
			if sess.RecordCancel != nil {
				sess.RecordCancel()
			}
			sess.RecordCancel = nil
			sess.RecordStop = nil
			fallback := "body_method"
			if state == "recording_subject" {
				fallback = "subject_method"
			}
			backUsingFlowLocked(sess, fallback)
			cancelNotice = "Recording canceled. "
		}
		s.mu.Unlock()
		if cancelNotice != "" {
			s.prompt(sess, cancelNotice+s.promptForState(sess))
			return nil
		}
		s.repeatCurrentPrompt(sess)
		return nil
	}
	if key == "#" {
		s.mu.Lock()
		if stateLocked(sess) == "review" && sess.PendingCancelConfirm {
			sess.PendingCancelConfirm = false
			s.mu.Unlock()
			s.prompt(sess, "Your message is kept. "+menuPrompt("review"))
			return nil
		}
		if ivr.IsHelpState(stateLocked(sess)) {
			backByContractLocked(sess, "info")
			s.mu.Unlock()
			s.repeatCurrentPrompt(sess)
			return nil
		}
		switch string(stateLocked(sess)) {
		case "accounts", "contacts", "settings", "info":
			backUsingFlowLocked(sess, "main")
		case "account_menu":
			backUsingFlowLocked(sess, "accounts")
		case "draft_menu":
			backUsingFlowLocked(sess, "account_menu")
		case "draft_list":
			backUsingFlowLocked(sess, "draft_menu")
		case "draft_edit":
			sess.EditingDraft = false
			backUsingFlowLocked(sess, "review")
		case "recipient_edit", "attachment_edit":
			backUsingFlowLocked(sess, "draft_edit")
		case "list":
			fallback := "main"
			if sess.ActiveAccount != "" {
				fallback = "folders"
			}
			backUsingFlowLocked(sess, fallback)
		case "folders":
			if sess.FolderPrefix != "" {
				sess.FolderPrefix = ""
				sess.FolderPage = 0
			} else {
				backUsingFlowLocked(sess, "account_menu")
			}
		case "folder_action":
			backUsingFlowLocked(sess, "folders")
		case "read", "confirm_delete":
			sess.BodyPaused = false
			sess.BodyChunks = nil
			sess.BodyMarkRead = nil
			backUsingFlowLocked(sess, "list")
		case "attachment_menu", "attachment_playback":
			if sess.AttachmentCancel != nil {
				sess.AttachmentCancel()
				sess.AttachmentCancel = nil
			}
			backUsingFlowLocked(sess, "read")
		case "move_menu":
			backUsingFlowLocked(sess, "read")
		case "account_settings":
			backUsingFlowLocked(sess, "account_menu")
		case "account_alert_folders":
			backUsingFlowLocked(sess, "account_settings")
		case "subject":
			if sess.EditingDraft {
				transitionLocked(sess, "draft_edit")
			} else {
				transitionLocked(sess, "compose")
			}
		case "subject_method":
			if sess.EditingDraft {
				transitionLocked(sess, "draft_edit")
			} else {
				transitionLocked(sess, "recipient_menu")
			}
		case "body":
			if sess.EditingDraft {
				transitionLocked(sess, "draft_edit")
			} else {
				transitionLocked(sess, "subject")
			}
		case "body_method":
			if sess.EditingDraft {
				transitionLocked(sess, "draft_edit")
			} else {
				transitionLocked(sess, "subject")
			}
		case "recipient_input":
			transitionLocked(sess, "recipient_menu")
		case "recipient_menu":
			if sess.EditingDraft {
				transitionLocked(sess, "draft_edit")
			} else {
				transitionLocked(sess, "compose")
			}
		case "field_confirm":
			transitionLocked(sess, sess.ConfirmState)
			sess.Editor = newIVREditor(sess.ConfirmState)
			sess.Editor.Text = sess.ConfirmValue
			sess.ConfirmValue = ""
			sess.ConfirmState = ""
			sess.ConfirmRecipientKind = ""
		case "review":
			backUsingFlowLocked(sess, "body")
		case "forward_options":
			backUsingFlowLocked(sess, "read")
		case "more_options":
			backUsingFlowLocked(sess, "read")
		case "contact_name":
			backUsingFlowLocked(sess, "more_options")
		case "contact_confirm":
			backUsingFlowLocked(sess, "more_options")
		case "audio_recording":
			sess.RecordGeneration++
			if sess.RecordCancel != nil {
				sess.RecordCancel()
				sess.RecordCancel = nil
			}
			backUsingFlowLocked(sess, "review")
		case "recording", "recording_subject":
			sess.RecordGeneration++
			if sess.RecordCancel != nil {
				sess.RecordCancel()
				sess.RecordCancel = nil
			}
			sess.RecordStop = nil
			fallback := "body_method"
			if string(stateLocked(sess)) == "recording_subject" {
				fallback = "subject_method"
			}
			backUsingFlowLocked(sess, fallback)
		}
		s.mu.Unlock()
		s.repeatCurrentPrompt(sess)
		return nil
	}
	s.mu.Lock()
	typedState := stateLocked(sess)
	state := string(typedState)
	s.mu.Unlock()
	if !ivr.Accepts(typedState, key) {
		s.prompt(sess, "That key is not available here. "+s.promptForState(sess))
		return nil
	}
	if next, ok := ivr.HelpDestination(typedState, key); ok {
		s.mu.Lock()
		transitionLocked(sess, string(next))
		s.mu.Unlock()
		s.prompt(sess, menuPrompt(string(next)))
		return nil
	}
	switch state {
	case "main":
		switch key {
		case "1":
			s.openAccounts(sess)
		case "2":
			s.mu.Lock()
			transitionLocked(sess, "settings")
			s.mu.Unlock()
			s.prompt(sess, s.settingsPrompt(sess))
		case "3":
			s.mu.Lock()
			transitionLocked(sess, "info")
			s.mu.Unlock()
			s.prompt(sess, menuPrompt("info"))
		}
	case "accounts":
		if key == "0" {
			s.openMessages(sess, true, "all unread mail")
		} else {
			s.handleAccountMenu(sess, key)
		}
	case "account_menu":
		s.handleSelectedAccountMenu(sess, key)
	case "draft_menu":
		s.handleDraftMenu(sess, key)
	case "draft_list":
		s.handleDraftList(sess, key)
	case "draft_edit":
		s.handleDraftEdit(sess, key)
	case "recipient_edit":
		s.handleRecipientEdit(sess, key)
	case "attachment_edit":
		s.handleAttachmentEdit(sess, key)
	case "account_settings":
		s.handleAccountSettings(sess, key)
	case "account_alert_folders":
		s.handleAccountAlertFolders(sess, key)
	case "folders":
		s.handleFolderMenu(sess, key)
	case "folder_action":
		s.handleFolderAction(sess, key)
	case "contacts":
		s.handleContactMenu(sess, key)
	case "recipient_pick":
		s.handleRecipientPick(sess, key)
	case "contact_pick":
		s.handleContactPick(sess, key)
	case "recipient_menu":
		s.handleRecipientMenu(sess, key)
	case "field_confirm":
		s.handleFieldConfirmation(sess, key)
	case "subject_method":
		s.handleFieldMethod(sess, key, "subject")
	case "body_method":
		s.handleFieldMethod(sess, key, "body")
	case "settings":
		s.handleSettingsMenu(sess, key)
	case "list":
		s.mu.Lock()
		if len(sess.Messages) == 0 {
			s.mu.Unlock()
			s.prompt(sess, "There are no messages here. Press pound to go back.")
			return nil
		}
		m := sess.Messages[sess.Cursor]
		wrapped := ""
		switch key {
		case "1":
			transitionLocked(sess, "read")
		case "2":
			if sess.Cursor == len(sess.Messages)-1 {
				wrapped = "That was the last message. Starting over from the first."
			}
			sess.Cursor = (sess.Cursor + 1) % len(sess.Messages)
			m = sess.Messages[sess.Cursor]
		case "3":
			if sess.Cursor == 0 {
				wrapped = "That was the first message. Going back to the last one."
			}
			sess.Cursor = (sess.Cursor + len(sess.Messages) - 1) % len(sess.Messages)
			m = sess.Messages[sess.Cursor]
		case "4":
			transitionLocked(sess, "confirm_delete")
		case "5":
			s.startComposeLocked(sess, "reply")
		case "6":
			s.startComposeLocked(sess, "forward")
		case "7":
			transitionLocked(sess, "list_actions")
		}
		state, cursor, total := string(stateLocked(sess)), sess.Cursor, len(sess.Messages)
		s.mu.Unlock()
		if wrapped != "" {
			s.prompt(sess, wrapped)
		}
		if state == "read" {
			s.readMessage(sess, m)
		} else if state == "confirm_delete" {
			s.prompt(sess, "Delete this message? Press 1 to confirm or 2 to cancel.")
		} else if state == "compose" {
			s.prompt(sess, "Enter the recipient using multi tap, then press pound.")
		} else if state == "subject" {
			s.prompt(sess, "Enter the subject using multi tap, then press pound.")
		} else if state == "list_actions" {
			s.prompt(sess, menuPrompt("list_actions"))
		} else if state == "subject_method" {
			s.prompt(sess, menuPrompt("subject_method"))
		} else if state == "body" {
			s.prompt(sess, "Enter the message using multi tap, then press pound.")
		} else {
			s.prompt(sess, listPrompt(m, cursor, total))
		}
	case "read", "list_actions":
		s.mu.Lock()
		if len(sess.Messages) == 0 {
			s.mu.Unlock()
			s.prompt(sess, "There are no messages here. Press pound to go back.")
			return nil
		}
		if sess.Cursor < 0 || sess.Cursor >= len(sess.Messages) {
			sess.Cursor = 0
		}
		m := sess.Messages[sess.Cursor]
		isRead := string(stateLocked(sess)) == "read"
		s.mu.Unlock()
		// While a message body plays or sits paused, keys 4, 6 and 7 are
		// pause, skip and speech rate instead of their read-menu meanings.
		if isRead && s.handleBodyPlaybackKey(sess, key) {
			return nil
		}
		if key == "1" {
			s.readMessage(sess, m)
		} else if key == "2" {
			s.toggleRead(sess, m)
		} else if key == "3" {
			s.startCompose(sess, "reply")
			s.prompt(sess, "Replying. "+menuPrompt("body_method"))
		} else if key == "4" {
			s.startCompose(sess, "replyall")
			s.prompt(sess, "Reply all. "+menuPrompt("body_method"))
		} else if key == "5" {
			s.mu.Lock()
			transitionLocked(sess, "forward_options")
			s.mu.Unlock()
			s.prompt(sess, "Forward the original message with its attachments? Press 1 for yes or 2 for no.")
		} else if key == "6" {
			s.mu.Lock()
			transitionLocked(sess, "confirm_delete")
			s.mu.Unlock()
			s.prompt(sess, "Delete this message? Press 1 to confirm or 2 to cancel.")
		} else if key == "8" {
			s.attachmentMenu(sess)
		} else if key == "7" {
			s.openMoveMenu(sess, m)
		} else if key == "9" {
			// While a long message still has unheard pages, key 9 keeps
			// reading the rest; once the body is spent it opens the
			// more-options menu as before.
			if !s.speakMoreBody(sess) {
				s.mu.Lock()
				transitionLocked(sess, "more_options")
				s.mu.Unlock()
				s.prompt(sess, menuPrompt("more_options"))
			}
		} else if key == "0" {
			// "0 for next" continues the list from the next message rather
			// than reading it outright: the caller hears the next entry and
			// the list menu, and stays in control of what plays.
			m = s.advanceToPlayable(sess)
			s.mu.Lock()
			next := "list"
			if string(stateLocked(sess)) == "list_actions" {
				next = "list_actions"
			}
			transitionLocked(sess, next)
			cursor, total := sess.Cursor, len(sess.Messages)
			sess.BodyPending = nil
			sess.BodyChunks = nil
			sess.BodyIndex = 0
			sess.BodyPaused = false
			sess.BodyInterrupted = false
			sess.BodyMarkRead = nil
			s.mu.Unlock()
			if next == "list_actions" {
				s.prompt(sess, fmt.Sprintf("Message %d of %d. From %s. Subject %s. %s", cursor+1, total, spokenHeader(m.Sender, "unknown sender", 240), spokenHeader(m.Subject, "no subject", 300), menuPrompt("list_actions")))
			} else {
				s.prompt(sess, listPrompt(m, cursor, total))
			}
		}
	case "attachment_menu":
		if key == "0" {
			s.mu.Lock()
			playable := 0
			for _, attachment := range sess.CurrentAttachments {
				if attachment.Playable && len(attachment.Data) > 0 {
					playable++
				}
			}
			more := (sess.AttachmentPage+1)*9 < playable
			if more {
				sess.AttachmentPage++
			}
			s.mu.Unlock()
			if more {
				s.attachmentMenu(sess)
			} else {
				s.prompt(sess, "There are no more attachments. Press pound to return.")
				s.attachmentMenu(sess)
			}
			return nil
		}
		if key < "1" || key > "9" {
			return nil
		}
		s.mu.Lock()
		transitionLocked(sess, "attachment_playback")
		s.mu.Unlock()
		s.prompt(sess, "Playing attachment.")
		s.startTask(func() { s.playAttachment(sess, int(key[0]-'1')) })
	case "attachment_playback":
		// Any DTMF key is a barge-in. # and * are handled above; for other
		// keys stop the media and return to the message action view.
		s.mu.Lock()
		if sess.AttachmentCancel != nil {
			sess.AttachmentCancel()
			sess.AttachmentCancel = nil
		}
		transitionLocked(sess, "read")
		s.mu.Unlock()
		s.prompt(sess, "Playback stopped. Press 8 for attachments, or pound to return.")
	case "move_menu":
		if key == "0" {
			s.mu.Lock()
			more := (sess.MovePage+1)*9 < len(sess.MoveFolders)
			if more {
				sess.MovePage++
			}
			s.mu.Unlock()
			if more {
				s.promptMoveMenu(sess)
			} else {
				s.prompt(sess, "There are no more folders. Press pound to return.")
				s.promptMoveMenu(sess)
			}
			return nil
		}
		if key < "1" || key > "9" {
			return nil
		}
		s.mu.Lock()
		index := sess.MovePage*9 + int(key[0]-'1')
		if index >= len(sess.MoveFolders) {
			s.mu.Unlock()
			s.prompt(sess, "There is no folder for that key. Press pound to return.")
			s.promptMoveMenu(sess)
			return nil
		}
		destination := sess.MoveFolders[index]
		s.mu.Unlock()
		s.moveCurrent(sess, destination)
	case "more_options":
		s.handleMoreOptions(sess, key)
	case "contact_confirm":
		if key == "1" {
			s.savePendingContact(sess)
		} else if key == "2" {
			s.mu.Lock()
			transitionLocked(sess, "more_options")
			s.mu.Unlock()
			s.prompt(sess, menuPrompt("more_options"))
		}
	case "confirm_delete":
		if key == "1" {
			s.deleteCurrent(sess)
		} else if key == "2" {
			s.mu.Lock()
			transitionLocked(sess, "list")
			var m store.MailSummary
			cursor, total := 0, 0
			if len(sess.Messages) > 0 {
				if sess.Cursor < 0 || sess.Cursor >= len(sess.Messages) {
					sess.Cursor = 0
				}
				m = sess.Messages[sess.Cursor]
				cursor, total = sess.Cursor, len(sess.Messages)
			}
			s.mu.Unlock()
			if total > 0 {
				s.prompt(sess, listPrompt(m, cursor, total))
			} else {
				s.prompt(sess, menuPrompt("list"))
			}
		}
	case "review":
		if key != "3" {
			s.mu.Lock()
			sess.PendingCancelConfirm = false
			s.mu.Unlock()
		}
		if key == "1" {
			s.sendDraft(sess)
		} else if key == "2" {
			s.saveDraft(sess)
		} else if key == "3" {
			s.handleReviewCancel(sess)
		} else if key == "4" {
			s.mu.Lock()
			transitionLocked(sess, "draft_edit")
			s.mu.Unlock()
			s.prompt(sess, menuPrompt("draft_edit"))
		} else if key == "5" {
			s.startAudioAttachment(sess)
		} else if key == "6" {
			s.prompt(sess, s.draftReviewPrompt(sess))
		}
	case "forward_options":
		if key == "1" || key == "2" {
			s.startCompose(sess, "forward")
			if key == "2" {
				s.mu.Lock()
				sess.Draft.Attachments = nil
				sess.Draft.ForwardOriginal = false
				s.mu.Unlock()
			}
			s.prompt(sess, "Forwarding. "+menuPrompt("compose"))
		}
	case "audio_recording":
		if key == "0" {
			s.mu.Lock()
			if sess.RecordStop != nil {
				close(sess.RecordStop)
				sess.RecordStop = nil
			}
			s.mu.Unlock()
		}
	case "recording", "recording_subject":
		if key == "0" {
			s.mu.Lock()
			if sess.RecordStop != nil {
				close(sess.RecordStop)
				sess.RecordStop = nil
			}
			s.mu.Unlock()
		}
	}
	return nil
}

// pinFailureNotice words the wrong-PIN prompt with the lockout countdown so
// the caller knows how many attempts remain before this number is locked out.
func pinFailureNotice(remaining int) string {
	if remaining == 1 {
		return "That PIN was not accepted. One attempt remains before this number is locked out. Try again, or press star to clear."
	}
	return fmt.Sprintf("That PIN was not accepted. %d attempts remain before lockout. Try again, or press star to clear.", remaining)
}

// handleReviewCancel implements the review menu's cancel key. A single press
// used to discard a fully typed email with no confirmation; the first press
// now asks, and the second discards.
func (s *Service) handleReviewCancel(sess *session) {
	s.mu.Lock()
	confirmed := sess.PendingCancelConfirm
	sess.PendingCancelConfirm = false
	s.mu.Unlock()
	if confirmed {
		s.mu.Lock()
		transitionLocked(sess, "main")
		s.mu.Unlock()
		s.prompt(sess, "Composition cancelled. "+s.promptForState(sess))
		return
	}
	s.mu.Lock()
	sess.PendingCancelConfirm = true
	s.mu.Unlock()
	s.prompt(sess, "Press 3 again to cancel this email, or pound to go back.")
}
