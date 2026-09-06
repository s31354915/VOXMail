package calls

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	Recorder *VoiceRecorder
	DataRoot string
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
	Accounts      []store.Account
	Contacts      []store.Contact
	Folders       []string
	ActiveAccount string
	Editor        *keypad.MultiTap
	Draft         draft
	TxPath        string
	RxPath        string
	Lease         *speech.Lease
	Runtime       *speech.Runtime
	Closed        bool
	promptMu      sync.Mutex
}

type draft struct{ To, Subject, Body string }

type VoiceRecorder struct {
	Runtime *speech.Runtime
	Whisper speech.Whisper
	Binary  string
	Dir     string
	Window  time.Duration
}

func (r *VoiceRecorder) RecordAndTranscribe(ctx context.Context, rawPath string, offset int64) (string, error) {
	if r == nil || rawPath == "" {
		return "", fmt.Errorf("voice recorder is not configured")
	}
	window := r.Window
	if window <= 0 {
		window = 15 * time.Second
	}
	timer := time.NewTimer(window)
	select {
	case <-timer.C:
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return "", ctx.Err()
	}
	info, err := os.Stat(rawPath)
	if err != nil {
		return "", err
	}
	if offset < 0 || offset > info.Size() {
		return "", fmt.Errorf("invalid recording offset")
	}
	dir := r.Dir
	if dir == "" {
		dir = filepath.Dir(rawPath)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	raw, err := os.Open(rawPath)
	if err != nil {
		return "", err
	}
	defer raw.Close()
	if _, err := raw.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	segment, err := os.CreateTemp(dir, ".recording-*.pcm")
	if err != nil {
		return "", err
	}
	segmentPath := segment.Name()
	defer os.Remove(segmentPath)
	if _, err := io.CopyN(segment, raw, info.Size()-offset); err != nil && err != io.EOF {
		segment.Close()
		return "", err
	}
	if err := segment.Close(); err != nil {
		return "", err
	}
	wav, err := os.CreateTemp(dir, ".recording-*.wav")
	if err != nil {
		return "", err
	}
	wavPath := wav.Name()
	_ = wav.Close()
	defer os.Remove(wavPath)
	binary := r.Binary
	if binary == "" {
		binary = "ffmpeg"
	}
	convert := exec.CommandContext(ctx, binary, "-nostdin", "-f", "s16le", "-ar", "8000", "-ac", "1", "-i", segmentPath, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", "-y", wavPath)
	if output, err := convert.CombinedOutput(); err != nil {
		return "", fmt.Errorf("recording conversion: %w: %s", err, strings.TrimSpace(string(output)))
	}
	whisper := r.Whisper
	if r.Runtime != nil {
		if err := r.Runtime.Wait(ctx); err != nil {
			return "", err
		}
		whisper = r.Runtime.Whisper
	}
	return whisper.TranscribeAndRemove(ctx, wavPath)
}

// PromptPlayer turns short IVR prompts into 8 kHz signed PCM and writes them
// to the call's baresip FIFO. Calls are serialized per FIFO so prompts cannot
// overlap when a user barges in during a menu.
type PromptPlayer struct {
	Piper        speech.Piper
	Runtime      *speech.Runtime
	Pool         *speech.RuntimePool
	Store        *store.Store
	GreetingPath string
	Static       map[string]string
	StaticVoice  string
	Binary       string
	Dir          string
}

func (p *PromptPlayer) Activate() *speech.Lease {
	if p == nil || p.Runtime == nil {
		return nil
	}
	return p.Runtime.Activate()
}

func (p *PromptPlayer) ActivateForUser(userID string) (*speech.Runtime, *speech.Lease) {
	if p == nil {
		return nil, nil
	}
	voice, speed := "en_US-hfc_male-medium", 3
	if p.Store != nil {
		_ = p.Store.DB.QueryRowContext(context.Background(), `SELECT tts_voice,menu_speed FROM settings WHERE user_id=?`, userID).Scan(&voice, &speed)
	}
	if p.Pool != nil {
		runtime, lease := p.Pool.Activate(voice, speed)
		return runtime, lease
	}
	return p.Runtime, p.Activate()
}

func (p *PromptPlayer) Play(ctx context.Context, fifo, text string) error {
	return p.PlayWithRuntime(ctx, p.Runtime, fifo, text)
}

func (p *PromptPlayer) PlayWithRuntime(ctx context.Context, runtime *speech.Runtime, fifo, text string) error {
	if p == nil || fifo == "" || text == "" {
		return nil
	}
	if p.Binary == "" {
		p.Binary = "ffmpeg"
	}
	if p.Dir == "" {
		p.Dir = filepath.Dir(fifo)
	}
	staticVoiceMatches := runtime == nil || p.StaticVoice == "" || strings.TrimSuffix(filepath.Base(runtime.Piper.Model), filepath.Ext(runtime.Piper.Model)) == p.StaticVoice
	if static := p.Static[text]; static != "" && staticVoiceMatches {
		if _, err := os.Stat(static); err == nil {
			return p.playWAV(ctx, fifo, static)
		}
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
	if runtime != nil {
		if err := runtime.Synthesize(ctx, text, wav); err != nil {
			return err
		}
		return p.playWAV(ctx, fifo, wav)
	}
	if err := p.Piper.Synthesize(ctx, text, wav); err != nil {
		return err
	}
	return p.playWAV(ctx, fifo, wav)
}

func (p *PromptPlayer) PlayGreeting(ctx context.Context, fifo string) error {
	if p == nil || p.GreetingPath == "" {
		return fmt.Errorf("static greeting is not configured")
	}
	return p.playWAV(ctx, fifo, p.GreetingPath)
}

func (p *PromptPlayer) playWAV(ctx context.Context, fifo, wav string) error {
	binary := p.Binary
	if binary == "" {
		binary = "ffmpeg"
	}
	pipe, err := os.OpenFile(fifo, os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer pipe.Close()
	cmd := exec.CommandContext(ctx, binary, "-nostdin", "-i", wav, "-ar", "8000", "-ac", "1", "-f", "s16le", "pipe:1")
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
			sess := s.sessions[message.CallID]
			if sess != nil {
				sess.Closed = true
			}
			delete(s.sessions, message.CallID)
			s.mu.Unlock()
			if sess != nil && sess.Lease != nil {
				sess.Lease.Release()
			}
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
		s.sessions[message.CallID] = &session{UserID: user.ID, State: "pin", TxPath: message.TxPath, RxPath: message.RxPath}
	}
	s.mu.Unlock()
	if busy {
		return bridge.Encode(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 486, Reason: "all lines are busy"})
	}
	if err := bridge.Encode(conn, bridge.Message{Type: "answer", CallID: message.CallID, UserID: user.ID}); err != nil {
		return err
	}
	s.mu.Lock()
	sess := s.sessions[message.CallID]
	if s.Media != nil {
		sess.Runtime, sess.Lease = s.Media.ActivateForUser(user.ID)
	}
	s.mu.Unlock()
	go s.greet(sess)
	return nil
}

func (s *Service) greet(sess *session) {
	if sess == nil || s.Media == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if s.Media.GreetingPath != "" {
		if err := s.Media.PlayGreeting(ctx, sess.TxPath); err == nil {
			return
		} else if s.Log != nil {
			s.Log.Warn("static greeting unavailable; falling back to synthesis", "error", err)
		}
	}
	s.prompt(sess, "Welcome to VOXMail. Enter your PIN, then press pound.")
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
		sess.Failures = 0
		s.mu.Unlock()
		if s.Log != nil {
			s.Log.Info("ivr PIN accepted", "call_id", message.CallID, "user_id", sess.UserID)
		}
		sess.State = "main"
		s.prompt(sess, "You are signed in. Press 1 for unread mail, 2 for all mail, 3 for accounts, 4 for contacts, 5 to compose, or 6 for settings.")
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
		case "compose", "accounts", "contacts", "settings":
			sess.State = "main"
		case "list":
			if sess.ActiveAccount != "" {
				sess.State = "folders"
			} else {
				sess.State = "main"
			}
		case "folders":
			sess.State = "accounts"
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
			s.openAccounts(sess)
		case "4":
			s.openContacts(sess)
		case "5":
			s.startCompose(sess, "")
			s.prompt(sess, "Enter the recipient using multi tap, then press pound.")
		case "6":
			s.mu.Lock()
			sess.State = "settings"
			s.mu.Unlock()
			s.prompt(sess, "Settings. Press 1 for voice settings, 2 for call alerts, or pound to return.")
		}
	case "accounts":
		s.handleAccountMenu(sess, key)
	case "folders":
		s.handleFolderMenu(sess, key)
	case "contacts":
		s.handleContactMenu(sess, key)
	case "settings":
		s.handleSettingsMenu(sess, key)
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
		} else if key == "3" {
			s.startVoiceRecording(sess)
		}
	}
	return nil
}

