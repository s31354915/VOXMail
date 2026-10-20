package calls

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/maintenance"
	"github.com/voxmail/voxmail/internal/process"
	"github.com/voxmail/voxmail/internal/speech"

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
		s.prompt(sess, "Voice composition is not available on this call. Press 1 to type your message, or pound to go back.")
		return
	}
	parent := s.eventContext(sess)
	s.mu.Lock()
	transitionLocked(sess, "recording")
	stop := make(chan struct{})
	ctx, cancel := context.WithCancel(parent)
	sess.RecordGeneration++
	generation := sess.RecordGeneration
	sess.RecordStop = stop
	sess.RecordCancel = cancel
	s.mu.Unlock()
	s.prompt(sess, fmt.Sprintf("You have up to %d seconds. After the tone, speak your message. Press 0 to finish, or pound to cancel and return to the message choices.", int(s.Recorder.speechWindow()/time.Second)))
	s.playRecordingTone(sess)
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
		recordCtx, finish := context.WithTimeout(ctx, s.Recorder.speechWindow()+process.ConversionTimeout+process.WhisperTimeout)
		defer finish()
		text, err := s.Recorder.RecordAndTranscribeUntilWithStatus(recordCtx, sess.RxPath, offset, stop, func() {
			_ = s.prompt(sess, "Recording finished. I’m transcribing your message now. This can take a little while. Please stay on the line; press pound to cancel.")
		})
		s.mu.Lock()
		active := sess.RecordGeneration == generation && string(stateLocked(sess)) == "recording" && !sess.Closed
		if sess.RecordGeneration == generation {
			sess.RecordStop = nil
			sess.RecordCancel = nil
		}
		empty := false
		if active {
			if err == nil && strings.TrimSpace(text) != "" {
				sess.Draft.Body = strings.TrimSpace(text)
				transitionLocked(sess, "review")
				sess.EditingDraft = false
			} else {
				empty = err == nil
				transitionLocked(sess, "body_method")
				sess.Editor = nil
			}
		}
		closed := sess.Closed
		s.mu.Unlock()
		if closed || !active {
			s.logDiscardedRecording(sess, closed, err)
			return
		}
		if err != nil {
			// Pressing pound cancels the recording on purpose; a failure
			// message after an intentional cancel reads as an error.
			if errors.Is(err, context.Canceled) {
				return
			}
			s.prompt(sess, "The recording could not be transcribed. Press 1 to type, 2 to record again, or pound to go back.")
			return
		}
		if empty {
			s.prompt(sess, "I did not hear anything in that recording. Press 1 to type, 2 to record again, or pound to go back.")
			return
		}
		s.prompt(sess, "Your message was transcribed. Press 6 at review to hear it before sending. "+menuPrompt("review"))
	})
}

