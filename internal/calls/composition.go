package calls

import (
	"context"
	"fmt"
	"net/mail"
	"os"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/keypad"
	"github.com/voxmail/voxmail/internal/mailer"
)

func (s *Service) startCompose(sess *session, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startComposeLocked(sess, mode)
}

func (s *Service) startComposeTo(sess *session, recipient string) {
	s.mu.Lock()
	sess.Draft = draft{To: recipient}
	sess.EditingDraft = false
	sess.RecipientKind = ""
	transitionLocked(sess, "subject_method")
	sess.Editor = nil
	s.mu.Unlock()
}

func (s *Service) startVoiceRecording(sess *session) {
	if s.Recorder == nil || sess == nil || sess.RxPath == "" {
		s.prompt(sess, "Voice composition is not available on this call.")
		return
	}
	parent := s.eventContext(sess)
	s.mu.Lock()
	transitionLocked(sess, "recording")
	stop := make(chan struct{})
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	sess.RecordStop = stop
	sess.RecordCancel = cancel
	s.mu.Unlock()
	s.prompt(sess, "Speak your message after the tone. Press 0 when finished, or pound to cancel.")
	info, err := os.Stat(sess.RxPath)
	if err != nil {
		cancel()
		s.mu.Lock()
		sess.RecordStop = nil
		sess.RecordCancel = nil
		transitionLocked(sess, "body_method")
		s.mu.Unlock()
		s.prompt(sess, "Voice composition is not ready yet.")
		return
	}
	offset := info.Size()
	s.startTask(func() {
		defer cancel()
		text, err := s.Recorder.RecordAndTranscribeUntil(ctx, sess.RxPath, offset, stop)
		s.mu.Lock()
		sess.RecordStop = nil
		sess.RecordCancel = nil
		if string(stateLocked(sess)) == "recording" && !sess.Closed {
			if err == nil {
				sess.Draft.Body = strings.TrimSpace(text)
				transitionLocked(sess, "review")
				sess.EditingDraft = false
			} else {
				transitionLocked(sess, "body_method")
				sess.Editor = nil
			}
		}
		s.mu.Unlock()
		if err != nil {
			s.prompt(sess, "I could not understand the recording. Choose keypad or speech entry again.")
			return
		}
		s.prompt(sess, menuPrompt("review"))
	})
}

func (s *Service) startSubjectVoice(sess *session) {
	if s.Recorder == nil || sess == nil || sess.RxPath == "" {
		s.prompt(sess, "Voice entry is not available on this call.")
		return
	}
	info, err := os.Stat(sess.RxPath)
	if err != nil {
		s.mu.Lock()
		transitionLocked(sess, "subject_method")
		s.mu.Unlock()
		s.prompt(sess, "Voice recording is not ready yet.")
		return
	}
	parent := s.eventContext(sess)
	s.mu.Lock()
	transitionLocked(sess, "recording_subject")
	offset := info.Size()
	stop := make(chan struct{})
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	sess.RecordStop = stop
	sess.RecordCancel = cancel
	s.mu.Unlock()
	s.prompt(sess, "Speak the subject after the tone. Press 0 when finished, or pound to cancel.")
	s.startTask(func() {
		defer cancel()
		text, recordErr := s.Recorder.RecordAndTranscribeUntil(ctx, sess.RxPath, offset, stop)
		s.mu.Lock()
		sess.RecordStop = nil
		sess.RecordCancel = nil
		if string(stateLocked(sess)) == "recording_subject" && !sess.Closed {
			if recordErr == nil {
				sess.Draft.Subject = strings.TrimSpace(text)
				if sess.EditingDraft {
					transitionLocked(sess, "review")
					sess.EditingDraft = false
				} else {
					transitionLocked(sess, "body_method")
				}
			} else {
				transitionLocked(sess, "subject_method")
			}
		}
		closed := sess.Closed
		s.mu.Unlock()
		if closed {
			return
		}
		if recordErr != nil {
			s.prompt(sess, "I could not understand the subject. Choose keypad or speech entry again.")
			return
		}
		s.mu.Lock()
		next := string(stateLocked(sess))
		s.mu.Unlock()
		s.prompt(sess, menuPrompt(next))
	})
}