func (s *Service) openAccounts(sess *session) {
	accounts, err := s.Store.ListAccounts(context.Background(), sess.UserID)
	if err != nil || len(accounts) == 0 {
		s.prompt(sess, "No mail accounts are configured.")
		return
	}
	s.mu.Lock()
	sess.Accounts = accounts
	sess.State = "accounts"
	sess.ActiveAccount = ""
	s.mu.Unlock()
	s.prompt(sess, accountPrompt(accounts))
}

func (s *Service) handleAccountMenu(sess *session, key string) {
	if key < "1" || key > "9" {
		return
	}
	s.mu.Lock()
	index := int(key[0] - '1')
	if index >= len(sess.Accounts) {
		s.mu.Unlock()
		return
	}
	account := sess.Accounts[index]
	sess.ActiveAccount = account.ID
	s.mu.Unlock()
	folders, err := s.Store.ListMailFolders(context.Background(), sess.UserID, account.ID)
	if err != nil {
		s.prompt(sess, "Folders are temporarily unavailable.")
		return
	}
	seen := make(map[string]bool, len(folders))
	for _, folder := range folders {
		seen[folder] = true
	}
	var folderMap map[string]string
	if json.Unmarshal([]byte(account.FolderMap), &folderMap) == nil {
		for remote, local := range folderMap {
			if local != "" && !seen[local] {
				folders = append(folders, local)
				seen[local] = true
			}
			if remote != "" && !seen[remote] {
				folders = append(folders, remote)
				seen[remote] = true
			}
		}
	}
	sort.Strings(folders)
	s.mu.Lock()
	sess.Folders = folders
	sess.State = "folders"
	s.mu.Unlock()
	if len(folders) == 0 {
		s.prompt(sess, "This account has no synchronized folders.")
	} else {
		s.prompt(sess, folderPrompt(account, folders))
	}
}

