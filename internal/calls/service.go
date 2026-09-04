package calls

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/voxmail/voxmail/internal/auth"
	"github.com/voxmail/voxmail/internal/bridge"
	"github.com/voxmail/voxmail/internal/keypad"
	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/speech"
	"github.com/voxmail/voxmail/internal/store"
)

type Service struct {
	Socket   string
	Store    *store.Store
	Log      *slog.Logger
	MaxCalls int
	Media    *PromptPlayer
	Secrets  *secret.Box
	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	UserID        string
	PIN           string
	Failures      int
	Authenticated bool
	State         string
	Messages      []store.MailSummary
	Cursor        int
	Editor        *keypad.MultiTap
	Draft         draft
	TxPath        string
	promptMu      sync.Mutex
}

type draft struct{ To, Subject, Body string }

// PromptPlayer turns short IVR prompts into 8 kHz signed PCM and writes them
// to the call's baresip FIFO. Calls are serialized per FIFO so prompts cannot
// overlap when a user barges in during a menu.
type PromptPlayer struct {
	Piper  speech.Piper
	Binary string
	Dir    string
}

func (p *PromptPlayer) Play(ctx context.Context, fifo, text string) error {
	if p == nil || fifo == "" || text == "" {
		return nil
	}
	if p.Binary == "" {
		p.Binary = "ffmpeg"
	}
	if p.Dir == "" {
		p.Dir = filepath.Dir(fifo)
	}
	if err := os.MkdirAll(p.Dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(p.Dir, ".prompt-*.wav")
	if err != nil {
		return err
	}
	wav := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(wav)
	if err := p.Piper.Synthesize(ctx, text, wav); err != nil {
		return err
	}
	pipe, err := os.OpenFile(fifo, os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer pipe.Close()
	cmd := exec.CommandContext(ctx, p.Binary, "-nostdin", "-i", wav, "-ar", "8000", "-ac", "1", "-f", "s16le", "pipe:1")
	cmd.Stdout = pipe
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("prompt conversion: %w", err)
	}
	return nil
}

func (s *Service) Run(ctx context.Context) error {
	for {
		if err := s.runConnection(ctx); err != nil && !errors.Is(err, context.Canceled) && s.Log != nil {
			s.Log.Warn("baresip bridge disconnected", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
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
	reader := bufio.NewReader(conn)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		message, err := bridge.Decode(reader)
		if err != nil {
			return err
		}
		switch message.Type {
		case "call_incoming":
			if err := s.admit(conn, message); err != nil {
				return err
			}
		case "call_closed":
			s.mu.Lock()
			delete(s.sessions, message.CallID)
			s.mu.Unlock()
			if s.Log != nil {
				s.Log.Info("call closed", "call_id", message.CallID, "reason", message.Reason)
			}
		case "dtmf":
			if err := s.handleDTMF(conn, message); err != nil {
				return err
			}
		}
	}
}

func (s *Service) admit(conn net.Conn, message bridge.Message) error {
	if s.Store == nil {
		return bridge.Encode(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 500, Reason: "server unavailable"})
	}
	user, err := s.Store.UserByPhone(context.Background(), normalizePhone(message.From))
	if err != nil || !user.Enabled {
		return bridge.Encode(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 603, Reason: "caller not authorized"})
	}
	s.mu.Lock()
	busy := s.MaxCalls > 0 && len(s.sessions) >= s.MaxCalls
	if !busy {
		s.sessions[message.CallID] = &session{UserID: user.ID, State: "pin", TxPath: message.TxPath}
	}
	s.mu.Unlock()
	if busy {
		return bridge.Encode(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 486, Reason: "all lines are busy"})
	}
	if err := bridge.Encode(conn, bridge.Message{Type: "answer", CallID: message.CallID, UserID: user.ID}); err != nil {
		return err
	}
	go s.prompt(s.sessions[message.CallID], "Welcome to VOXMail. Enter your PIN, then press pound.")
	return nil
}