func (s *Service) startAudioAttachment(sess *session) {
	if s.Recorder == nil || sess == nil || sess.RxPath == "" {
		s.prompt(sess, "Audio attachment recording is not available on this call.")
		return
	}
	info, err := os.Stat(sess.RxPath)
	if err != nil {
		s.prompt(sess, "Audio recording is not ready yet.")
		return
	}
	ctx, cancel := context.WithTimeout(s.eventContext(sess), 35*time.Second)
	s.mu.Lock()
	transitionLocked(sess, "audio_recording")
	sess.RecordCancel = cancel
	offset := info.Size()
	s.mu.Unlock()
	s.prompt(sess, fmt.Sprintf("Speak after the tone. Press 0 when finished. The recording is limited to %d seconds.", int(s.Recorder.audioWindow()/time.Second)))
	s.startTask(func() {
		data, recordErr := s.Recorder.RecordAudio(ctx, sess.RxPath, offset)
		cancel()
		s.mu.Lock()
		sess.RecordCancel = nil
		if string(stateLocked(sess)) == "audio_recording" && !sess.Closed {
			if recordErr == nil {
				sess.Draft.Attachments = append(sess.Draft.Attachments, mailer.Attachment{Filename: fmt.Sprintf("voxmail-audio-%d.wav", time.Now().Unix()), ContentType: "audio/wav", Data: data})
				transitionLocked(sess, "review")
			}
		}
		stillRecording := string(stateLocked(sess)) == "audio_recording"
		closed := sess.Closed
		s.mu.Unlock()
		if closed {
			return
		}
		if recordErr != nil {
			if stillRecording {
				s.mu.Lock()
				transitionLocked(sess, "review")
				s.mu.Unlock()
			}
			s.prompt(sess, "The audio attachment could not be recorded. Returning to review.")
			return
		}
		s.prompt(sess, "Audio attachment added. "+menuPrompt("review"))
	})
}
func (s *Service) startComposeLocked(sess *session, mode string) {
	sess.Draft = draft{}
	sess.EditingDraft = false
	sess.RecipientKind = ""
	if mode == "" {
		transitionLocked(sess, "compose")
		sess.Editor = newIVREditor("compose")
		return
	}
	// Reply and forward are intentionally local conveniences: the original
	// message remains untouched while its sender/subject seed the new draft.
	if len(sess.Messages) > 0 && sess.Cursor >= 0 && sess.Cursor < len(sess.Messages) {
		message := sess.Messages[sess.Cursor]
		if mode != "forward" {
			if address, err := mail.ParseAddress(message.Sender); err == nil {
				sess.Draft.To = address.Address
			} else {
				sess.Draft.To = message.Sender
			}
		}
		if mode == "replyall" {
			allRecipients := strings.TrimSpace(message.Recipients)
			if strings.TrimSpace(message.Cc) != "" {
				if allRecipients != "" {
					allRecipients += ", "
				}
				allRecipients += message.Cc
			}
			own := make(map[string]struct{})
			if s.Store != nil {
				if accounts, err := s.Store.ListAccounts(context.Background(), sess.UserID); err == nil {
					for _, account := range accounts {
						if address, err := mail.ParseAddress(account.Email); err == nil {
							own[strings.ToLower(address.Address)] = struct{}{}
						}
					}
				}
			}
			if recipients, err := mail.ParseAddressList(allRecipients); err == nil {
				seen := make(map[string]struct{})
				for _, recipient := range recipients {
					address := strings.ToLower(recipient.Address)
					if strings.EqualFold(recipient.Address, sess.Draft.To) || address == "" {
						continue
					}
					if _, exists := own[address]; exists {
						continue
					}
					if _, exists := seen[address]; exists {
						continue
					}
					seen[address] = struct{}{}
					sess.Draft.Cc = append(sess.Draft.Cc, recipient.Address)
				}
			}
		}
		prefix := "Re: "
		if mode == "forward" {
			prefix = "Fwd: "
			sess.Draft.Body = fmt.Sprintf("Forwarded message from %s. Subject: %s.", message.Sender, message.Subject)
			sess.Draft.ForwardOriginal = true
			messageID := message.ID
			sess.Draft.OriginalMessageID = &messageID
			if raw, err := os.ReadFile(message.Path); err == nil && len(raw) <= 25<<20 {
				sess.Draft.Attachments = []mailer.Attachment{{Filename: "forwarded-message.eml", ContentType: "message/rfc822", Data: raw}}
			}
		}
		sess.Draft.Subject = prefix + message.Subject
	}
	if mode == "forward" {
		transitionLocked(sess, "compose")
		sess.Editor = newIVREditor("compose")
		return
	}
	transitionLocked(sess, "subject_method")
	sess.Editor = nil
}

