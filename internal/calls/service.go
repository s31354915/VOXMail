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
	"syscall"
	"time"

	"github.com/voxmail/voxmail/internal/auth"
	"github.com/voxmail/voxmail/internal/bridge"
	remoteimap "github.com/voxmail/voxmail/internal/imap"
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
	// DraftAppender is an optional seam for the remote Drafts append operation;
	// production uses the authenticated IMAP client, while integration tests
	// can verify the round-trip without requiring a live provider.
	DraftAppender     func(context.Context, store.Account, string, []byte) error
	InactivityTimeout time.Duration
	// InactivityHangupDelay allows the goodbye prompt to finish before the
	// bridge receives hangup. Zero uses the production default.
	InactivityHangupDelay time.Duration
	mu                    sync.Mutex
	sessions              map[string]*session
	client                *bridge.Client
	pending               map[string]DialRequest
	ctx                   context.Context

	pinMu   sync.Mutex
	pinLock map[string]time.Time
}

// AccountRefresher is implemented by the mail synchronization service.  It is
// deliberately a small interface so the call/IVR layer can request a manual
// refresh without importing the synchronization package or coupling the two
// service lifecycles.
type AccountRefresher interface {
	RefreshAccount(context.Context, string, string) error
}

// pinCooldown is how long a caller is locked out after failing the IVR PIN
// three times, across separate sessions from the same number.
const pinCooldown = 15 * time.Minute

// DialRequest describes an outgoing call. Outgoing calls back a subscribed
// phone number instead of admitting a caller, so the session is attached to
// the caller's account and plays Text once the far end answers.
type DialRequest struct {
	RequestID string      `json:"request_id"`
	URI       string      `json:"uri"`
	UserID    string      `json:"user_id"`
	AccountID string      `json:"account_id"`
	Text      string      `json:"text"`
	Done      chan<- bool `json:"-"`
}

// Dial asks baresip to place an outgoing call to URI. The request is queued
// until the matching call_outgoing event arrives, which then becomes an
// established session with Text played once the far end picks up.
func (s *Service) Dial(ctx context.Context, req DialRequest) error {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if c == nil {
		return errors.New("bridge is not connected")
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
	time.AfterFunc(callTimeout, func() {
		s.expirePending(requestID)
	})
	dialCtx, cancel := context.WithTimeout(ctx, bridgeWriteTimeout)
	err := c.DialWithRequestID(dialCtx, req.RequestID, strings.TrimSpace(req.URI))
	cancel()
	if err != nil {
		s.mu.Lock()
		if s.pending != nil {
			delete(s.pending, req.RequestID)
		}
		s.mu.Unlock()
		return err
	}
	if s.Log != nil {
		s.Log.Info("outgoing call dialed", "uri", req.URI, "user_id", req.UserID, "account_id", req.AccountID)
	}
	return nil
}

func (s *Service) expirePending(requestID string) {
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
		s.Log.Warn("outgoing call request expired before baresip created a call", "request_id", requestID, "uri", pending.URI)
	}
	signalAlert(pending.Done, false)
}

func (s *Service) hangup(callID string) error {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if c == nil {
		return errors.New("bridge is not connected")
	}
	return c.Send(bridge.Message{Type: "hangup", CallID: callID})
}