func (s *Service) handleDTMF(conn net.Conn, message bridge.Message) error {
	s.mu.Lock()
	sess := s.sessions[message.CallID]
	s.mu.Unlock()
	if sess == nil {
		return nil
	}
	if sess.Authenticated {
		return s.handleMenu(conn, sess, message)
	}
	if message.Digit == "*" {
		sess.PIN = ""
		return nil
	}
	if message.Digit != "#" {
		if len(sess.PIN) < 12 && len(message.Digit) == 1 && message.Digit[0] >= '0' && message.Digit[0] <= '9' {
			sess.PIN += message.Digit
		}
		return nil
	}
	user, err := s.Store.UserByID(context.Background(), sess.UserID)
	if err == nil && auth.Check(user.PINHash, sess.PIN) {
		s.mu.Lock()
		sess.Authenticated = true
		s.mu.Unlock()
		if s.Log != nil {
			s.Log.Info("ivr PIN accepted", "call_id", message.CallID, "user_id", sess.UserID)
		}
		sess.State = "main"
		s.prompt(sess, "You are signed in. Press 1 for unread mail, 2 for all mail, 3 to compose, or 4 for contacts.")
		return nil
	}
	sess.PIN = ""
	sess.Failures++
	if sess.Failures >= 3 {
		return bridge.Encode(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 603, Reason: "PIN verification failed"})
	}
	s.prompt(sess, "That PIN was not accepted. Try again, or press star to clear.")
	return nil
}

func (s *Service) handleMenu(conn net.Conn, sess *session, message bridge.Message) error {
	key := message.Digit
	if key == "" {
		return nil
	}
	s.mu.Lock()
	editing := sess.State == "compose" || sess.State == "subject" || sess.State == "body"
	s.mu.Unlock()
	if editing {
		s.handleCompose(sess, key)
		return nil
	}
	if key == "*" {
		s.prompt(sess, menuPrompt(sess.State))
		return nil
	}
	if key == "#" {
		s.mu.Lock()
		switch sess.State {
		case "list", "compose":
			sess.State = "main"
		case "read", "confirm_delete":
			sess.State = "list"
		case "subject":
			sess.State = "compose"
		case "body":
			sess.State = "subject"
		case "review":
			sess.State = "body"
		}
		s.mu.Unlock()
		s.prompt(sess, menuPrompt(sess.State))
		return nil
	}
	switch sess.State {
	case "main":
		switch key {
		case "1", "2":
			mails, err := s.Store.ListMail(context.Background(), sess.UserID, key == "1")
			if err != nil {
				s.prompt(sess, "Mail is temporarily unavailable.")
				return nil
			}
			s.mu.Lock()
			sess.Messages, sess.Cursor, sess.State = mails, 0, "list"
			s.mu.Unlock()
			if len(mails) == 0 {
				s.prompt(sess, "There are no messages in that view.")
			} else {
				s.prompt(sess, listPrompt(mails[0], 0, len(mails)))
			}
		case "3":
			s.startCompose(sess, "")
			s.prompt(sess, "Enter the recipient using multi tap, then press pound.")
		case "4":
			contacts, _ := s.Store.ListContacts(context.Background(), sess.UserID)
			if len(contacts) == 0 {
				s.prompt(sess, "You have no contacts.")
			} else {
				s.prompt(sess, fmt.Sprintf("You have %d contacts. %s", len(contacts), contacts[0].Name))
			}
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
			sess.State = "read"
		case "2":
			sess.Cursor = (sess.Cursor + 1) % len(sess.Messages)
			m = sess.Messages[sess.Cursor]
		case "3":
			sess.Cursor = (sess.Cursor + len(sess.Messages) - 1) % len(sess.Messages)
			m = sess.Messages[sess.Cursor]
		case "4":
			sess.State = "confirm_delete"
		case "5":
			s.startComposeLocked(sess, "reply")
		case "6":
			s.startComposeLocked(sess, "forward")
		}
		state, cursor, total := sess.State, sess.Cursor, len(sess.Messages)
		s.mu.Unlock()
		if state == "read" {
			s.readMessage(sess, m)
		} else if state == "confirm_delete" {
			s.prompt(sess, "Delete this message? Press 1 to confirm or 2 to cancel.")
		} else if state == "compose" {
			s.prompt(sess, "Enter the recipient using multi tap, then press pound.")
		} else if state == "subject" {
			s.prompt(sess, "Enter the subject using multi tap, then press pound.")
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
			s.mu.Lock()
			sess.Cursor = (sess.Cursor + 1) % len(sess.Messages)
			m = sess.Messages[sess.Cursor]
			s.mu.Unlock()
			s.readMessage(sess, m)
		} else if key == "4" {
			s.mu.Lock()
			sess.State = "confirm_delete"
			s.mu.Unlock()
			s.prompt(sess, "Delete this message? Press 1 to confirm or 2 to cancel.")
		} else if key == "5" {
			s.startCompose(sess, "reply")
			s.prompt(sess, "Review the reply subject, then press pound for the message.")
		}
	case "confirm_delete":
		if key == "1" {
			s.deleteCurrent(sess)
		} else if key == "2" {
			s.mu.Lock()
			sess.State = "list"
			s.mu.Unlock()
			s.prompt(sess, menuPrompt("list"))
		}
	case "compose", "subject", "body":
		s.handleCompose(sess, key)
	case "review":
		if key == "1" {
			s.sendDraft(sess)
		} else if key == "2" {
			s.mu.Lock()
			sess.State = "body"
			sess.Editor = keypad.New(keypad.ModeText)
			s.mu.Unlock()
			s.prompt(sess, "Edit the message, then press pound.")
		}
	}
	return nil
}