func (s *Service) handleFolderMenu(sess *session, key string) {
	if key < "1" || key > "9" {
		return
	}
	s.mu.Lock()
	index := int(key[0] - '1')
	if index >= len(sess.Folders) {
		s.mu.Unlock()
		return
	}
	folder := sess.Folders[index]
	accountID := sess.ActiveAccount
	s.mu.Unlock()
	mails, err := s.Store.ListMailForAccount(context.Background(), sess.UserID, accountID, folder, false)
	if err != nil {
		s.prompt(sess, "Mail is temporarily unavailable.")
		return
	}
	s.mu.Lock()
	sess.Messages, sess.Cursor, sess.State = mails, 0, "list"
	s.mu.Unlock()
	if len(mails) == 0 {
		s.prompt(sess, "There are no messages in that folder.")
	} else {
		s.prompt(sess, listPrompt(mails[0], 0, len(mails)))
	}
}

func (s *Service) openContacts(sess *session) {
	contacts, err := s.Store.ListContacts(context.Background(), sess.UserID)
	if err != nil || len(contacts) == 0 {
		s.prompt(sess, "You have no contacts.")
		return
	}
	s.mu.Lock()
	sess.Contacts = contacts
	sess.State = "contacts"
	s.mu.Unlock()
	s.prompt(sess, contactPrompt(contacts))
}

