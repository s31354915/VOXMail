package calls

import (
	"fmt"
	"net"
	"strings"

	"github.com/voxmail/voxmail/internal/auth"
	"github.com/voxmail/voxmail/internal/bridge"
	"github.com/voxmail/voxmail/internal/ivr"
)

func (s *Service) handleDTMF(conn net.Conn, message bridge.Message) error {
	s.mu.Lock()
	sess := s.sessions[message.CallID]
	if sess == nil || sess.Closed {
		s.mu.Unlock()
		return nil
	}
	s.armInactivityTimeoutLocked(sess)
	if sess.Authenticated {
		s.mu.Unlock()
		return s.handleMenu(conn, sess, message)
	}
	if message.Digit == "*" {
		sess.PIN = ""
		s.mu.Unlock()
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
	if err != nil || !auth.Check(user.PINHash, pin) {
		s.mu.Lock()
		sess.PIN = ""
		sess.Failures++
		if sess.Failures >= 3 {
			phone := sess.Phone
			s.mu.Unlock()
			s.recordPinFailure(phone)
			if s.Log != nil {
				s.Log.Warn("ivr PIN locked out", "call_id", message.CallID, "phone", phone)
			}
			return s.sendBridge(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 603, Reason: "PIN verification failed"})
		}
		s.mu.Unlock()
		s.prompt(sess, "That PIN was not accepted. Try again, or press star to clear.")
		return nil
	}
	s.mu.Lock()
	sess.Authenticated = true
	sess.Failures = 0
	if sess.Flow == nil {
		sess.Flow = ivr.NewSession(sess.CallID)
		sess.Flow.State = ivr.StatePIN
	}
	sess.Flow.Enter(ivr.StateMain)
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
		return "You are signed in. Press 1 for email, 2 for settings, or 3 for information and instructions."
	}
	total := 0
	parts := make([]string, 0, len(accounts))
	for _, account := range accounts {
		count, listErr := s.Store.CountUnreadForAccount(s.baseContext(), sess.UserID, account.ID)
		if listErr != nil {
			continue
		}
		total += count
		if count > 0 {
			parts = append(parts, fmt.Sprintf("%d in %s", count, account.CanonicalName))
		}
	}
	if len(parts) == 0 {
		return "You have no unread email. Press 1 for email, 2 for settings, or 3 for information and instructions."
	}
	return fmt.Sprintf("You have %d unread emails, %s. Press 1 for email, 2 for settings, or 3 for information and instructions.", total, strings.Join(parts, ", "))
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
		if string(stateLocked(sess)) != previousState {
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
		if state == "attachment_playback" && sess.AttachmentCancel != nil {
			sess.AttachmentCancel()
		}
		if state == "audio_recording" && sess.RecordCancel != nil {
			sess.RecordCancel()
		}
		if (state == "recording" || state == "recording_subject") && sess.RecordCancel != nil {
			sess.RecordCancel()
			sess.RecordCancel = nil
		}
		s.mu.Unlock()
		s.prompt(sess, s.promptForState(sess))
		return nil
	}
	if key == "#" {
		s.mu.Lock()
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
			if sess.RecordCancel != nil {
				sess.RecordCancel()
				sess.RecordCancel = nil
			}
			backUsingFlowLocked(sess, "review")
		case "recording", "recording_subject":
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
		s.prompt(sess, s.promptForState(sess))
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
	case "info":
		if key == "1" {
			s.prompt(sess, menuPrompt("info"))
		}
	case "list":
		s.mu.Lock()
		if len(sess.Messages) == 0 {
			s.mu.Unlock()
			return nil
		}
		m := sess.Messages[sess.Cursor]
		switch key {
		case "1":
			transitionLocked(sess, "read")
		case "2":
			sess.Cursor = (sess.Cursor + 1) % len(sess.Messages)
			m = sess.Messages[sess.Cursor]
		case "3":
			sess.Cursor = (sess.Cursor + len(sess.Messages) - 1) % len(sess.Messages)
			m = sess.Messages[sess.Cursor]
		case "4":
			transitionLocked(sess, "confirm_delete")
		case "5":
			s.startComposeLocked(sess, "reply")
		case "6":
			s.startComposeLocked(sess, "forward")
		}
		state, cursor, total := string(stateLocked(sess)), sess.Cursor, len(sess.Messages)
		s.mu.Unlock()
		if state == "read" {
			s.readMessage(sess, m)
		} else if state == "confirm_delete" {
			s.prompt(sess, "Delete this message? Press 1 to confirm or 2 to cancel.")
		} else if state == "compose" {
			s.prompt(sess, "Enter the recipient using multi tap, then press pound.")
		} else if state == "subject" {
			s.prompt(sess, "Enter the subject using multi tap, then press pound.")
		} else if state == "subject_method" {
			s.prompt(sess, menuPrompt("subject_method"))
		} else if state == "body" {
			s.prompt(sess, "Enter the message using multi tap, then press pound.")
		} else {
			s.prompt(sess, listPrompt(m, cursor, total))
		}
	case "read":
		s.mu.Lock()
		if len(sess.Messages) == 0 {
			s.mu.Unlock()
			return nil
		}
		m := sess.Messages[sess.Cursor]
		s.mu.Unlock()
		if key == "1" {
			s.readMessage(sess, m)
		} else if key == "2" {
			s.toggleRead(sess, m)
		} else if key == "3" {
			s.startCompose(sess, "reply")
			s.prompt(sess, "Replying. "+menuPrompt("subject_method"))
		} else if key == "4" {
			s.startCompose(sess, "replyall")
			s.prompt(sess, "Reply all. "+menuPrompt("subject_method"))
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
			s.mu.Lock()
			transitionLocked(sess, "more_options")
			s.mu.Unlock()
			s.prompt(sess, menuPrompt("more_options"))
		} else if key == "0" {
			s.mu.Lock()
			sess.Cursor = (sess.Cursor + 1) % len(sess.Messages)
			m = sess.Messages[sess.Cursor]
			s.mu.Unlock()
			s.readMessage(sess, m)
		}
	case "attachment_menu":
		if key == "0" {
			s.mu.Lock()
			sess.AttachmentPage++
			s.mu.Unlock()
			s.attachmentMenu(sess)
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
			if (sess.MovePage+1)*9 < len(sess.MoveFolders) {
				sess.MovePage++
			}
			s.mu.Unlock()
			s.promptMoveMenu(sess)
			return nil
		}
		if key < "1" || key > "9" {
			return nil
		}
		s.mu.Lock()
		index := sess.MovePage*9 + int(key[0]-'1')
		if index >= len(sess.MoveFolders) {
			s.mu.Unlock()
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
			s.mu.Unlock()
			s.prompt(sess, menuPrompt("list"))
		}
	case "review":
		if key == "1" {
			s.sendDraft(sess)
		} else if key == "2" {
			s.saveDraft(sess)
		} else if key == "3" {
			s.mu.Lock()
			transitionLocked(sess, "main")
			s.mu.Unlock()
			s.prompt(sess, "Composition cancelled.")
		} else if key == "4" {
			s.mu.Lock()
			transitionLocked(sess, "draft_edit")
			s.mu.Unlock()
			s.prompt(sess, menuPrompt("draft_edit"))
		} else if key == "5" {
			s.startAudioAttachment(sess)
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
			s.prompt(sess, "Forwarding. Enter the recipient using multi tap, then press pound.")
		}
	case "audio_recording":
		if key == "0" {
			s.mu.Lock()
			if sess.RecordCancel != nil {
				sess.RecordCancel()
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