// InvalidateUserSessions immediately ends every active call belonging to a
// user. It is used when the phone PIN changes so an already-authenticated call
// cannot remain usable with credentials that have just been rotated.
func (s *Service) InvalidateUserSessions(userID string) {
	if s == nil || strings.TrimSpace(userID) == "" {
		return
	}
	s.mu.Lock()
	var revoked []*session
	for callID, sess := range s.sessions {
		if sess == nil || sess.UserID != userID {
			continue
		}
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
		delete(s.sessions, callID)
		revoked = append(revoked, sess)
	}
	client := s.client
	s.mu.Unlock()
	for _, sess := range revoked {
		if client != nil {
			_ = client.Send(bridge.Message{Type: "hangup", CallID: sess.CallID, Code: 603, Reason: "PIN changed"})
		}
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
	Flow                 *ivr.Session
	Closed               bool
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

// bridgeWriteTimeout bounds the synchronous part of placing an outbound
// call. The call itself may remain active for callTimeout, but an unavailable
// baresip socket must fail promptly.
const bridgeWriteTimeout = 5 * time.Second

type VoiceRecorder struct {
	Runtime *speech.Runtime
	Whisper speech.Whisper
	Binary  string
	Dir     string
	Window  time.Duration
}

func (r *VoiceRecorder) RecordAndTranscribe(ctx context.Context, rawPath string, offset int64) (string, error) {
	return r.recordAndTranscribe(ctx, rawPath, offset, nil)
}

// RecordAndTranscribeUntil captures until the caller signals stop, the
// configured maximum window expires, or the context is cancelled.  A stop
// signal is a normal completion path; context cancellation remains an abort
// path for call teardown and the # navigation key.
func (r *VoiceRecorder) RecordAndTranscribeUntil(ctx context.Context, rawPath string, offset int64, stop <-chan struct{}) (string, error) {
	return r.recordAndTranscribe(ctx, rawPath, offset, stop)
}

func (r *VoiceRecorder) recordAndTranscribe(ctx context.Context, rawPath string, offset int64, stop <-chan struct{}) (string, error) {
	if r == nil || rawPath == "" {
		return "", fmt.Errorf("voice recorder is not configured")
	}
	window := r.Window
	if window <= 0 {
		window = 15 * time.Second
	}
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-stop:
	case <-ctx.Done():
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

// RecordAudio captures a bounded raw call segment and returns a normal WAV
// attachment. It deliberately does not run Whisper; callers who want speech
// composition use RecordAndTranscribe instead.
func (r *VoiceRecorder) RecordAudio(ctx context.Context, rawPath string, offset int64) ([]byte, error) {
	if r == nil || rawPath == "" {
		return nil, fmt.Errorf("voice recorder is not configured")
	}
	window := r.Window
	if window <= 0 || window > 60*time.Second {
		window = 30 * time.Second
	}
	timer := time.NewTimer(window)
	select {
	case <-timer.C:
	case <-ctx.Done():
		// Cancellation is also the DTMF "finished" signal. Capture the
		// bytes written so far; the caller-close cleanup path discards the
		// result after the goroutine observes sess.Closed.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	info, err := os.Stat(rawPath)
	if err != nil {
		return nil, err
	}
	if offset < 0 || offset > info.Size() {
		return nil, fmt.Errorf("invalid recording offset")
	}
	dir := r.Dir
	if dir == "" {
		dir = filepath.Dir(rawPath)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	segment, err := os.CreateTemp(dir, ".attachment-*.pcm")
	if err != nil {
		return nil, err
	}
	segmentPath := segment.Name()
	defer os.Remove(segmentPath)
	input, err := os.Open(rawPath)
	if err != nil {
		segment.Close()
		return nil, err
	}
	if _, err := input.Seek(offset, io.SeekStart); err != nil {
		input.Close()
		segment.Close()
		return nil, err
	}
	_, copyErr := io.Copy(segment, io.LimitReader(input, int64(window/time.Second)*16000))
	input.Close()
	if err := segment.Close(); err != nil {
		return nil, err
	}
	if copyErr != nil {
		return nil, copyErr
	}
	wav, err := os.CreateTemp(dir, ".attachment-*.wav")
	if err != nil {
		return nil, err
	}
	wavPath := wav.Name()
	_ = wav.Close()
	defer os.Remove(wavPath)
	binary := r.Binary
	if binary == "" {
		binary = "ffmpeg"
	}
	convertCtx, convertCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer convertCancel()
	convert := exec.CommandContext(convertCtx, binary, "-nostdin", "-f", "s16le", "-ar", "8000", "-ac", "1", "-i", segmentPath, "-ar", "8000", "-ac", "1", "-c:a", "pcm_s16le", "-y", wavPath)
	if output, err := convert.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("audio attachment conversion: %w: %s", err, strings.TrimSpace(string(output)))
	}
	data, err := os.ReadFile(wavPath)
	if err != nil {
		return nil, err
	}
	if len(data) > 2<<20 {
		return nil, fmt.Errorf("audio attachment exceeds size limit")
	}
	return data, nil
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
	staticMu     sync.RWMutex
	StaticByKey  map[string]map[string]string
}

type pcmLimitWriter struct {
	w   io.Writer
	n   int64
	max int64
}

func (w *pcmLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.max-w.n {
		return 0, fmt.Errorf("decoded audio exceeds playback limit")
	}
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func openPCMWriter(ctx context.Context, path string) (*os.File, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("audio output path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	isFIFO := info.Mode()&os.ModeNamedPipe != 0
	for {
		file, openErr := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
		if openErr == nil {
			if isFIFO {
				// Once a reader is present, restore blocking writes so a slow
				// baresip callback does not turn ordinary audio into EAGAIN.
				if nonblockErr := syscall.SetNonblock(int(file.Fd()), false); nonblockErr != nil {
					_ = file.Close()
					return nil, nonblockErr
				}
			}
			return file, nil
		}
		var errno syscall.Errno
		if !isFIFO || (!errors.As(openErr, &errno) || (errno != syscall.ENXIO && errno != syscall.EAGAIN && errno != syscall.EWOULDBLOCK)) {
			return nil, openErr
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *PromptPlayer) Activate() *speech.Lease {
	if p == nil || p.Runtime == nil {
		return nil
	}
	return p.Runtime.Activate()
}

func (p *PromptPlayer) ActivateForUser(userID string) (*speech.Runtime, *speech.Lease) {
	menuRuntime, menuLease, _, _ := p.ActivateForUserSpeeds(userID)
	return menuRuntime, menuLease
}

func (p *PromptPlayer) ActivateForUserSpeeds(userID string) (*speech.Runtime, *speech.Lease, *speech.Runtime, *speech.Lease) {
	if p == nil {
		return nil, nil, nil, nil
	}
	voice, menuSpeed, emailSpeed := "en_US-hfc_male-medium", 3, 2
	if p.Store != nil {
		_ = p.Store.DB.QueryRowContext(context.Background(), `SELECT tts_voice,menu_speed,email_speed FROM settings WHERE user_id=?`, userID).Scan(&voice, &menuSpeed, &emailSpeed)
	}
	if p.Pool != nil {
		menuRuntime, menuLease := p.Pool.Activate(voice, menuSpeed)
		emailRuntime, emailLease := p.Pool.Activate(voice, emailSpeed)
		return menuRuntime, menuLease, emailRuntime, emailLease
	}
	return p.Runtime, p.Activate(), p.Runtime, p.Activate()
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
	p.staticMu.RLock()
	staticVoice := p.StaticVoice
	staticAssets := p.Static
	staticMuAssets := staticAssets[text]
	if runtime != nil && p.StaticByKey != nil {
		staticMuAssets = ""
		if assets := p.StaticByKey[staticRuntimeKey(runtime)]; assets != nil {
			staticMuAssets = assets[text]
			staticVoice = ""
		}
	}
	p.staticMu.RUnlock()
	staticVoiceMatches := runtime == nil || staticVoice == "" || strings.TrimSuffix(filepath.Base(runtime.Piper.Model), filepath.Ext(runtime.Piper.Model)) == staticVoice
	if static := staticMuAssets; static != "" && staticVoiceMatches {
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

// SetStaticPrompts replaces the fixed-prompt index only after a complete
// regeneration has succeeded. The call player can therefore be updated while
// other calls are still reading the previous map.
func (p *PromptPlayer) SetStaticPrompts(assets map[string]string, voice string) {
	p.SetStaticPromptsForSpeed(assets, voice, 3)
}

func (p *PromptPlayer) SetStaticPromptsForSpeed(assets map[string]string, voice string, speed int) {
	if p == nil {
		return
	}
	copyAssets := make(map[string]string, len(assets))
	for text, path := range assets {
		copyAssets[text] = path
	}
	p.staticMu.Lock()
	if p.StaticByKey == nil {
		p.StaticByKey = make(map[string]map[string]string)
	}
	p.StaticByKey[voice+"|"+fmt.Sprint(speed)] = copyAssets
	if p.Static == nil || (speed == 3 && (p.StaticVoice == "" || p.StaticVoice == voice)) {
		p.Static = copyAssets
		p.StaticVoice = voice
	}
	p.staticMu.Unlock()
}

func staticRuntimeKey(runtime *speech.Runtime) string {
	if runtime == nil {
		return ""
	}
	voice := strings.TrimSuffix(filepath.Base(runtime.Piper.Model), filepath.Ext(runtime.Piper.Model))
	return voice + "|" + fmt.Sprint(runtime.Speed)
}

func (p *PromptPlayer) PlayGreeting(ctx context.Context, fifo string) error {
	return p.PlayGreetingWithRuntime(ctx, nil, fifo)
}

func (p *PromptPlayer) PlayGreetingWithRuntime(ctx context.Context, runtime *speech.Runtime, fifo string) error {
	if p == nil {
		return fmt.Errorf("static greeting is not configured")
	}
	if runtime != nil {
		p.staticMu.RLock()
		var greeting string
		if assets := p.StaticByKey[staticRuntimeKey(runtime)]; assets != nil {
			greeting = assets[speech.StaticWelcomeText]
		}
		p.staticMu.RUnlock()
		if greeting != "" {
			if _, err := os.Stat(greeting); err == nil {
				return p.playWAV(ctx, fifo, greeting)
			}
		}
	}
	if p.GreetingPath == "" {
		return fmt.Errorf("static greeting is not configured")
	}
	return p.playWAV(ctx, fifo, p.GreetingPath)
}

// PlayMedia converts an extracted audio or video attachment to the raw PCM
// format expected by the per-call baresip FIFO. Video is intentionally
// rendered audio-only; no video frames are ever sent to a phone call.
func (p *PromptPlayer) PlayMedia(ctx context.Context, fifo, input string) error {
	return p.playInput(ctx, fifo, input)
}

func (p *PromptPlayer) playWAV(ctx context.Context, fifo, wav string) error {
	return p.playInput(ctx, fifo, wav)
}

func (p *PromptPlayer) playInput(ctx context.Context, fifo, input string) error {
	if p == nil {
		return fmt.Errorf("prompt player is not configured")
	}
	binary := p.Binary
	if binary == "" {
		binary = "ffmpeg"
	}
	const maxPlaybackDuration = 10 * time.Minute
	mediaCtx, cancel := context.WithTimeout(ctx, maxPlaybackDuration)
	defer cancel()
	pipe, err := openPCMWriter(mediaCtx, fifo)
	if err != nil {
		return err
	}
	defer pipe.Close()
	cmd := exec.CommandContext(mediaCtx, binary, "-nostdin", "-i", input, "-vn", "-ar", "8000", "-ac", "1", "-f", "s16le", "pipe:1")
	cmd.Stdout = &pcmLimitWriter{w: pipe, max: 8_000 * 2 * 600}
	if err := cmd.Run(); err != nil {
		if mediaCtx.Err() != nil {
			return mediaCtx.Err()
		}
		return fmt.Errorf("prompt conversion: %w", err)
	}
	return nil
}

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
	s.mu.Lock()
	s.client = bridge.NewClient(conn)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.client = nil
		s.pending = nil
		sessions := s.sessions
		s.sessions = make(map[string]*session)
		s.mu.Unlock()
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
			s.releaseSessionSpeech(sess)
		}
	}()
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
		case "call_outgoing":
			if err := s.startOutgoing(conn, message); err != nil {
				return err
			}
		case "call_established":
			s.onEstablished(message)
		case "call_closed":
			s.mu.Lock()
			sess := s.sessions[message.CallID]
			var alertDone chan<- bool
			if sess != nil {
				sess.Closed = true
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
			s.releaseSessionSpeech(sess)
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

// onEstablished triggers playback that must follow media establishment. RTP
// flows only once the audio stream starts, so greetings and alert text are
// deferred from admission/dial to this point rather than written into a FIFO
// that baresip has not opened yet.
func (s *Service) onEstablished(message bridge.Message) {
	s.mu.Lock()
	sess := s.sessions[message.CallID]
	sessState := ""
	if sess != nil {
		sessState = sess.State
	}
	s.mu.Unlock()
	if sess == nil {
		return
	}
	switch sessState {
	case "outgoing":
		go s.playOutgoingAlert(sess)
	default:
		go s.greet(sess)
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

func (s *Service) admit(conn net.Conn, message bridge.Message) error {
	if s.Store == nil {
		return bridge.Encode(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 500, Reason: "server unavailable"})
	}
	phone := normalizePhone(message.From)
	if locked := s.pinLockedUntil(phone); !locked.IsZero() {
		return bridge.Encode(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 603, Reason: "caller is temporarily locked out"})
	}
	user, err := s.Store.UserByPhone(s.baseContext(), phone)
	if err != nil || !user.Enabled {
		return bridge.Encode(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 603, Reason: "caller not authorized"})
	}
	s.mu.Lock()
	busy := s.MaxCalls > 0 && len(s.sessions) >= s.MaxCalls
	if !busy {
		flow := ivr.NewSession(message.CallID)
		flow.State = ivr.StatePIN
		s.sessions[message.CallID] = &session{CallID: message.CallID, UserID: user.ID, Phone: phone, State: "pin", Flow: flow, TxPath: message.TxPath, RxPath: message.RxPath}
		s.armInactivityTimeoutLocked(s.sessions[message.CallID])
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
		sess.Runtime, sess.Lease, sess.EmailRuntime, sess.EmailLease = s.Media.ActivateForUserSpeeds(user.ID)
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) pinLockedUntil(phone string) time.Time {
	if phone == "" {
		return time.Time{}
	}
	now := time.Now()
	s.pinMu.Lock()
	until := time.Time{}
	if s.pinLock != nil {
		until = s.pinLock[phone]
	}
	s.pinMu.Unlock()
	if until.After(now) {
		return until
	}
	if !until.IsZero() {
		s.pinMu.Lock()
		delete(s.pinLock, phone)
		s.pinMu.Unlock()
	}
	if s.Store != nil {
		if persisted, err := s.Store.PinLockout(context.Background(), phone); err == nil && persisted.After(now) {
			s.pinMu.Lock()
			if s.pinLock == nil {
				s.pinLock = make(map[string]time.Time)
			}
			s.pinLock[phone] = persisted
			s.pinMu.Unlock()
			return persisted
		}
	}
	return time.Time{}
}

func (s *Service) recordPinFailure(phone string) {
	if phone == "" {
		return
	}
	until := time.Now().Add(pinCooldown)
	s.pinMu.Lock()
	if s.pinLock == nil {
		s.pinLock = make(map[string]time.Time)
	}
	s.pinLock[phone] = until
	s.pinMu.Unlock()
	if s.Store != nil {
		_ = s.Store.SetPinLockout(context.Background(), phone, until)
	}
}

func (s *Service) clearPinFailure(phone string) {
	if phone == "" {
		return
	}
	s.pinMu.Lock()
	delete(s.pinLock, phone)
	s.pinMu.Unlock()
	if s.Store != nil {
		_ = s.Store.ClearPinLockout(context.Background(), phone)
	}
}

// startOutgoing turns a call_outgoing event into a session. The pending dial
// request supplies the callee's account and the text to read once the far end
// answers. Baresip rings until the remote phone picks up; an unanswered
// outgoing session is torn down after the call timeout.
func (s *Service) startOutgoing(conn net.Conn, message bridge.Message) error {
	s.mu.Lock()
	var req DialRequest
	if message.RequestID != "" && s.pending != nil {
		if candidate, ok := s.pending[message.RequestID]; ok {
			req = candidate
			delete(s.pending, message.RequestID)
		}
	}
	// Protocol-v1 shims did not carry request IDs. Keep a compatibility
	// fallback for a single outstanding request, but never silently choose from
	// several concurrent requests.
	if req.UserID == "" && message.RequestID == "" && len(s.pending) == 1 {
		for requestID, candidate := range s.pending {
			req = candidate
			delete(s.pending, requestID)
			break
		}
	}
	s.mu.Unlock()
	sess := &session{CallID: message.CallID, UserID: req.UserID, State: "outgoing", AlertText: req.Text, AlertDone: req.Done, TxPath: message.TxPath, RxPath: message.RxPath}
	s.mu.Lock()
	s.sessions[message.CallID] = sess
	if s.Media != nil && sess.UserID != "" {
		sess.Runtime, sess.Lease, sess.EmailRuntime, sess.EmailLease = s.Media.ActivateForUserSpeeds(sess.UserID)
	}
	s.mu.Unlock()
	if sess.UserID == "" {
		if s.Log != nil {
			s.Log.Warn("outgoing call without a pending request; hanging up", "call_id", message.CallID)
		}
		return bridge.Encode(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 603, Reason: "unknown outgoing call"})
	}
	if s.Log != nil {
		s.Log.Info("outgoing call session", "call_id", message.CallID, "user_id", sess.UserID, "tx_path", message.TxPath)
	}
	time.AfterFunc(callTimeout, func() {
		s.mu.Lock()
		current := s.sessions[message.CallID]
		timeout := current == sess && sess.State == "outgoing"
		s.mu.Unlock()
		if timeout {
			if s.Log != nil {
				s.Log.Warn("outgoing call timed out", "call_id", message.CallID)
			}
			s.notifyAlert(sess, false)
			_ = s.hangup(message.CallID)
		}
	})
	return nil
}

// playOutgoingAlert reads the alert text to the answered phone and then hangs
// up. It is the established-phase counterpart of greet for outbound calls.
func (s *Service) playOutgoingAlert(sess *session) {
	if sess == nil {
		return
	}
	s.mu.Lock()
	transitionLocked(sess, "announce")
	s.mu.Unlock()
	success := true
	if s.Media != nil && sess.AlertText != "" {
		success = s.prompt(sess, sess.AlertText) == nil
	}
	s.notifyAlert(sess, success)
	if err := s.hangup(sess.CallID); err != nil && s.Log != nil {
		s.Log.Warn("alert hangup failed", "call_id", sess.CallID, "error", err)
	}
}

func (s *Service) notifyAlert(sess *session, success bool) {
	if sess == nil {
		return
	}
	s.mu.Lock()
	done := sess.AlertDone
	sess.AlertDone = nil
	s.mu.Unlock()
	signalAlert(done, success)
}

func signalAlert(done chan<- bool, success bool) {
	if done == nil {
		return
	}
	select {
	case done <- success:
	default:
	}
}

func (s *Service) releaseSessionSpeech(sess *session) {
	if sess == nil {
		return
	}
	if sess.Lease != nil {
		sess.Lease.Release()
		sess.Lease = nil
	}
	if sess.EmailLease != nil {
		sess.EmailLease.Release()
		sess.EmailLease = nil
	}
}

// armInactivityTimeoutLocked resets the call-level IVR timer. It must be
// called with s.mu held; generation numbers prevent an older timer from
// hanging up a session after a newer DTMF event has reset it.
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
	ctx, cancel := context.WithCancel(context.Background())
	sess.inactivityCancel = cancel
	sess.inactivityGeneration++
	generation := sess.inactivityGeneration
	callID := sess.CallID
	go func() {
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
		go s.prompt(sess, "No input was received. Goodbye.")
		delay := s.InactivityHangupDelay
		if delay <= 0 {
			delay = defaultInactivityHangupDelay
		}
		time.AfterFunc(delay, func() { _ = s.hangup(callID) })
	}()
}

// backByContractLocked applies the formal IVR navigation contract. Recursive
// folder and editor states can layer their data-dependent behavior on top of
// this helper, but ordinary menu states do not need a second hard-coded back
// map in the call service.
func backByContractLocked(sess *session, fallback string) {
	if sess == nil {
		return
	}
	if back, ok := ivr.Back(ivr.State(sess.State)); ok {
		sess.State = string(back)
		return
	}
	sess.State = fallback
}

// ensureFlowLocked keeps sessions created by older callers and tests
// compatible while giving live calls one formal navigation history.
func ensureFlowLocked(sess *session) {
	if sess == nil {
		return
	}
	if sess.Flow == nil {
		sess.Flow = ivr.NewSession(sess.CallID)
		sess.Flow.State = ivr.State(sess.State)
		return
	}
	if sess.Flow.State != ivr.State(sess.State) {
		sess.Flow.Enter(ivr.State(sess.State))
	}
}

func backUsingFlowLocked(sess *session, fallback string) {
	if sess == nil {
		return
	}
	ensureFlowLocked(sess)
	if sess.Flow != nil && sess.Flow.State == ivr.State(sess.State) {
		if err := sess.Flow.Back(); err == nil {
			sess.State = string(sess.Flow.State)
			return
		}
	}
	backByContractLocked(sess, fallback)
}

// transitionLocked is the only state-changing primitive used by call
// handlers. It keeps the legacy string used by the existing handlers in sync
// with the formal IVR session and records the previous state for #.
func transitionLocked(sess *session, next string) {
	if sess == nil || sess.State == next {
		return
	}
	ensureFlowLocked(sess)
	if sess.Flow != nil {
		sess.Flow.Enter(ivr.State(next))
	}
	sess.State = next
}

func (s *Service) greet(sess *session) {
	if sess == nil || s.Media == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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

func (s *Service) handleDTMF(conn net.Conn, message bridge.Message) error {
	s.mu.Lock()
	sess := s.sessions[message.CallID]
	if sess == nil {
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
			return bridge.Encode(conn, bridge.Message{Type: "hangup", CallID: message.CallID, Code: 603, Reason: "PIN verification failed"})
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
		messages, listErr := s.Store.ListMailForAccount(s.baseContext(), sess.UserID, account.ID, "", true)
		if listErr != nil {
			continue
		}
		count := len(messages)
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
	ensureFlowLocked(sess)
	previousState := sess.State
	editing := sess.State == "compose" || sess.State == "recipient_input" || sess.State == "subject" || sess.State == "body" || sess.State == "contact_name"
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if sess.State != previousState {
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
		state := sess.State
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
		switch sess.State {
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
			sess.Editor = keypad.New(confirmEditorMode(sess.ConfirmState))
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
			if sess.State == "recording_subject" {
				fallback = "subject_method"
			}
			backUsingFlowLocked(sess, fallback)
		}
		s.mu.Unlock()
		s.prompt(sess, s.promptForState(sess))
		return nil
	}
	s.mu.Lock()
	state := sess.State
	s.mu.Unlock()
	if !ivr.Accepts(ivr.State(state), key) {
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
			s.prompt(sess, "VOXMail reads synchronized email over the phone. Press star to repeat, or pound to return.")
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
			s.prompt(sess, "Press pound to go back. Star repeats the current information.")
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
		go s.playAttachment(sess, int(key[0]-'1'))
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

func (s *Service) openAccounts(sess *session) {
	accounts, err := s.Store.ListAccounts(s.baseContext(), sess.UserID)
	if err != nil || len(accounts) == 0 {
		s.prompt(sess, "No mail accounts are configured.")
		return
	}
	s.mu.Lock()
	sess.Accounts = accounts
	sess.AccountPage = 0
	transitionLocked(sess, "accounts")
	sess.ActiveAccount = ""
	s.mu.Unlock()
	s.prompt(sess, "Email. Press 0 for all unread mail. "+accountPrompt(accounts, 0))
}

func (s *Service) openMessages(sess *session, unreadOnly bool, description string) {
	mails, err := s.Store.ListMail(s.baseContext(), sess.UserID, unreadOnly)
	if err != nil {
		s.prompt(sess, "Mail is temporarily unavailable.")
		return
	}
	s.mu.Lock()
	sess.Messages, sess.Cursor = mails, 0
	transitionLocked(sess, "list")
	s.mu.Unlock()
	if len(mails) == 0 {
		s.prompt(sess, "There are no "+description+".")
	} else {
		s.prompt(sess, listPrompt(mails[0], 0, len(mails)))
	}
}

func (s *Service) handleAccountMenu(sess *session, key string) {
	if key < "1" || key > "9" {
		return
	}
	s.mu.Lock()
	if key == "9" && len(sess.Accounts) > (sess.AccountPage+1)*8 {
		sess.AccountPage++
		page := sess.AccountPage
		accounts := append([]store.Account(nil), sess.Accounts...)
		s.mu.Unlock()
		s.prompt(sess, "Email. Press 0 for all unread mail. "+accountPrompt(accounts, page))
		return
	}
	page := sess.AccountPage
	index := int(key[0] - '1')
	index += page * 8
	if key == "9" || index >= len(sess.Accounts) {
		s.mu.Unlock()
		return
	}
	account := sess.Accounts[index]
	sess.ActiveAccount = account.ID
	transitionLocked(sess, "account_menu")
	s.mu.Unlock()
	s.prompt(sess, s.accountMenuPrompt(sess, account))
}

func (s *Service) handleSelectedAccountMenu(sess *session, key string) {
	s.mu.Lock()
	accountID := sess.ActiveAccount
	var account store.Account
	for _, candidate := range sess.Accounts {
		if candidate.ID == accountID {
			account = candidate
			break
		}
	}
	s.mu.Unlock()
	if account.ID == "" {
		s.prompt(sess, "That account is unavailable.")
		return
	}
	switch key {
	case "1":
		s.openAccountFolders(sess, account)
	case "2":
		s.mu.Lock()
		transitionLocked(sess, "draft_menu")
		s.mu.Unlock()
		s.prompt(sess, "Press 1 for a new email, 2 to resume a saved draft, or pound to go back.")
	case "3":
		s.refreshAccount(sess, account.ID)
	case "4":
		if !s.alertsAvailable(sess) {
			s.prompt(sess, "Alert settings are disabled by the administrator. Press pound to return.")
			return
		}
		s.mu.Lock()
		transitionLocked(sess, "account_settings")
		s.mu.Unlock()
		s.prompt(sess, menuPrompt("account_settings"))
	}
}

func (s *Service) handleDraftMenu(sess *session, key string) {
	switch key {
	case "1":
		s.startCompose(sess, "")
		s.prompt(sess, "Enter the recipient using multi tap, then press pound.")
	case "2":
		s.openDrafts(sess)
	}
}

func (s *Service) handleDraftEdit(sess *session, key string) {
	s.mu.Lock()
	switch key {
	case "1":
		transitionLocked(sess, "recipient_edit")
	case "2":
		sess.EditingDraft = true
		sess.Draft.Subject = ""
		transitionLocked(sess, "subject_method")
	case "3":
		sess.EditingDraft = true
		sess.Draft.Body = ""
		transitionLocked(sess, "body_method")
	case "4":
		transitionLocked(sess, "attachment_edit")
	default:
		s.mu.Unlock()
		return
	}
	state := sess.State
	s.mu.Unlock()
	switch state {
	case "recipient_edit":
		s.prompt(sess, menuPrompt("recipient_edit"))
	case "subject_method", "body_method", "attachment_edit":
		s.prompt(sess, menuPrompt(state))
	}
}

func (s *Service) handleRecipientEdit(sess *session, key string) {
	s.mu.Lock()
	switch key {
	case "1":
		sess.Draft.To = ""
		sess.Draft.AdditionalTo = nil
		sess.EditingDraft = true
		transitionLocked(sess, "compose")
		sess.Editor = keypad.New(keypad.ModeEmail)
	case "2":
		sess.Draft.Cc = nil
		transitionLocked(sess, "draft_edit")
	case "3":
		sess.Draft.Bcc = nil
		transitionLocked(sess, "draft_edit")
	case "4":
		sess.EditingDraft = true
		transitionLocked(sess, "recipient_menu")
	default:
		s.mu.Unlock()
		return
	}
	state := sess.State
	d := sess.Draft
	s.mu.Unlock()
	if state == "compose" {
		s.prompt(sess, "Enter the replacement primary recipient using multi tap, then press pound.")
	} else if state == "recipient_menu" {
		s.prompt(sess, recipientPrompt(d))
	} else {
		s.prompt(sess, "Recipient changes applied. "+menuPrompt("draft_edit"))
	}
}

func (s *Service) handleAttachmentEdit(sess *session, key string) {
	switch key {
	case "1":
		s.startAudioAttachment(sess)
	case "2", "3":
		s.mu.Lock()
		hasAttachment := len(sess.Draft.Attachments) > 0
		if hasAttachment {
			sess.Draft.Attachments = sess.Draft.Attachments[:len(sess.Draft.Attachments)-1]
		}
		count := len(sess.Draft.Attachments)
		s.mu.Unlock()
		if !hasAttachment {
			s.prompt(sess, "There is no attachment to change. Press 1 to add one, or pound to return.")
			return
		}
		if key == "3" {
			s.startAudioAttachment(sess)
			return
		}
		s.prompt(sess, fmt.Sprintf("The last attachment was removed. %d attachments remain. Press 1 to add one, or pound to return.", count))
	}
}

func (s *Service) openDrafts(sess *session) {
	s.mu.Lock()
	accountID := sess.ActiveAccount
	s.mu.Unlock()
	drafts, err := s.Store.ListDrafts(s.baseContext(), sess.UserID, accountID)
	if err != nil {
		s.prompt(sess, "Saved drafts are temporarily unavailable.")
		return
	}
	if len(drafts) == 0 {
		s.prompt(sess, "There are no saved drafts. Press 1 for a new email, or pound to go back.")
		return
	}
	s.mu.Lock()
	sess.Drafts = drafts
	sess.DraftPage = 0
	transitionLocked(sess, "draft_list")
	s.mu.Unlock()
	s.promptDraftList(sess)
}

func (s *Service) promptDraftList(sess *session) {
	s.mu.Lock()
	page := sess.DraftPage
	drafts := append([]store.DraftRecord(nil), sess.Drafts...)
	s.mu.Unlock()
	start := page * 8
	if start >= len(drafts) {
		s.prompt(sess, "There are no more saved drafts. Press pound to go back.")
		return
	}
	end := start + 8
	if end > len(drafts) {
		end = len(drafts)
	}
	parts := make([]string, 0, end-start)
	for i, item := range drafts[start:end] {
		description := strings.TrimSpace(item.Subject)
		if description == "" {
			description = "no subject"
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s", i+1, description))
	}
	next := ""
	if end < len(drafts) {
		next = " Press 9 for more."
	}
	s.prompt(sess, "Saved drafts. "+strings.Join(parts, ". ")+"."+next+" Press pound to go back.")
}

func (s *Service) handleDraftList(sess *session, key string) {
	if key == "9" {
		s.mu.Lock()
		if (sess.DraftPage+1)*8 < len(sess.Drafts) {
			sess.DraftPage++
		}
		s.mu.Unlock()
		s.promptDraftList(sess)
		return
	}
	if key < "1" || key > "8" {
		return
	}
	s.mu.Lock()
	index := sess.DraftPage*8 + int(key[0]-'1')
	if index < 0 || index >= len(sess.Drafts) {
		s.mu.Unlock()
		return
	}
	draftRecord := sess.Drafts[index]
	s.mu.Unlock()
	s.resumeDraft(sess, draftRecord)
}

func (s *Service) resumeDraft(sess *session, record store.DraftRecord) {
	if record.UserID != sess.UserID {
		s.prompt(sess, "That draft is unavailable.")
		return
	}
	attachments := make([]mailer.Attachment, 0, len(record.Attachments))
	for _, saved := range record.Attachments {
		path, ok := s.safeDraftAttachmentPath(sess, saved.Path)
		if !ok {
			s.prompt(sess, "That draft contains an unsafe attachment path.")
			return
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 25<<20 {
			s.prompt(sess, "A draft attachment is unavailable.")
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			s.prompt(sess, "A draft attachment could not be loaded.")
			return
		}
		attachments = append(attachments, mailer.Attachment{Filename: saved.Filename, ContentType: saved.ContentType, Data: data})
	}
	d := draft{ID: record.ID, Subject: record.Subject, Body: record.Body, Cc: append([]string(nil), record.Cc...), Bcc: append([]string(nil), record.Bcc...), Attachments: attachments, ForwardOriginal: record.ForwardMode == "forward", OriginalMessageID: record.OriginalMessageID}
	if len(record.To) > 0 {
		d.To = record.To[0]
		d.AdditionalTo = append([]string(nil), record.To[1:]...)
	}
	s.mu.Lock()
	sess.Draft = d
	sess.ActiveAccount = record.AccountID
	sess.RecipientKind = ""
	sess.Editor = nil
	switch {
	case strings.TrimSpace(d.To) == "":
		transitionLocked(sess, "compose")
		sess.Editor = keypad.New(keypad.ModeEmail)
	case strings.TrimSpace(d.Subject) == "":
		transitionLocked(sess, "subject_method")
	case strings.TrimSpace(d.Body) == "":
		transitionLocked(sess, "body_method")
	default:
		transitionLocked(sess, "review")
	}
	state := sess.State
	s.mu.Unlock()
	s.prompt(sess, "Draft resumed. "+menuPrompt(state))
}

func (s *Service) safeDraftAttachmentPath(sess *session, path string) (string, bool) {
	root := s.DataRoot
	if root == "" {
		root = filepath.Dir(sess.TxPath)
	}
	draftsRoot, err := filepath.Abs(filepath.Join(root, "drafts"))
	if err != nil {
		return "", false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(draftsRoot, pathAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return pathAbs, true
}

func (s *Service) handleAccountSettings(sess *session, key string) {
	s.mu.Lock()
	accountID := sess.ActiveAccount
	var account store.Account
	for _, candidate := range sess.Accounts {
		if candidate.ID == accountID {
			account = candidate
			break
		}
	}
	s.mu.Unlock()
	if account.ID == "" {
		s.prompt(sess, "That account is unavailable.")
		return
	}
	switch key {
	case "1":
		newValue := !account.CallAlertEnabled
		result, err := s.Store.DB.ExecContext(s.baseContext(), `UPDATE accounts SET call_alert_enabled=? WHERE id=? AND user_id=?`, newValue, account.ID, sess.UserID)
		if err != nil {
			s.prompt(sess, "The account alert setting could not be changed.")
			return
		}
		if count, _ := result.RowsAffected(); count != 1 {
			s.prompt(sess, "The account alert setting could not be changed.")
			return
		}
		s.mu.Lock()
		for i := range sess.Accounts {
			if sess.Accounts[i].ID == account.ID {
				sess.Accounts[i].CallAlertEnabled = newValue
			}
		}
		s.mu.Unlock()
		if newValue {
			s.prompt(sess, "Call alerts are enabled for this account. "+menuPrompt("account_settings"))
		} else {
			s.prompt(sess, "Call alerts are disabled for this account. "+menuPrompt("account_settings"))
		}
	case "2":
		s.openAccountAlertFolders(sess, account)
	}
}

func (s *Service) openAccountAlertFolders(sess *session, account store.Account) {
	folders, err := s.Store.ListMailFolders(s.baseContext(), sess.UserID, account.ID)
	if err != nil {
		s.prompt(sess, "Alert folders are temporarily unavailable.")
		return
	}
	seen := make(map[string]bool, len(folders))
	for _, folder := range folders {
		seen[folder] = true
	}
	var mapping map[string]string
	_ = json.Unmarshal([]byte(account.FolderMap), &mapping)
	for remote, local := range mapping {
		if local != "" && !seen[local] {
			folders = append(folders, local)
			seen[local] = true
		}
		if remote != "" && !seen[remote] {
			folders = append(folders, remote)
			seen[remote] = true
		}
	}
	sort.Strings(folders)
	selected := make(map[string]bool)
	var configured []string
	if json.Unmarshal([]byte(account.AlertFolders), &configured) == nil {
		for _, value := range configured {
			for _, folder := range folders {
				if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(folder)) {
					selected[folder] = true
				}
			}
		}
	}
	s.mu.Lock()
	sess.AlertFolders = folders
	sess.AlertFolderSelection = selected
	sess.AlertFolderPage = 0
	transitionLocked(sess, "account_alert_folders")
	s.mu.Unlock()
	s.promptAccountAlertFolders(sess)
}

func (s *Service) promptAccountAlertFolders(sess *session) {
	s.mu.Lock()
	page := sess.AlertFolderPage
	folders := append([]string(nil), sess.AlertFolders...)
	selected := make(map[string]bool, len(sess.AlertFolderSelection))
	for folder, value := range sess.AlertFolderSelection {
		selected[folder] = value
	}
	s.mu.Unlock()
	start := page * 8
	if start >= len(folders) {
		s.prompt(sess, "There are no synchronized folders. Press pound to return.")
		return
	}
	end := start + 8
	if end > len(folders) {
		end = len(folders)
	}
	parts := make([]string, 0, end-start)
	for i, folder := range folders[start:end] {
		state := "off"
		if selected[folder] {
			state = "on"
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s, currently %s", i+1, folder, state))
	}
	next := ""
	if end < len(folders) {
		next = " Press 9 for more."
	}
	s.prompt(sess, "Choose alert folders. "+strings.Join(parts, ". ")+"."+next+" Press pound when finished.")
}

func (s *Service) handleAccountAlertFolders(sess *session, key string) {
	if key == "9" {
		s.mu.Lock()
		if (sess.AlertFolderPage+1)*8 < len(sess.AlertFolders) {
			sess.AlertFolderPage++
		}
		s.mu.Unlock()
		s.promptAccountAlertFolders(sess)
		return
	}
	if key < "1" || key > "8" {
		return
	}
	s.mu.Lock()
	index := sess.AlertFolderPage*8 + int(key[0]-'1')
	if index < 0 || index >= len(sess.AlertFolders) {
		s.mu.Unlock()
		return
	}
	folder := sess.AlertFolders[index]
	sess.AlertFolderSelection[folder] = !sess.AlertFolderSelection[folder]
	var account store.Account
	for _, candidate := range sess.Accounts {
		if candidate.ID == sess.ActiveAccount {
			account = candidate
			break
		}
	}
	selected := make([]string, 0, len(sess.AlertFolderSelection))
	for candidate, enabled := range sess.AlertFolderSelection {
		if enabled {
			selected = append(selected, mappedFolder(account, candidate))
		}
	}
	selected = uniqueFolders(selected)
	accountID, userID := sess.ActiveAccount, sess.UserID
	s.mu.Unlock()
	sort.Strings(selected)
	data, err := json.Marshal(selected)
	if err == nil {
		_, err = s.Store.DB.ExecContext(s.baseContext(), `UPDATE accounts SET alert_folders=? WHERE id=? AND user_id=?`, string(data), accountID, userID)
	}
	if err != nil {
		s.prompt(sess, "The alert folders could not be changed.")
		return
	}
	s.mu.Lock()
	for i := range sess.Accounts {
		if sess.Accounts[i].ID == accountID {
			sess.Accounts[i].AlertFolders = string(data)
		}
	}
	s.mu.Unlock()
	s.promptAccountAlertFolders(sess)
}

func uniqueFolders(folders []string) []string {
	seen := make(map[string]struct{}, len(folders))
	out := make([]string, 0, len(folders))
	for _, folder := range folders {
		folder = strings.TrimSpace(folder)
		if folder == "" {
			continue
		}
		key := strings.ToLower(folder)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, folder)
	}
	return out
}

func (s *Service) refreshAccount(sess *session, accountID string) {
	s.mu.Lock()
	refresher := s.Refresher
	s.mu.Unlock()
	if refresher == nil {
		s.prompt(sess, "Manual refresh is unavailable. Press pound to return.")
		return
	}
	s.prompt(sess, "Refreshing this account now. Please wait.")
	go func() {
		ctx, cancel := context.WithTimeout(s.baseContext(), 2*time.Minute)
		defer cancel()
		err := refresher.RefreshAccount(ctx, sess.UserID, accountID)
		if err != nil {
			s.prompt(sess, "The account refresh failed. Please try again later.")
			return
		}
		s.prompt(sess, "The account refresh finished. Press 1 to listen to mail, 2 to send an email, or pound to return.")
	}()
}

func (s *Service) openAccountFolders(sess *session, account store.Account) {
	folders, err := s.Store.ListMailFolders(s.baseContext(), sess.UserID, account.ID)
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
	sess.FolderPage = 0
	sess.FolderPrefix = ""
	sess.SelectedFolder = ""
	transitionLocked(sess, "folders")
	s.mu.Unlock()
	if len(folders) == 0 {
		s.prompt(sess, "This account has no synchronized folders.")
	} else {
		s.prompt(sess, folderPromptFor(account, folders, "", 0))
	}
}

func (s *Service) handleFolderMenu(sess *session, key string) {
	s.mu.Lock()
	folders := append([]string(nil), sess.Folders...)
	prefix, page, accountID := sess.FolderPrefix, sess.FolderPage, sess.ActiveAccount
	var account store.Account
	for _, candidate := range sess.Accounts {
		if candidate.ID == accountID {
			account = candidate
			break
		}
	}
	s.mu.Unlock()
	if key == "0" && prefix == "" {
		inbox, err := s.Store.InboxFolder(s.baseContext(), account)
		if err != nil {
			s.prompt(sess, "Inbox is temporarily unavailable.")
			return
		}
		s.listenFolder(sess, accountID, inbox)
		return
	}
	children := folderChildren(folders, prefix)
	if key == "9" {
		if (page+1)*8 < len(children) {
			s.mu.Lock()
			sess.FolderPage++
			page = sess.FolderPage
			s.mu.Unlock()
			s.prompt(sess, folderPromptFor(account, folders, prefix, page))
		}
		return
	}
	if key < "1" || key > "8" {
		return
	}
	index := page*8 + int(key[0]-'1')
	if index < 0 || index >= len(children) {
		return
	}
	s.mu.Lock()
	sess.SelectedFolder = children[index]
	transitionLocked(sess, "folder_action")
	s.mu.Unlock()
	s.prompt(sess, fmt.Sprintf("Folder %s. Press 1 to listen to messages here, or 2 to open nested folders. Press pound to return.", mappedFolder(account, children[index])))
}

func (s *Service) handleFolderAction(sess *session, key string) {
	s.mu.Lock()
	folder, accountID := sess.SelectedFolder, sess.ActiveAccount
	var account store.Account
	for _, candidate := range sess.Accounts {
		if candidate.ID == accountID {
			account = candidate
			break
		}
	}
	s.mu.Unlock()
	if folder == "" {
		return
	}
	switch key {
	case "1":
		s.listenFolder(sess, accountID, folder)
	case "2":
		s.mu.Lock()
		sess.FolderPrefix = folder
		sess.FolderPage = 0
		transitionLocked(sess, "folders")
		folders := append([]string(nil), sess.Folders...)
		s.mu.Unlock()
		if len(folderChildren(folders, folder)) == 0 {
			s.prompt(sess, "There are no nested folders here. Press 1 to listen to this folder, or pound to return.")
			return
		}
		s.prompt(sess, folderPromptFor(account, folders, folder, 0))
	}
}

func (s *Service) listenFolder(sess *session, accountID, folder string) {
	mails, err := s.Store.ListMailForAccount(s.baseContext(), sess.UserID, accountID, folder, false)
	if err != nil {
		s.prompt(sess, "Mail is temporarily unavailable.")
		return
	}
	s.mu.Lock()
	sess.Messages, sess.Cursor = mails, 0
	transitionLocked(sess, "list")
	s.mu.Unlock()
	if len(mails) == 0 {
		s.prompt(sess, "There are no messages in that folder.")
	} else {
		s.prompt(sess, listPrompt(mails[0], 0, len(mails)))
	}
}

func folderChildren(folders []string, prefix string) []string {
	prefix = strings.Trim(strings.ReplaceAll(prefix, "\\", "/"), "/")
	seen := make(map[string]bool)
	var out []string
	for _, raw := range folders {
		folder := strings.Trim(strings.ReplaceAll(raw, "\\", "/"), "/")
		if folder == "" || folder == prefix || (prefix != "" && !strings.HasPrefix(folder, prefix+"/")) {
			continue
		}
		rest := strings.TrimPrefix(folder, prefix)
		rest = strings.TrimPrefix(rest, "/")
		child := rest
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			child = strings.TrimSuffix(folder[:len(folder)-len(rest)+slash], "/")
		}
		if !seen[child] {
			seen[child] = true
			out = append(out, child)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Service) openContacts(sess *session) {
	contacts, err := s.Store.ListContacts(s.baseContext(), sess.UserID)
	if err != nil || len(contacts) == 0 {
		s.prompt(sess, "You have no contacts.")
		return
	}
	s.mu.Lock()
	sess.Contacts = contacts
	sess.ContactPage = 0
	transitionLocked(sess, "contacts")
	s.mu.Unlock()
	s.prompt(sess, contactPrompt(contacts, 0))
}

func (s *Service) handleContactMenu(sess *session, key string) {
	if key < "1" || key > "9" {
		return
	}
	s.mu.Lock()
	if key == "9" && len(sess.Contacts) > (sess.ContactPage+1)*8 {
		sess.ContactPage++
		page := sess.ContactPage
		contacts := append([]store.Contact(nil), sess.Contacts...)
		s.mu.Unlock()
		s.prompt(sess, contactPrompt(contacts, page))
		return
	}
	page := sess.ContactPage
	index := int(key[0] - '1')
	index += page * 8
	if key == "9" {
		s.mu.Unlock()
		return
	}
	if index >= len(sess.Contacts) {
		s.mu.Unlock()
		return
	}
	contact := sess.Contacts[index]
	s.mu.Unlock()
	s.startComposeTo(sess, contact.Email)
	s.prompt(sess, fmt.Sprintf("Composing to %s. %s", contact.Name, menuPrompt("subject_method")))
}

func (s *Service) handleRecipientMenu(sess *session, key string) {
	switch key {
	case "1", "2", "3":
		s.mu.Lock()
		transitionLocked(sess, "recipient_input")
		sess.RecipientKind = map[string]string{"1": "to", "2": "cc", "3": "bcc"}[key]
		sess.Editor = keypad.New(keypad.ModeEmail)
		s.mu.Unlock()
		s.prompt(sess, "Enter the email address using multi tap, then press pound.")
	case "4":
		s.mu.Lock()
		transitionLocked(sess, "subject_method")
		sess.RecipientKind = ""
		sess.Editor = nil
		s.mu.Unlock()
		s.prompt(sess, menuPrompt("subject_method"))
	}
}

func (s *Service) handleFieldMethod(sess *session, key, field string) {
	if key == "1" {
		s.mu.Lock()
		transitionLocked(sess, field)
		sess.Editor = keypad.New(keypad.ModeText)
		if field == "subject" {
			sess.Editor.Text = sess.Draft.Subject
		} else {
			sess.Editor.Text = sess.Draft.Body
		}
		s.mu.Unlock()
		s.prompt(sess, "Type with multi tap, then press pound.")
		return
	}
	if key == "2" {
		if field == "body" {
			s.startVoiceRecording(sess)
		} else {
			s.startSubjectVoice(sess)
		}
	}
}

func (s *Service) handleSettingsMenu(sess *session, key string) {
	switch key {
	case "1":
		s.prompt(sess, "Voice model and speech speed are managed in the web console.")
	case "2":
		if !s.alertsAvailable(sess) {
			s.prompt(sess, "Call alerts are disabled by the administrator.")
			return
		}
		var enabled int
		if err := s.Store.DB.QueryRowContext(s.baseContext(), `SELECT alerts_enabled FROM settings WHERE user_id=?`, sess.UserID).Scan(&enabled); err != nil {
			s.prompt(sess, "Alert settings are unavailable.")
			return
		}
		enabled = 1 - enabled
		if _, err := s.Store.DB.ExecContext(s.baseContext(), `UPDATE settings SET alerts_enabled=? WHERE user_id=?`, enabled, sess.UserID); err != nil {
			s.prompt(sess, "Alert settings could not be changed.")
			return
		}
		if enabled == 1 {
			s.prompt(sess, "Call alerts are now enabled.")
		} else {
			s.prompt(sess, "Call alerts are now disabled.")
		}
	case "3":
		s.openContacts(sess)
	}
}

func accountPrompt(accounts []store.Account, page int) string {
	parts := make([]string, 0, len(accounts))
	start := page * 8
	for i := start; i < len(accounts); i++ {
		if i == start+8 {
			break
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s", i-start+1, accounts[i].CanonicalName))
	}
	next := ""
	if start+8 < len(accounts) {
		next = " Press 9 for more."
	}
	return "Accounts. " + strings.Join(parts, ". ") + "." + next + " Press pound to go back."
}

func (s *Service) accountMenuPrompt(sess *session, account store.Account) string {
	if !s.alertsAvailable(sess) {
		return fmt.Sprintf("%s. Press 1 to listen to mail, 2 to send an email, or 3 to refresh. Press pound to go back.", account.CanonicalName)
	}
	return fmt.Sprintf("%s. Press 1 to listen to mail, 2 to send an email, 3 to refresh, or 4 for account settings. Press pound to go back.", account.CanonicalName)
}

func folderPrompt(account store.Account, folders []string, page int) string {
	return folderPromptFor(account, folders, "", page)
}

func folderPromptFor(account store.Account, folders []string, prefix string, page int) string {
	children := folderChildren(folders, prefix)
	parts := make([]string, 0, len(folders))
	start := page * 8
	for i := start; i < len(children); i++ {
		if i == start+8 {
			break
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s", i-start+1, mappedFolder(account, children[i])))
	}
	next := ""
	if start+8 < len(children) {
		next = " Press 9 for more."
	}
	where := "root"
	if prefix != "" {
		where = mappedFolder(account, prefix)
	}
	return "Folders in " + where + " for " + account.CanonicalName + ". " + strings.Join(parts, ". ") + "." + next + " Press 0 for Inbox at the root, or pound to go back."
}

func contactPrompt(contacts []store.Contact, page int) string {
	parts := make([]string, 0, len(contacts))
	start := page * 8
	for i := start; i < len(contacts); i++ {
		if i == start+8 {
			break
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s", i-start+1, contacts[i].Name))
	}
	next := ""
	if start+8 < len(contacts) {
		next = " Press 9 for more."
	}
	return "Contacts. " + strings.Join(parts, ". ") + "." + next + " Press pound to go back."
}

func recipientPrompt(d draft) string {
	toCount := 0
	if strings.TrimSpace(d.To) != "" {
		toCount = 1 + len(d.AdditionalTo)
	}
	return fmt.Sprintf("You have %d To recipient(s), %d Cc, and %d Bcc. Press 1 to add To, 2 to add Cc, 3 to add Bcc, or 4 to continue.", toCount, len(d.Cc), len(d.Bcc))
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

func remoteFolder(account store.Account, local string) string {
	var mapping map[string]string
	if json.Unmarshal([]byte(account.FolderMap), &mapping) == nil {
		for remote, mapped := range mapping {
			if remote == local || mapped == local {
				return remote
			}
		}
	}
	return local
}

func (s *Service) remoteConnection(sess *session, message store.MailSummary) (*remoteimap.Client, store.Account, error) {
	if s.Store == nil || s.Secrets == nil {
		return nil, store.Account{}, fmt.Errorf("remote mail credentials are unavailable")
	}
	accounts, err := s.Store.ListAccounts(s.baseContext(), sess.UserID)
	if err != nil {
		return nil, store.Account{}, err
	}
	for _, account := range accounts {
		if account.ID != message.AccountID {
			continue
		}
		password, err := s.Secrets.Open(account.IMAPPassword)
		if err != nil {
			return nil, store.Account{}, err
		}
		port := account.IMAPPort
		if port == 0 {
			port = 993
		}
		connection, err := remoteimap.Open(s.baseContext(), remoteimap.Config{Host: account.IMAPHost, Port: port, Security: account.IMAPSecurity, Username: account.IMAPUser, Password: password})
		if err != nil {
			return nil, store.Account{}, err
		}
		return connection, account, nil
	}
	return nil, store.Account{}, fmt.Errorf("mail account was not found")
}

func (s *Service) setRemoteSeen(sess *session, message store.MailSummary, seen bool) error {
	connection, account, err := s.remoteConnection(sess, message)
	if err != nil {
		return err
	}
	defer connection.Close()
	remoteFolderName := remoteFolder(account, message.Folder)
	if message.UID != 0 {
		if err := connection.SetSeenByUID(remoteFolderName, message.UID, message.UIDValidity, seen); err == nil {
			return nil
		} else if message.MessageID == "" {
			return err
		}
	}
	return connection.SetSeen(remoteFolderName, message.MessageID, seen)
}

func (s *Service) moveRemote(sess *session, message store.MailSummary, destination string) error {
	connection, account, err := s.remoteConnection(sess, message)
	if err != nil {
		return err
	}
	defer connection.Close()
	source, target := remoteFolder(account, message.Folder), remoteFolder(account, destination)
	if message.UID != 0 {
		if err := connection.MoveByUID(source, target, message.UID, message.UIDValidity); err == nil {
			return nil
		} else if message.MessageID == "" {
			return err
		}
	}
	return connection.Move(source, target, message.MessageID)
}

func (s *Service) recordMutation(sess *session, message store.MailSummary, operation, fromFolder, toFolder, status string, mutationErr error) {
	if s.Store == nil || sess == nil || message.ID == 0 {
		return
	}
	if err := s.Store.RecordMessageMutation(s.baseContext(), message.ID, sess.UserID, operation, fromFolder, toFolder, status, mutationErr); err != nil && s.Log != nil {
		s.Log.Warn("could not record mail mutation", "message_id", message.ID, "operation", operation, "error", err)
	}
	_ = s.Store.Audit(s.baseContext(), sess.UserID, "mail_mutation", operation+":"+status)
}

// setMaildirRead updates the Maildir info suffix without touching the message
// body. Maildir flags are the local cache's durable read-state representation;
// changing SQLite alone would be overwritten by the next index pass.
func setMaildirRead(path string, read bool) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("maildir path is empty")
	}
	marker := ":2,"
	base, flags := path, ""
	if index := strings.LastIndex(path, marker); index >= 0 {
		base, flags = path[:index], path[index+len(marker):]
	}
	var kept strings.Builder
	for _, flag := range flags {
		if flag != 'S' && flag != 's' {
			kept.WriteRune(flag)
		}
	}
	flags = kept.String()
	if read {
		flags += "S"
	}
	target := base + marker + flags
	if target == path {
		return path, nil
	}
	if err := os.Rename(path, target); err != nil {
		return "", err
	}
	return target, nil
}

// rollbackRemoteMove is best effort.  IMAP MOVE can change the UID, so the
// helper intentionally falls back to Message-ID when the new UID is no longer
// valid in the destination folder.
func (s *Service) rollbackRemoteMove(sess *session, message store.MailSummary, fromFolder, toFolder string) error {
	rollback := message
	rollback.Folder = fromFolder
	return s.moveRemote(sess, rollback, toFolder)
}

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
	s.mu.Lock()
	transitionLocked(sess, "recording")
	stop := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	go func(offset int64) {
		defer cancel()
		text, err := s.Recorder.RecordAndTranscribeUntil(ctx, sess.RxPath, offset, stop)
		s.mu.Lock()
		sess.RecordStop = nil
		sess.RecordCancel = nil
		if sess.State == "recording" && !sess.Closed {
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
	}(info.Size())
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
	s.mu.Lock()
	transitionLocked(sess, "recording_subject")
	offset := info.Size()
	stop := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	sess.RecordStop = stop
	sess.RecordCancel = cancel
	s.mu.Unlock()
	s.prompt(sess, "Speak the subject after the tone. Press 0 when finished, or pound to cancel.")
	go func() {
		defer cancel()
		text, recordErr := s.Recorder.RecordAndTranscribeUntil(ctx, sess.RxPath, offset, stop)
		s.mu.Lock()
		sess.RecordStop = nil
		sess.RecordCancel = nil
		if sess.State == "recording_subject" && !sess.Closed {
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
		next := sess.State
		s.mu.Unlock()
		s.prompt(sess, menuPrompt(next))
	}()
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
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	s.mu.Lock()
	transitionLocked(sess, "audio_recording")
	sess.RecordCancel = cancel
	offset := info.Size()
	s.mu.Unlock()
	s.prompt(sess, "Speak after the tone. Press 0 when finished. The recording is limited to 30 seconds.")
	go func() {
		data, recordErr := s.Recorder.RecordAudio(ctx, sess.RxPath, offset)
		cancel()
		s.mu.Lock()
		sess.RecordCancel = nil
		if sess.State == "audio_recording" && !sess.Closed {
			if recordErr == nil {
				sess.Draft.Attachments = append(sess.Draft.Attachments, mailer.Attachment{Filename: fmt.Sprintf("voxmail-audio-%d.wav", time.Now().Unix()), ContentType: "audio/wav", Data: data})
				transitionLocked(sess, "review")
			}
		}
		stillRecording := sess.State == "audio_recording"
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
		s.prompt(sess, "Audio attachment added. Press 1 to send, 2 to save as draft, 3 to cancel, or 5 to record another attachment.")
	}()
}
func (s *Service) startComposeLocked(sess *session, mode string) {
	sess.Draft = draft{}
	sess.EditingDraft = false
	sess.RecipientKind = ""
	if mode == "" {
		transitionLocked(sess, "compose")
		sess.Editor = keypad.New(keypad.ModeEmail)
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
		sess.Editor = keypad.New(keypad.ModeEmail)
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
		sess.Editor = keypad.New(keypad.ModeText)
	}
	_, done := sess.Editor.Press(key[0])
	if !done {
		return
	}
	value := strings.TrimSpace(sess.Editor.Text)
	switch sess.State {
	case "compose":
		if _, err := mail.ParseAddress(value); err != nil {
			go s.prompt(sess, "That email address is not valid. Enter it again, then press pound.")
			return
		}
		sess.ConfirmValue = value
		sess.ConfirmState = "compose"
		sess.ConfirmRecipientKind = ""
		transitionLocked(sess, "field_confirm")
		sess.Editor = nil
		go s.prompt(sess, fieldConfirmPrompt("compose", value))
	case "recipient_input":
		if _, err := mail.ParseAddress(value); err != nil {
			go s.prompt(sess, "That email address is not valid. Enter it again, then press pound.")
			return
		}
		sess.ConfirmValue = value
		sess.ConfirmState = "recipient_input"
		sess.ConfirmRecipientKind = sess.RecipientKind
		transitionLocked(sess, "field_confirm")
		sess.Editor = nil
		go s.prompt(sess, fieldConfirmPrompt("recipient_input", value))
	case "subject":
		sess.ConfirmValue = value
		sess.ConfirmState = "subject"
		sess.ConfirmRecipientKind = ""
		transitionLocked(sess, "field_confirm")
		sess.Editor = nil
		go s.prompt(sess, fieldConfirmPrompt("subject", value))
	case "body":
		sess.ConfirmValue = value
		sess.ConfirmState = "body"
		sess.ConfirmRecipientKind = ""
		transitionLocked(sess, "field_confirm")
		sess.Editor = nil
		go s.prompt(sess, fieldConfirmPrompt("body", value))
	case "contact_name":
		if value == "" {
			go s.prompt(sess, "Enter a contact name, then press pound.")
			return
		}
		sess.ConfirmValue = value
		sess.ConfirmState = "contact_name"
		sess.ConfirmRecipientKind = ""
		transitionLocked(sess, "field_confirm")
		sess.Editor = nil
		go s.prompt(sess, fieldConfirmPrompt("contact_name", value))
	}
}

func confirmEditorMode(state string) keypad.Mode {
	if state == "compose" || state == "recipient_input" {
		return keypad.ModeEmail
	}
	return keypad.ModeText
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
		sess.Editor = keypad.New(confirmEditorMode(state))
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
	next := sess.State
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
		sess.Editor = keypad.New(keypad.ModeText)
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

func (s *Service) savePendingContact(sess *session) {
	s.mu.Lock()
	name, email := sess.PendingContactName, sess.PendingContactEmail
	s.mu.Unlock()
	if _, err := s.Store.AddContact(s.baseContext(), store.Contact{UserID: sess.UserID, Name: name, Email: email}); err != nil {
		s.prompt(sess, "That contact could not be saved. It may already exist.")
		return
	}
	s.mu.Lock()
	transitionLocked(sess, "more_options")
	sess.PendingContactName = ""
	sess.PendingContactEmail = ""
	s.mu.Unlock()
	s.prompt(sess, "Contact saved. "+menuPrompt("more_options"))
}

func (s *Service) readMessage(sess *session, m store.MailSummary) {
	file, err := os.Open(m.Path)
	if err != nil {
		s.prompt(sess, "That message is no longer available.")
		return
	}
	parsed, err := mailparse.Parse(file)
	_ = file.Close()
	if err != nil {
		s.prompt(sess, "I could not read that message.")
		return
	}
	if _, localErr := s.queueLocalReadState(sess, m, true); localErr != nil {
		s.prompt(sess, "The local mail cache could not be updated.")
		return
	}
	body := parsed.Text
	if strings.TrimSpace(body) == "" {
		body = "There is no readable email body."
	}
	s.mu.Lock()
	sess.CurrentAttachments = append([]mailparse.Attachment(nil), parsed.Attachments...)
	sess.AttachmentPage = 0
	s.mu.Unlock()
	if len(parsed.Attachments) > 0 {
		descriptions := make([]string, 0, len(parsed.Attachments))
		playable := 0
		for _, attachment := range parsed.Attachments {
			kind := attachment.ContentType
			if kind == "" {
				kind = "unknown file type"
			}
			status := "not playable"
			if attachment.Playable {
				status = "playable"
				playable++
			}
			descriptions = append(descriptions, fmt.Sprintf("%s, %s, %s", attachment.Name, kind, status))
		}
		body += fmt.Sprintf(" There are %d attachments: %s.", len(parsed.Attachments), strings.Join(descriptions, ". "))
		if playable > 0 {
			body += " Press 8 to listen to playable attachments."
		} else {
			body += " There are no playable attachments."
		}
	}
	if len([]rune(body)) > 2800 {
		body = string([]rune(body)[:2800]) + ". Message truncated."
	}
	opening := "Email from"
	if !m.Read {
		opening = "New unread email from"
	}
	s.prompt(sess, fmt.Sprintf("%s %s. Subject %s. %s %s", opening, parsed.From, parsed.Subject, body, menuPrompt("read")))
}

func (s *Service) attachmentMenu(sess *session) {
	s.mu.Lock()
	attachments := append([]mailparse.Attachment(nil), sess.CurrentAttachments...)
	page := sess.AttachmentPage
	s.mu.Unlock()
	playable := make([]mailparse.Attachment, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment.Playable && len(attachment.Data) > 0 {
			playable = append(playable, attachment)
		}
	}
	if len(playable) == 0 {
		s.mu.Lock()
		transitionLocked(sess, "read")
		s.mu.Unlock()
		s.prompt(sess, "There are no playable attachments.")
		return
	}
	start := page * 9
	if start >= len(playable) {
		page = 0
		start = 0
	}
	end := start + 9
	if end > len(playable) {
		end = len(playable)
	}
	parts := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		kind := "audio"
		if strings.HasPrefix(strings.ToLower(playable[i].ContentType), "video/") {
			kind = "video audio"
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s, %s", i-start+1, playable[i].Name, kind))
	}
	extra := ""
	if end < len(playable) {
		extra = " Press 0 for more."
	}
	s.mu.Lock()
	transitionLocked(sess, "attachment_menu")
	sess.AttachmentPage = page
	s.mu.Unlock()
	s.prompt(sess, fmt.Sprintf("There are %d playable attachments. %s.%s Press pound to return.", len(playable), strings.Join(parts, ". "), extra))
}

func (s *Service) playAttachment(sess *session, index int) {
	s.mu.Lock()
	attachments := append([]mailparse.Attachment(nil), sess.CurrentAttachments...)
	page := sess.AttachmentPage
	s.mu.Unlock()
	playable := make([]mailparse.Attachment, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment.Playable && len(attachment.Data) > 0 {
			playable = append(playable, attachment)
		}
	}
	position := page*9 + index
	if position < 0 || position >= len(playable) || len(playable[position].Data) > 50<<20 {
		s.prompt(sess, "That attachment cannot be played.")
		return
	}
	if s.Media == nil || sess.TxPath == "" {
		s.prompt(sess, "Audio playback is unavailable.")
		return
	}
	dir := s.DataRoot
	if dir == "" {
		dir = filepath.Dir(sess.TxPath)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		s.prompt(sess, "I could not prepare that attachment.")
		return
	}
	tmp, err := os.CreateTemp(dir, ".attachment-*")
	if err != nil {
		s.prompt(sess, "I could not prepare that attachment.")
		return
	}
	path := tmp.Name()
	if _, err := tmp.Write(playable[position].Data); err == nil {
		err = tmp.Close()
	} else {
		_ = tmp.Close()
	}
	defer os.Remove(path)
	if err != nil {
		s.prompt(sess, "I could not prepare that attachment.")
		return
	}
	ctx, cancel := context.WithTimeout(s.baseContext(), 10*time.Minute)
	s.mu.Lock()
	sess.AttachmentCancel = cancel
	s.mu.Unlock()
	err = s.Media.PlayMedia(ctx, sess.TxPath, path)
	cancel()
	s.mu.Lock()
	wasPlaying := sess.State == "attachment_playback"
	sess.AttachmentCancel = nil
	if wasPlaying {
		transitionLocked(sess, "read")
	}
	s.mu.Unlock()
	if wasPlaying {
		if err != nil && ctx.Err() == nil {
			s.prompt(sess, "Attachment playback failed.")
		} else {
			s.prompt(sess, "Attachment playback finished. Press 8 to listen again, or pound to return.")
		}
	}
}

func (s *Service) openMoveMenu(sess *session, message store.MailSummary) {
	accounts, err := s.Store.ListAccounts(s.baseContext(), sess.UserID)
	if err != nil {
		s.prompt(sess, "Folders are temporarily unavailable.")
		return
	}
	var account store.Account
	for _, candidate := range accounts {
		if candidate.ID == message.AccountID {
			account = candidate
			break
		}
	}
	if account.ID == "" {
		s.prompt(sess, "The message account is unavailable.")
		return
	}
	folders, err := s.Store.ListMailFolders(s.baseContext(), sess.UserID, account.ID)
	if err != nil {
		s.prompt(sess, "Folders are temporarily unavailable.")
		return
	}
	seen := make(map[string]bool, len(folders))
	for _, folder := range folders {
		seen[folder] = true
	}
	var mapping map[string]string
	_ = json.Unmarshal([]byte(account.FolderMap), &mapping)
	for _, local := range mapping {
		if local != "" && !seen[local] {
			folders = append(folders, local)
			seen[local] = true
		}
	}
	sort.Strings(folders)
	if len(folders) == 0 {
		s.prompt(sess, "There are no destination folders.")
		return
	}
	s.mu.Lock()
	sess.MoveFolders = folders
	sess.MovePage = 0
	transitionLocked(sess, "move_menu")
	s.mu.Unlock()
	s.promptMoveMenu(sess)
}

func (s *Service) promptMoveMenu(sess *session) {
	s.mu.Lock()
	page := sess.MovePage
	folders := append([]string(nil), sess.MoveFolders...)
	s.mu.Unlock()
	start := page * 9
	if start >= len(folders) {
		return
	}
	end := start + 9
	if end > len(folders) {
		end = len(folders)
	}
	parts := make([]string, 0, end-start)
	for i, folder := range folders[start:end] {
		parts = append(parts, fmt.Sprintf("Press %d for %s", i+1, folder))
	}
	next := ""
	if end < len(folders) {
		next = " Press 0 for more folders."
	}
	s.prompt(sess, "Move message. "+strings.Join(parts, ". ")+"."+next+" Press pound to return.")
}

func (s *Service) moveCurrent(sess *session, destination string) {
	s.mu.Lock()
	if sess.Cursor < 0 || sess.Cursor >= len(sess.Messages) {
		s.mu.Unlock()
		return
	}
	message := sess.Messages[sess.Cursor]
	s.mu.Unlock()
	cleanDestination := filepath.Clean(filepath.FromSlash(destination))
	if cleanDestination == "." || filepath.IsAbs(cleanDestination) || strings.HasPrefix(cleanDestination, ".."+string(filepath.Separator)) || cleanDestination == ".." {
		s.prompt(sess, "That destination is not allowed.")
		return
	}
	if err := s.moveRemote(sess, message, destination); err != nil {
		s.recordMutation(sess, message, "move", message.Folder, destination, "failed", err)
		s.prompt(sess, "I could not move that message on the mail server.")
		return
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(message.Path)))
	targetDir := filepath.Join(root, cleanDestination, "cur")
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		s.recordMutation(sess, message, "move", message.Folder, destination, "partial", err)
		_ = s.rollbackRemoteMove(sess, message, destination, message.Folder)
		s.prompt(sess, "The local mail cache could not be updated.")
		return
	}
	target := filepath.Join(targetDir, filepath.Base(message.Path))
	if err := os.Rename(message.Path, target); err != nil {
		s.recordMutation(sess, message, "move", message.Folder, destination, "partial", err)
		_ = s.rollbackRemoteMove(sess, message, destination, message.Folder)
		s.prompt(sess, "The local mail cache could not be updated.")
		return
	}
	if err := s.Store.MoveMailIndex(s.baseContext(), sess.UserID, message.ID, cleanDestination, target); err != nil {
		_ = os.Rename(target, message.Path)
		s.recordMutation(sess, message, "move", message.Folder, destination, "partial", err)
		_ = s.rollbackRemoteMove(sess, message, destination, message.Folder)
		s.prompt(sess, "The message moved, but the local index could not be updated.")
		return
	}
	s.recordMutation(sess, message, "move", message.Folder, destination, "success", nil)
	s.mu.Lock()
	sess.Messages[sess.Cursor].Folder = cleanDestination
	sess.Messages[sess.Cursor].Path = target
	transitionLocked(sess, "read")
	s.mu.Unlock()
	s.prompt(sess, "Message moved.")
}

func (s *Service) toggleRead(sess *session, message store.MailSummary) {
	read := !message.Read
	if _, err := s.queueLocalReadState(sess, message, read); err != nil {
		s.prompt(sess, "The local read state could not be updated.")
		return
	}
	if read {
		s.prompt(sess, "Message marked read locally and queued for synchronization.")
	} else {
		s.prompt(sess, "Message marked unread locally and queued for synchronization.")
	}
}

// queueLocalReadState is deliberately Maildir-first. mbsync's bidirectional
// Sync All propagates the S flag on its next run, while the renamed local file
// and SQLite path keep the cache internally consistent immediately. Direct
// IMAP is reserved for operations that Maildir cannot represent safely, such
// as a cross-folder move.
func (s *Service) queueLocalReadState(sess *session, message store.MailSummary, read bool) (string, error) {
	localPath, err := setMaildirRead(message.Path, read)
	if err != nil {
		s.recordMutation(sess, message, "mark_read", message.Folder, message.Folder, "failed", err)
		return "", err
	}
	if err := s.Store.MarkMailReadAtPath(s.baseContext(), sess.UserID, message.ID, read, localPath); err != nil {
		if localPath != message.Path {
			_ = os.Rename(localPath, message.Path)
		}
		s.recordMutation(sess, message, "mark_read", message.Folder, message.Folder, "failed", err)
		return "", err
	}
	s.recordMutation(sess, message, "mark_read", message.Folder, message.Folder, "queued", nil)
	s.mu.Lock()
	for i := range sess.Messages {
		if sess.Messages[i].ID == message.ID {
			sess.Messages[i].Read = read
			sess.Messages[i].Path = localPath
		}
	}
	s.mu.Unlock()
	return localPath, nil
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
	trashName := ""
	trashRemote := ""
	if accounts, err := s.Store.ListAccounts(s.baseContext(), sess.UserID); err == nil {
		for _, account := range accounts {
			if account.ID != m.AccountID {
				continue
			}
			if role, roleErr := s.Store.FolderRole(s.baseContext(), account, "trash"); roleErr == nil {
				trashName, trashRemote = role.LocalPath, role.RemotePath
			}
		}
	}
	if trashRemote == "" || trashName == "" {
		s.prompt(sess, "A Trash folder has not been mapped for this account.")
		return
	}
	if err := s.moveRemote(sess, m, trashRemote); err != nil {
		s.recordMutation(sess, m, "delete", m.Folder, trashName, "failed", err)
		if s.Log != nil {
			s.Log.Warn("could not move remote message to trash", "message_id", m.ID, "error", err)
		}
		s.prompt(sess, "I could not move that message to the remote Trash folder.")
		return
	}
	trash := filepath.Join(rootAbs, trashName, "cur")
	if !strings.HasPrefix(filepath.Clean(trash), rootAbs+string(filepath.Separator)) {
		s.recordMutation(sess, m, "delete", m.Folder, trashName, "partial", errors.New("trash path escaped account root"))
		_ = s.rollbackRemoteMove(sess, m, trashName, m.Folder)
		s.prompt(sess, "I could not delete that message.")
		return
	}
	if err := os.MkdirAll(trash, 0700); err != nil {
		s.recordMutation(sess, m, "delete", m.Folder, trashName, "partial", err)
		_ = s.rollbackRemoteMove(sess, m, trashName, m.Folder)
		s.prompt(sess, "I could not delete that message.")
		return
	}
	target := filepath.Join(trash, filepath.Base(pathAbs))
	if !strings.Contains(target, ":2,") {
		target += ":2,S"
	}
	err := os.Rename(pathAbs, target)
	if err == nil {
		if indexErr := s.Store.DeleteMailIndex(s.baseContext(), sess.UserID, m.ID); indexErr != nil {
			err = indexErr
		}
	}
	if err != nil {
		if renameErr := os.Rename(target, pathAbs); renameErr != nil && s.Log != nil {
			s.Log.Warn("could not roll back local trash move", "message_id", m.ID, "error", renameErr)
		}
		_ = s.rollbackRemoteMove(sess, m, trashName, m.Folder)
		s.recordMutation(sess, m, "delete", m.Folder, trashName, "partial", err)
	} else {
		s.recordMutation(sess, m, "delete", m.Folder, trashName, "success", nil)
	}
	s.mu.Lock()
	transitionLocked(sess, "list")
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
	parseRecipients := func(values []string) ([]string, error) {
		out := make([]string, 0, len(values))
		for _, value := range values {
			address, err := mail.ParseAddress(value)
			if err != nil {
				return nil, err
			}
			out = append(out, address.Address)
		}
		return out, nil
	}
	toValues := append([]string{d.To}, d.AdditionalTo...)
	to, err := parseRecipients(toValues)
	if err != nil || len(to) == 0 {
		s.prompt(sess, "That recipient is not valid.")
		return
	}
	cc, err := parseRecipients(d.Cc)
	if err != nil {
		s.prompt(sess, "A Cc recipient is not valid.")
		return
	}
	bcc, err := parseRecipients(d.Bcc)
	if err != nil {
		s.prompt(sess, "A Bcc recipient is not valid.")
		return
	}
	accounts, err := s.Store.ListAccounts(s.baseContext(), sess.UserID)
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
	raw := mailer.BuildMessageWithAttachments(a.Email, a.SenderName, to, cc, bcc, d.Subject, d.Body, d.Attachments)
	envelope := append(append(append([]string{}, to...), cc...), bcc...)
	err = mailer.Send(mailer.Config{Host: a.SMTPHost, Port: a.SMTPPort, Security: a.SMTPSecurity, Username: a.SMTPUser, Password: password, From: a.Email}, envelope, raw)
	if err != nil {
		s.prompt(sess, "Sending failed. Check the account settings.")
		return
	}
	if d.ID != "" {
		if err := s.Store.DeleteDraft(s.baseContext(), sess.UserID, d.ID); err == nil {
			s.removeDraftAttachments(sess, d)
		}
	}
	s.mu.Lock()
	transitionLocked(sess, "main")
	s.mu.Unlock()
	s.prompt(sess, "Message sent.")
}

func (s *Service) saveDraft(sess *session) {
	s.mu.Lock()
	d := sess.Draft
	activeAccount := sess.ActiveAccount
	s.mu.Unlock()
	accounts, err := s.Store.ListAccounts(s.baseContext(), sess.UserID)
	if err != nil || len(accounts) == 0 {
		s.prompt(sess, "No mail account is available for this draft.")
		return
	}
	accountID := activeAccount
	if accountID == "" {
		accountID = accounts[0].ID
	}
	draftID := d.ID
	if draftID == "" {
		draftID = fmt.Sprintf("draft-%d", time.Now().UnixNano())
	}
	dir := s.DataRoot
	if dir == "" {
		dir = filepath.Dir(sess.TxPath)
	}
	dir = filepath.Join(dir, "drafts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		s.prompt(sess, "Draft storage is unavailable.")
		return
	}
	var files []string
	attachments := make([]store.DraftAttachment, 0, len(d.Attachments))
	for i, attachment := range d.Attachments {
		if len(attachment.Data) == 0 {
			continue
		}
		path := filepath.Join(dir, fmt.Sprintf("%s-%d.bin", draftID, i))
		if err := os.WriteFile(path, attachment.Data, 0600); err != nil {
			for _, created := range files {
				_ = os.Remove(created)
			}
			s.prompt(sess, "Draft attachment storage failed.")
			return
		}
		files = append(files, path)
		attachments = append(attachments, store.DraftAttachment{Filename: attachment.Filename, ContentType: attachment.ContentType, Path: path, Size: int64(len(attachment.Data))})
	}
	recipients := append([]string{d.To}, d.AdditionalTo...)
	record := store.DraftRecord{ID: draftID, UserID: sess.UserID, AccountID: accountID, Subject: d.Subject, Body: d.Body, To: recipients, Cc: d.Cc, Bcc: d.Bcc, Attachments: attachments, OriginalMessageID: d.OriginalMessageID}
	if d.ForwardOriginal {
		record.ForwardMode = "forward"
	}
	if err := s.Store.SaveDraft(s.baseContext(), record); err != nil {
		for _, created := range files {
			_ = os.Remove(created)
		}
		s.prompt(sess, "The draft could not be saved.")
		return
	}
	remoteSaved := false
	if account, ok := findAccount(accounts, accountID); ok {
		if role, roleErr := s.Store.FolderRole(s.baseContext(), account, "drafts"); roleErr == nil && (s.Secrets != nil || s.DraftAppender != nil) {
			to, cc, bcc := draftAddresses(d)
			raw := mailer.BuildMessageWithAttachments(account.Email, account.SenderName, to, cc, bcc, d.Subject, d.Body, d.Attachments)
			if s.DraftAppender != nil {
				remoteSaved = s.DraftAppender(s.baseContext(), account, role.RemotePath, raw) == nil
			} else if password, openErr := s.Secrets.Open(account.IMAPPassword); openErr == nil {
				if connection, connErr := s.openAccountIMAP(account, password); connErr == nil {
					remoteSaved = connection.Append(role.RemotePath, raw) == nil
					_ = connection.Close()
				}
			}
		}
	}
	s.mu.Lock()
	transitionLocked(sess, "main")
	s.mu.Unlock()
	if remoteSaved {
		s.prompt(sess, "Draft saved to the remote Drafts folder.")
	} else {
		s.prompt(sess, "Draft saved locally. Map a Drafts folder and check the account connection to upload it remotely.")
	}
}

func (s *Service) removeDraftAttachments(sess *session, d draft) {
	if d.ID == "" {
		return
	}
	root := s.DataRoot
	if root == "" {
		root = filepath.Dir(sess.TxPath)
	}
	draftsRoot := filepath.Join(root, "drafts")
	entries, err := os.ReadDir(draftsRoot)
	if err != nil {
		return
	}
	prefix := d.ID + "-"
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		_ = os.Remove(filepath.Join(draftsRoot, entry.Name()))
	}
}

func findAccount(accounts []store.Account, id string) (store.Account, bool) {
	for _, account := range accounts {
		if account.ID == id {
			return account, true
		}
	}
	return store.Account{}, false
}

func draftAddresses(d draft) (to, cc, bcc []string) {
	if strings.TrimSpace(d.To) != "" {
		to = append(to, d.To)
	}
	to = append(to, d.AdditionalTo...)
	cc = append(cc, d.Cc...)
	bcc = append(bcc, d.Bcc...)
	return to, cc, bcc
}

func (s *Service) openAccountIMAP(account store.Account, password string) (*remoteimap.Client, error) {
	port := account.IMAPPort
	if port == 0 {
		port = 993
	}
	return remoteimap.Open(s.baseContext(), remoteimap.Config{Host: account.IMAPHost, Port: port, Security: account.IMAPSecurity, Username: account.IMAPUser, Password: password})
}

func (s *Service) prompt(sess *session, text string) error {
	if s.Media == nil || sess == nil || sess.TxPath == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	s.mu.Lock()
	if sess.playCancel != nil {
		sess.playCancel()
	}
	sess.playSequence++
	sequence := sess.playSequence
	sess.playCancel = cancel
	s.mu.Unlock()
	sess.promptMu.Lock()
	err := s.Media.PlayWithRuntime(ctx, s.promptRuntime(sess), sess.TxPath, text)
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

func (s *Service) promptRuntime(sess *session) *speech.Runtime {
	if sess == nil {
		return nil
	}
	s.mu.Lock()
	state := sess.State
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
	state := sess.State
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