func (s *Service) handleContactMenu(sess *session, key string) {
	if key < "1" || key > "9" {
		return
	}
	s.mu.Lock()
	index := int(key[0] - '1')
	if index >= len(sess.Contacts) {
		s.mu.Unlock()
		return
	}
	contact := sess.Contacts[index]
	s.mu.Unlock()
	s.startComposeTo(sess, contact.Email)
	s.prompt(sess, fmt.Sprintf("Composing to %s. Enter the subject, then press pound.", contact.Name))
}

func (s *Service) handleSettingsMenu(sess *session, key string) {
	switch key {
	case "1":
		s.prompt(sess, "Voice model and speech speed are managed in the web console.")
	case "2":
		var enabled int
		if err := s.Store.DB.QueryRowContext(context.Background(), `SELECT alerts_enabled FROM settings WHERE user_id=?`, sess.UserID).Scan(&enabled); err != nil {
			s.prompt(sess, "Alert settings are unavailable.")
			return
		}
		enabled = 1 - enabled
		if _, err := s.Store.DB.ExecContext(context.Background(), `UPDATE settings SET alerts_enabled=? WHERE user_id=?`, enabled, sess.UserID); err != nil {
			s.prompt(sess, "Alert settings could not be changed.")
			return
		}
		if enabled == 1 {
			s.prompt(sess, "Call alerts are now enabled.")
		} else {
			s.prompt(sess, "Call alerts are now disabled.")
		}
	}
}