func (s *Service) startSubjectVoice(sess *session) {
	if s.Recorder == nil || sess == nil || sess.RxPath == "" {
		s.prompt(sess, "Voice entry is not available on this call. Press 1 to type the subject, or pound to go back.")
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
	stop := make(chan struct{})
	ctx, cancel := context.WithCancel(parent)
	sess.RecordStop = stop
	sess.RecordGeneration++
	generation := sess.RecordGeneration
	sess.RecordCancel = cancel
	s.mu.Unlock()
	s.prompt(sess, fmt.Sprintf("You have up to %d seconds. After the tone, say the email subject. Press 0 to finish, or pound to cancel and return to the subject choices.", int(s.Recorder.speechWindow()/time.Second)))
	s.playRecordingTone(sess)
	info, err = os.Stat(sess.RxPath)
	if err != nil {
		cancel()
		s.mu.Lock()
		sess.RecordStop, sess.RecordCancel = nil, nil
		transitionLocked(sess, "subject_method")
		s.mu.Unlock()
		s.prompt(sess, "The recording could not start. "+menuPrompt("subject_method"))
		return
	}
	offset := info.Size()
	s.startTask(func() {
		defer cancel()
		recordCtx, finish := context.WithTimeout(ctx, s.Recorder.speechWindow()+process.ConversionTimeout+process.WhisperTimeout)
		defer finish()
		text, recordErr := s.Recorder.RecordAndTranscribeUntilWithStatus(recordCtx, sess.RxPath, offset, stop, func() {
			_ = s.prompt(sess, "Recording finished. I’m transcribing the subject now. Please stay on the line; press pound to cancel.")
		})
		s.mu.Lock()
		active := sess.RecordGeneration == generation && string(stateLocked(sess)) == "recording_subject" && !sess.Closed
		if sess.RecordGeneration == generation {
			sess.RecordStop = nil
			sess.RecordCancel = nil
		}
		if active {
			if recordErr == nil && strings.TrimSpace(text) != "" {
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
		empty := recordErr == nil && strings.TrimSpace(text) == ""
		closed := sess.Closed
		s.mu.Unlock()
		if closed || !active {
			s.logDiscardedRecording(sess, closed, recordErr)
			return
		}
		if recordErr != nil {
			if errors.Is(recordErr, context.Canceled) {
				return
			}
			s.prompt(sess, "The subject recording could not be transcribed. Press 1 to type, 2 to record again, or pound to go back.")
			return
		}
		if empty {
			s.prompt(sess, "I did not hear anything in that recording. Press 1 to type, 2 to record again, or pound to go back.")
			return
		}
		s.mu.Lock()
		next := string(stateLocked(sess))
		s.mu.Unlock()
		s.prompt(sess, "Subject: "+speakable(text, maxIVRSubjectRunes)+". "+menuPrompt(next))
	})
}

// logDiscardedRecording makes every exit from a voice recording that does not
// transcribe say why. Before this, a recording abandoned by a state change or
// a closed call dropped its captured audio without a trace in the logs.
func (s *Service) logDiscardedRecording(sess *session, closed bool, err error) {
	if s.Log == nil {
		return
	}
	reason := "superseded by a newer recording"
	if closed {
		reason = "call closed"
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		s.Log.Warn("recording result discarded", "call_id", sess.CallID, "reason", reason, "error", err.Error())
		return
	}
	s.Log.Warn("recording result discarded", "call_id", sess.CallID, "reason", reason)
}

// playRecordingTone gives callers a distinct cue for the start of capture.
// It runs after the spoken instructions and before the receive-file offset is
// recorded, so neither the tone nor the instructions enter the transcription.
func (s *Service) playRecordingTone(sess *session) {
	if s.Media == nil || sess == nil || sess.TxPath == "" {
		return
	}
	dir := s.Media.Dir
	if dir == "" {
		dir = filepath.Dir(sess.TxPath)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		s.prompt(sess, "The start tone is unavailable. Start speaking now.")
		return
	}
	f, release, err := maintenance.CreateTemp(dir, ".recording-tone-*.wav")
	if err != nil {
		s.prompt(sess, "The start tone is unavailable. Start speaking now.")
		return
	}
	defer release()
	defer os.Remove(f.Name())
	if _, err = f.Write(recordingToneWAV()); err == nil {
		err = f.Close()
	} else {
		_ = f.Close()
	}
	if err == nil {
		err = s.playPrompt(sess, func(ctx context.Context) error {
			return s.Media.playWAVTail(ctx, sess.TxPath, f.Name(), pcmDrainTail)
		})
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		if s.Log != nil {
			s.Log.Warn("recording start tone could not play", "error", err)
		}
		s.prompt(sess, "The start tone is unavailable. Start speaking now.")
	}
}

func recordingToneWAV() []byte {
	const sampleRate = 8000
	const samples = sampleRate / 8 // 125 ms, short enough to keep the call moving.
	dataBytes := samples * 2
	wav := make([]byte, 44+dataBytes)
	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(36+dataBytes))
	copy(wav[8:12], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], 1)
	binary.LittleEndian.PutUint32(wav[24:28], sampleRate)
	binary.LittleEndian.PutUint32(wav[28:32], sampleRate*2)
	binary.LittleEndian.PutUint16(wav[32:34], 2)
	binary.LittleEndian.PutUint16(wav[34:36], 16)
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(dataBytes))
	for i := 0; i < samples; i++ {
		envelope := math.Min(1, math.Min(float64(i)/80, float64(samples-1-i)/80))
		value := int16(math.Sin(2*math.Pi*880*float64(i)/sampleRate) * 0.22 * envelope * 32767)
		binary.LittleEndian.PutUint16(wav[44+i*2:46+i*2], uint16(value))
	}
	return wav
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
	ctx, cancel := context.WithCancel(s.eventContext(sess))
	stop := make(chan struct{})
	s.mu.Lock()
	transitionLocked(sess, "audio_recording")
	sess.RecordGeneration++
	generation := sess.RecordGeneration
	sess.RecordCancel = cancel
	sess.RecordStop = stop
	s.mu.Unlock()
	s.prompt(sess, fmt.Sprintf("You have up to %d seconds for this audio attachment. Press 0 to finish or pound to cancel. Start speaking after these instructions.", int(s.Recorder.audioWindow()/time.Second)))
	info, err = os.Stat(sess.RxPath)
	if err != nil {
		cancel()
		s.mu.Lock()
		sess.RecordStop, sess.RecordCancel = nil, nil
		transitionLocked(sess, "review")
		s.mu.Unlock()
		s.prompt(sess, "The recording could not start. "+menuPrompt("review"))
		return
	}
	offset := info.Size()
	s.startTask(func() {
		recordCtx, finish := context.WithTimeout(ctx, s.Recorder.audioWindow()+process.ConversionTimeout)
		defer finish()
		data, recordErr := s.Recorder.RecordAudioUntil(recordCtx, sess.RxPath, offset, stop)
		cancel()
		s.mu.Lock()
		active := sess.RecordGeneration == generation && string(stateLocked(sess)) == "audio_recording" && !sess.Closed
		if sess.RecordGeneration == generation {
			sess.RecordCancel = nil
			sess.RecordStop = nil
		}
		empty := false
		if active {
			if recordErr == nil && len(data) > 0 {
				sess.Draft.Attachments = append(sess.Draft.Attachments, mailer.Attachment{Filename: fmt.Sprintf("voxmail-audio-%d.wav", time.Now().Unix()), ContentType: "audio/wav", Data: data})
				transitionLocked(sess, "review")
			} else {
				empty = recordErr == nil
				transitionLocked(sess, "review")
			}
		}
		stillRecording := string(stateLocked(sess)) == "audio_recording"
		closed := sess.Closed
		s.mu.Unlock()
		if closed || !active {
			s.logDiscardedRecording(sess, closed, recordErr)
			return
		}
		if recordErr != nil {
			// An intentional cancel (pressing 0 to stop) must not read as a
			// failure.
			if errors.Is(recordErr, context.Canceled) {
				return
			}
			if stillRecording {
				s.mu.Lock()
				transitionLocked(sess, "review")
				s.mu.Unlock()
			}
			s.prompt(sess, "The audio attachment could not be recorded. Returning to review.")
			return
		}
		if empty {
			s.prompt(sess, "I did not hear anything in that recording. Returning to review.")
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
		// Say the field is full the moment it fills up, not only when the
		// caller presses pound: swallowed keystrokes otherwise look like
		// the keypad stopped working.
		if sess.Editor.Overflowed && !sess.Editor.OverflowPrompted {
			sess.Editor.OverflowPrompted = true
			s.startTask(func() { s.prompt(sess, "That entry is too long. Press star to delete a character, then try again.") })
		}
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
			s.startTask(func() {
				s.prompt(sess, "That address could not be accepted. Your entry is still here. Press star to delete the last character. For at, press 1 twice, then star; for dot, press 1 three times, then star. Press pound to check it again.")
			})
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
			s.startTask(func() {
				s.prompt(sess, "That address could not be accepted. Your entry is still here. Press star to delete the last character. For at, press 1 twice, then star; for dot, press 1 three times, then star. Press pound to check it again.")
			})
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
	label := map[string]string{"compose": "Email address", "recipient_input": "Email address", "subject": "Subject", "body": "Message", "contact_name": "Contact name"}[state]
	if label == "" {
		label = "Entry"
	}
	spoken := speakable(value, 1200)
	if state == "compose" || state == "recipient_input" {
		spoken = speech.SpellCharacters(value)
	}
	if strings.TrimSpace(value) == "" {
		spoken = "blank"
	}
	return fmt.Sprintf("%s: %s. Press 1 to accept, 2 to edit, or 3 to start this entry over.", label, spoken)
}

func (s *Service) handleFieldConfirmation(sess *session, key string) {
	s.mu.Lock()
	value, state, recipientKind := sess.ConfirmValue, sess.ConfirmState, sess.ConfirmRecipientKind
	if value == "" || state == "" {
		s.mu.Unlock()
		return
	}
	if key == "2" || key == "3" {
		transitionLocked(sess, state)
		sess.Editor = newIVREditor(state)
		// Re-entering used to start from a blank editor, throwing away the
		// text the caller had already typed. Seed the editor with the value
		// they just confirmed against so star-key edits work naturally.
		if sess.Editor != nil && key == "2" {
			sess.Editor.Text = value
		}
		sess.ConfirmValue, sess.ConfirmState, sess.ConfirmRecipientKind = "", "", ""
		s.mu.Unlock()
		notice := "Editing your previous entry. Press star to delete the last character. "
		if key == "3" {
			notice = "Entry cleared. "
		}
		s.prompt(sess, notice+menuPrompt(state))
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
		s.prompt(sess, menuPrompt("contact_name"))
	case "2":
		s.prompt(sess, "The message date is "+speakable(message.Date, 80)+". Press pound to return.")
	case "3":
		s.prompt(sess, "The recipients are "+speakable(message.Recipients, 400)+". Press pound to return.")
	case "4":
		s.prompt(sess, "The subject is "+speakable(message.Subject, 300)+". Press pound to return.")
	}
}

// speakable bounds a raw header value (a pathological recipient list or
// subject line cannot turn a menu read into minutes of uninterrupted speech)
// and sanitizes it for text-to-speech.
func speakable(value string, max int) string {
	text, truncated := truncateForSpeech(value, max)
	if truncated {
		text += ", and more"
	}
	return speech.SanitizeNonLatin(text)
}