func (s *Service) handleCompose(sess *session, key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.Editor == nil {
		sess.Editor = newIVREditor(string(stateLocked(sess)))
	}
	_, done := sess.Editor.Press(key[0])
	if !done {
		return
	}
	if sess.Editor.Overflowed {
		s.startTask(func() { s.prompt(sess, "That entry is too long. Press star to delete a character, then try again.") })
		return
	}
	value := strings.TrimSpace(sess.Editor.Text)
	switch string(stateLocked(sess)) {
	case "compose":
		if _, err := mail.ParseAddress(value); err != nil {
			s.startTask(func() { s.prompt(sess, "That email address is not valid. Enter it again, then press pound.") })
			return
		}
		sess.ConfirmValue = value
		sess.ConfirmState = "compose"
		sess.ConfirmRecipientKind = ""
		transitionLocked(sess, "field_confirm")
		sess.Editor = nil
		s.startTask(func() { s.prompt(sess, fieldConfirmPrompt("compose", value)) })
	case "recipient_input":
		if _, err := mail.ParseAddress(value); err != nil {
			s.startTask(func() { s.prompt(sess, "That email address is not valid. Enter it again, then press pound.") })
			return
		}
		sess.ConfirmValue = value
		sess.ConfirmState = "recipient_input"
		sess.ConfirmRecipientKind = sess.RecipientKind
		transitionLocked(sess, "field_confirm")
		sess.Editor = nil
		s.startTask(func() { s.prompt(sess, fieldConfirmPrompt("recipient_input", value)) })
	case "subject":
		sess.ConfirmValue = value
		sess.ConfirmState = "subject"
		sess.ConfirmRecipientKind = ""
		transitionLocked(sess, "field_confirm")
		sess.Editor = nil
		s.startTask(func() { s.prompt(sess, fieldConfirmPrompt("subject", value)) })
	case "body":
		sess.ConfirmValue = value
		sess.ConfirmState = "body"
		sess.ConfirmRecipientKind = ""
		transitionLocked(sess, "field_confirm")
		sess.Editor = nil
		s.startTask(func() { s.prompt(sess, fieldConfirmPrompt("body", value)) })
	case "contact_name":
		if value == "" {
			s.startTask(func() { s.prompt(sess, "Enter a contact name, then press pound.") })
			return
		}
		sess.ConfirmValue = value
		sess.ConfirmState = "contact_name"
		sess.ConfirmRecipientKind = ""
		transitionLocked(sess, "field_confirm")
		sess.Editor = nil
		s.startTask(func() { s.prompt(sess, fieldConfirmPrompt("contact_name", value)) })
	}
}

func newIVREditor(state string) *keypad.MultiTap {
	mode := keypad.ModeText
	limit := maxIVRBodyRunes
	switch state {
	case "compose", "recipient_input":
		mode = keypad.ModeEmail
		limit = maxIVRRecipientRunes
	case "subject":
		limit = maxIVRSubjectRunes
	case "contact_name":
		limit = maxIVRContactNameRunes
	}
	return keypad.NewWithLimit(mode, limit)
}