func accountPrompt(accounts []store.Account) string {
	parts := make([]string, 0, len(accounts))
	for i, account := range accounts {
		if i == 9 {
			break
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s", i+1, account.CanonicalName))
	}
	return "Accounts. " + strings.Join(parts, ". ") + ". Press pound to go back."
}

func folderPrompt(account store.Account, folders []string) string {
	parts := make([]string, 0, len(folders))
	for i, folder := range folders {
		if i == 9 {
			break
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s", i+1, mappedFolder(account, folder)))
	}
	return "Folders for " + account.CanonicalName + ". " + strings.Join(parts, ". ") + ". Press pound to go back."
}

func contactPrompt(contacts []store.Contact) string {
	parts := make([]string, 0, len(contacts))
	for i, contact := range contacts {
		if i == 9 {
			break
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s", i+1, contact.Name))
	}
	return "Contacts. " + strings.Join(parts, ". ") + ". Press pound to go back."
}

func mappedFolder(account store.Account, folder string) string {
	var mapping map[string]string
	if json.Unmarshal([]byte(account.FolderMap), &mapping) == nil {
		if local := mapping[folder]; local != "" {
			return local
		}
		for remote, local := range mapping {
			if local == folder && remote != "" {
				return local
			}
		}
	}
	return folder
}

func (s *Service) startCompose(sess *session, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startComposeLocked(sess, mode)
}

func (s *Service) startComposeTo(sess *session, recipient string) {
	s.mu.Lock()
	sess.Draft = draft{To: recipient}
	sess.State = "subject"
	sess.Editor = keypad.New(keypad.ModeText)
	s.mu.Unlock()
}

func (s *Service) startVoiceRecording(sess *session) {
	if s.Recorder == nil || sess == nil || sess.RxPath == "" {
		s.prompt(sess, "Voice composition is not available on this call.")
		return
	}
	s.mu.Lock()
	sess.State = "recording"
	s.mu.Unlock()
	s.prompt(sess, "Speak your message after the tone. Recording will stop automatically.")
	info, err := os.Stat(sess.RxPath)
	if err != nil {
		s.mu.Lock()
		sess.State = "body"
		s.mu.Unlock()
		s.prompt(sess, "Voice composition is not ready yet.")
		return
	}
	go func(offset int64) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		text, err := s.Recorder.RecordAndTranscribe(ctx, sess.RxPath, offset)
		s.mu.Lock()
		if sess.State == "recording" && !sess.Closed {
			if err == nil {
				sess.Draft.Body = strings.TrimSpace(text)
				sess.State = "review"
			} else {
				sess.State = "body"
				sess.Editor = keypad.New(keypad.ModeText)
			}
		}
		s.mu.Unlock()
		if err != nil {
			s.prompt(sess, "I could not understand the recording. Use the keypad or try again.")
			return
		}
		s.prompt(sess, "Voice message captured. Press 1 to send, 2 to edit, or pound to go back.")
	}(info.Size())
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
	if len(parsed.Attachments) > 0 {
		body += fmt.Sprintf(" There are %d attachments.", len(parsed.Attachments))
	}
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
	accountRoot := ""
	if s.DataRoot != "" {
		accountRoot = filepath.Join(s.DataRoot, "mail", m.AccountID)
	} else {
		accountRoot = filepath.Dir(filepath.Dir(filepath.Dir(m.Path)))
	}
	rootAbs, rootErr := filepath.Abs(accountRoot)
	pathAbs, pathErr := filepath.Abs(m.Path)
	if rootErr != nil || pathErr != nil || (pathAbs != rootAbs && !strings.HasPrefix(pathAbs, rootAbs+string(filepath.Separator))) {
		s.prompt(sess, "I could not delete that message.")
		return
	}
	trashName := "Trash"
	if accounts, err := s.Store.ListAccounts(context.Background(), sess.UserID); err == nil {
		for _, account := range accounts {
			if account.ID != m.AccountID {
				continue
			}
			var mapping map[string]string
			if json.Unmarshal([]byte(account.FolderMap), &mapping) == nil {
				for remote, local := range mapping {
					if local != "" && (strings.Contains(strings.ToLower(remote), "trash") || strings.Contains(strings.ToLower(remote), "deleted")) {
						trashName = local
					}
				}
			}
		}
	}
	trash := filepath.Join(rootAbs, trashName, "cur")
	if !strings.HasPrefix(filepath.Clean(trash), rootAbs+string(filepath.Separator)) {
		s.prompt(sess, "I could not delete that message.")
		return
	}
	if err := os.MkdirAll(trash, 0700); err != nil {
		s.prompt(sess, "I could not delete that message.")
		return
	}
	target := filepath.Join(trash, filepath.Base(pathAbs))
	if !strings.Contains(target, ":2,") {
		target += ":2,S"
	}
	err := os.Rename(pathAbs, target)
	if err == nil {
		if indexErr := s.Store.DeleteMailIndex(context.Background(), sess.UserID, m.ID); indexErr != nil {
			err = indexErr
		}
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
	s.mu.Lock()
	activeAccount := sess.ActiveAccount
	s.mu.Unlock()
	if activeAccount != "" {
		for _, candidate := range accounts {
			if candidate.ID == activeAccount {
				a = candidate
				break
			}
		}
	}
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
	if err := s.Media.PlayWithRuntime(ctx, sess.Runtime, sess.TxPath, text); err != nil && s.Log != nil {
		s.Log.Warn("ivr prompt failed", "error", err)
	}
}

func menuPrompt(state string) string {
	switch state {
	case "main":
		return "Press 1 for unread mail, 2 for all mail, 3 for accounts, 4 for contacts, 5 to compose, or 6 for settings."
	case "accounts":
		return "Choose an account or press pound to go back."
	case "folders":
		return "Choose a folder or press pound to go back."
	case "contacts":
		return "Choose a contact or press pound to go back."
	case "settings":
		return "Press 1 for voice settings, 2 to toggle call alerts, or pound to go back."
	case "list":
		return "Press 1 to read, 2 for next, 3 for previous, 4 to delete, 5 to reply, or pound to go back."
	case "review":
		return "Press 1 to send, 2 to edit, 3 to record the message by voice, or pound to go back."
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