func (s *Service) startCompose(sess *session, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startComposeLocked(sess, mode)
}
func (s *Service) startComposeLocked(sess *session, mode string) {
	sess.Draft = draft{}
	if mode == "" {
		sess.State = "compose"
		sess.Editor = keypad.New(keypad.ModeEmail)
		return
	}
	// Reply and forward are intentionally local conveniences: the original
	// message remains untouched while its sender/subject seed the new draft.
	if len(sess.Messages) > 0 && sess.Cursor >= 0 && sess.Cursor < len(sess.Messages) {
		message := sess.Messages[sess.Cursor]
		if address, err := mail.ParseAddress(message.Sender); err == nil {
			sess.Draft.To = address.Address
		} else {
			sess.Draft.To = message.Sender
		}
		prefix := "Re: "
		if mode == "forward" {
			prefix = "Fwd: "
			sess.Draft.Body = fmt.Sprintf("Forwarded message from %s. Subject: %s.", message.Sender, message.Subject)
		}
		sess.Draft.Subject = prefix + message.Subject
	}
	sess.State = "subject"
	sess.Editor = keypad.New(keypad.ModeText)
	sess.Editor.Text = sess.Draft.Subject
}

func (s *Service) handleCompose(sess *session, key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.Editor == nil {
		sess.Editor = keypad.New(keypad.ModeText)
	}
	_, done := sess.Editor.Press(key[0])
	if !done {
		return
	}
	value := strings.TrimSpace(sess.Editor.Text)
	switch sess.State {
	case "compose":
		sess.Draft.To = value
		sess.State = "subject"
		sess.Editor = keypad.New(keypad.ModeText)
		go s.prompt(sess, "Enter the subject, then press pound.")
	case "subject":
		sess.Draft.Subject = value
		sess.State = "body"
		sess.Editor = keypad.New(keypad.ModeText)
		sess.Editor.Text = sess.Draft.Body
		go s.prompt(sess, "Enter the message, then press pound.")
	case "body":
		sess.Draft.Body = value
		sess.State = "review"
		go s.prompt(sess, "Press 1 to send, 2 to edit, or pound to go back.")
	}
}

func (s *Service) readMessage(sess *session, m store.MailSummary) {
	file, err := os.Open(m.Path)
	if err != nil {
		s.prompt(sess, "That message is no longer available.")
		return
	}
	parsed, err := mailparse.Parse(file)
	_ = file.Close()
	_ = s.Store.MarkMailRead(context.Background(), sess.UserID, m.ID, true)
	if err != nil {
		s.prompt(sess, "I could not read that message.")
		return
	}
	body := parsed.Text
	if len([]rune(body)) > 2800 {
		body = string([]rune(body)[:2800]) + ". Message truncated."
	}
	s.prompt(sess, fmt.Sprintf("From %s. Subject %s. %s Press 2 for next, 4 to delete, or pound to return.", parsed.From, parsed.Subject, body))
}