func fieldConfirmPrompt(state, value string) string {
	if state == "compose" || state == "recipient_input" {
		var chars strings.Builder
		for _, r := range value {
			if chars.Len() > 0 {
				chars.WriteString(" ")
			}
			chars.WriteRune(r)
		}
		return fmt.Sprintf("I heard %s. Press 1 to accept or 2 to re-enter.", chars.String())
	}
	return fmt.Sprintf("I heard %s. Press 1 to accept or 2 to re-enter.", value)
}

func (s *Service) handleFieldConfirmation(sess *session, key string) {
	s.mu.Lock()
	value, state, recipientKind := sess.ConfirmValue, sess.ConfirmState, sess.ConfirmRecipientKind
	if value == "" || state == "" {
		s.mu.Unlock()
		return
	}
	if key == "2" {
		transitionLocked(sess, state)
		sess.Editor = newIVREditor(state)
		sess.ConfirmValue, sess.ConfirmState, sess.ConfirmRecipientKind = "", "", ""
		s.mu.Unlock()
		s.prompt(sess, menuPrompt(state))
		return
	}
	if key != "1" {
		s.mu.Unlock()
		return
	}
	switch state {
	case "compose":
		sess.Draft.To = value
		transitionLocked(sess, "recipient_menu")
		sess.Editor = nil
	case "recipient_input":
		switch recipientKind {
		case "to":
			sess.Draft.AdditionalTo = append(sess.Draft.AdditionalTo, value)
		case "cc":
			sess.Draft.Cc = append(sess.Draft.Cc, value)
		case "bcc":
			sess.Draft.Bcc = append(sess.Draft.Bcc, value)
		}
		transitionLocked(sess, "recipient_menu")
		sess.Editor = nil
	case "subject":
		sess.Draft.Subject = value
		if sess.EditingDraft {
			transitionLocked(sess, "review")
			sess.EditingDraft = false
		} else {
			transitionLocked(sess, "body_method")
		}
		sess.Editor = nil
	case "body":
		sess.Draft.Body = value
		transitionLocked(sess, "review")
		sess.EditingDraft = false
		sess.Editor = nil
	case "contact_name":
		sess.PendingContactName = value
		transitionLocked(sess, "contact_confirm")
		sess.Editor = nil
	}
	sess.ConfirmValue, sess.ConfirmState, sess.ConfirmRecipientKind = "", "", ""
	next := string(stateLocked(sess))
	s.mu.Unlock()
	if next == "contact_confirm" {
		s.mu.Lock()
		email := sess.PendingContactEmail
		s.mu.Unlock()
		s.prompt(sess, fmt.Sprintf("Save %s as a contact for %s? Press 1 to confirm or 2 to cancel.", value, email))
		return
	}
	s.prompt(sess, menuPrompt(next))
}

func (s *Service) handleMoreOptions(sess *session, key string) {
	s.mu.Lock()
	if sess.Cursor < 0 || sess.Cursor >= len(sess.Messages) {
		s.mu.Unlock()
		return
	}
	message := sess.Messages[sess.Cursor]
	s.mu.Unlock()
	switch key {
	case "1":
		address, err := mail.ParseAddress(message.Sender)
		if err != nil || address.Address == "" {
			s.prompt(sess, "The sender address could not be added.")
			return
		}
		s.mu.Lock()
		sess.PendingContactEmail = address.Address
		sess.PendingContactName = ""
		transitionLocked(sess, "contact_name")
		sess.Editor = newIVREditor("contact_name")
		s.mu.Unlock()
		s.prompt(sess, "Enter a name for this sender using the keypad, then press pound.")
	case "2":
		s.prompt(sess, "The message date is "+message.Date+". Press pound to return.")
	case "3":
		s.prompt(sess, "The recipients are "+message.Recipients+". Press pound to return.")
	case "4":
		s.prompt(sess, "The subject is "+message.Subject+". Press pound to return.")
	}
}