func (s *Service) deleteCurrent(sess *session) {
	s.mu.Lock()
	if len(sess.Messages) == 0 {
		s.mu.Unlock()
		return
	}
	m := sess.Messages[sess.Cursor]
	s.mu.Unlock()
	trash := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(m.Path))), "Trash", "cur")
	_ = os.MkdirAll(trash, 0700)
	target := filepath.Join(trash, filepath.Base(m.Path))
	if !strings.Contains(target, ":2,") {
		target += ":2,S"
	}
	err := os.Rename(m.Path, target)
	if err == nil {
		_ = s.Store.DeleteMailIndex(context.Background(), sess.UserID, m.ID)
	}
	s.mu.Lock()
	sess.State = "list"
	s.mu.Unlock()
	if err == nil {
		s.prompt(sess, "Moved to trash.")
	} else {
		s.prompt(sess, "I could not delete that message.")
	}
}

func (s *Service) sendDraft(sess *session) {
	s.mu.Lock()
	d := sess.Draft
	s.mu.Unlock()
	address, err := mail.ParseAddress(d.To)
	if err != nil {
		s.prompt(sess, "That recipient is not valid.")
		return
	}
	accounts, err := s.Store.ListAccounts(context.Background(), sess.UserID)
	if err != nil || len(accounts) == 0 || s.Secrets == nil {
		s.prompt(sess, "No sending account is configured.")
		return
	}
	a := accounts[0]
	password, err := s.Secrets.Open(a.SMTPPassword)
	if err != nil {
		s.prompt(sess, "The sending account is unavailable.")
		return
	}
	raw := mailer.BuildMessage(a.Email, a.SenderName, []string{address.Address}, nil, nil, d.Subject, d.Body)
	err = mailer.Send(mailer.Config{Host: a.SMTPHost, Port: a.SMTPPort, Username: a.SMTPUser, Password: password, From: a.Email}, []string{address.Address}, raw)
	if err != nil {
		s.prompt(sess, "Sending failed. Check the account settings.")
		return
	}
	s.mu.Lock()
	sess.State = "main"
	s.mu.Unlock()
	s.prompt(sess, "Message sent.")
}

func (s *Service) prompt(sess *session, text string) {
	if s.Media == nil || sess == nil || sess.TxPath == "" {
		return
	}
	sess.promptMu.Lock()
	defer sess.promptMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := s.Media.Play(ctx, sess.TxPath, text); err != nil && s.Log != nil {
		s.Log.Warn("ivr prompt failed", "error", err)
	}
}

func menuPrompt(state string) string {
	switch state {
	case "main":
		return "Press 1 for unread mail, 2 for all mail, 3 to compose, or 4 for contacts."
	case "list":
		return "Press 1 to read, 2 for next, 3 for previous, 4 to delete, 5 to reply, or pound to go back."
	case "review":
		return "Press 1 to send, 2 to edit, or pound to go back."
	case "compose":
		return "Enter the recipient using multi tap, then press pound."
	case "subject":
		return "Enter the subject using multi tap, then press pound."
	case "body":
		return "Enter the message using multi tap, then press pound."
	}
	return "Press pound to go back or star to repeat."
}
func listPrompt(m store.MailSummary, cursor, total int) string {
	return fmt.Sprintf("Message %d of %d. From %s. Subject %s. Press 1 to read.", cursor+1, total, m.Sender, m.Subject)
}

// SIP providers commonly deliver the caller as sip:+15551212@host; the
// whitelist stores the stable telephone identity only.
func normalizePhone(value string) string {
	value = strings.TrimSpace(value)
	if at := strings.IndexByte(value, '@'); at >= 0 {
		value = value[:at]
	}
	if colon := strings.LastIndexByte(value, ':'); colon >= 0 {
		value = value[colon+1:]
	}
	if semi := strings.IndexByte(value, ';'); semi >= 0 {
		value = value[:semi]
	}
	return value
}
