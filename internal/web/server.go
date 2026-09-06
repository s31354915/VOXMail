package web

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/voxmail/voxmail/internal/auth"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

//go:embed static/index.html
var indexHTML []byte

type Server struct {
	Store    *store.Store
	Secrets  *secret.Box
	Log      *slog.Logger
	Sessions *SessionStore
	loginMu  sync.Mutex
	logins   map[string]loginState
}

type loginState struct {
	Failures int
	Until    time.Time
}

type SessionStore struct {
	mu      sync.Mutex
	byToken map[string]Session
}

type Session struct {
	UserID  string
	CSRF    string
	Expires time.Time
}

func NewSessionStore() *SessionStore { return &SessionStore{byToken: make(map[string]Session)} }

func (s *Server) Handler() http.Handler {
	if s.Sessions == nil {
		s.Sessions = NewSessionStore()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /api/v1", s.apiInfo)
	mux.HandleFunc("POST /api/v1/setup", s.setup)
	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.HandleFunc("POST /api/v1/logout", s.logout)
	mux.HandleFunc("GET /api/v1/me", s.me)
	mux.HandleFunc("GET /api/v1/users", s.users)
	mux.HandleFunc("POST /api/v1/users", s.createUser)
	mux.HandleFunc("DELETE /api/v1/users/{id}", s.deleteUser)
	mux.HandleFunc("GET /api/v1/accounts", s.accounts)
	mux.HandleFunc("POST /api/v1/accounts", s.saveAccount)
	mux.HandleFunc("POST /api/v1/accounts/test", s.testAccount)
	mux.HandleFunc("DELETE /api/v1/accounts/{id}", s.deleteAccount)
	mux.HandleFunc("GET /api/v1/contacts", s.contacts)
	mux.HandleFunc("POST /api/v1/contacts", s.createContact)
	mux.HandleFunc("PUT /api/v1/contacts/{id}", s.updateContact)
	mux.HandleFunc("DELETE /api/v1/contacts/{id}", s.deleteContact)
	mux.HandleFunc("GET /api/v1/whitelist", s.whitelist)
	mux.HandleFunc("POST /api/v1/whitelist", s.addWhitelist)
	mux.HandleFunc("DELETE /api/v1/whitelist/{id}", s.deleteWhitelist)
	mux.HandleFunc("GET /api/v1/settings", s.settings)
	mux.HandleFunc("PUT /api/v1/settings", s.saveSettings)
	mux.HandleFunc("GET /", s.index)
	return withSecurityHeaders(mux)
}

type setupRequest struct{ Username, Password, PIN string }

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	count, err := s.Store.UserCount(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if count != 0 {
		writeError(w, http.StatusConflict, "setup has already been completed")
		return
	}
	var req setupRequest
	if !decode(w, r, &req) {
		return
	}
	if err := validateCredentials(req.Username, req.Password, req.PIN); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	pw, err := auth.Hash(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "password cannot be hashed")
		return
	}
	pin, err := auth.Hash(req.PIN)
	if err != nil {
		writeError(w, http.StatusBadRequest, "PIN cannot be hashed")
		return
	}
	id := newID()
	u := store.User{ID: id, Username: strings.TrimSpace(req.Username), PasswordHash: pw, PINHash: pin, Role: "admin", Enabled: true}
	if err := s.Store.CreateUser(r.Context(), u); err != nil {
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), id, "setup_completed", "")
	s.issueSession(w, id)
	writeJSON(w, http.StatusCreated, publicUser(u))
}

type loginRequest struct{ Username, Password, TOTP string }

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decode(w, r, &req) {
		return
	}
	loginKey := r.RemoteAddr + "|" + strings.ToLower(strings.TrimSpace(req.Username))
	if retry := s.loginBlocked(loginKey); retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many sign-in attempts; try again later")
		return
	}
	u, err := s.Store.UserByUsername(r.Context(), strings.TrimSpace(req.Username))
	if err != nil || !u.Enabled || !auth.Check(u.PasswordHash, req.Password) {
		s.loginFailure(loginKey)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if u.TOTPSecret != "" && !auth.TOTP(u.TOTPSecret, req.TOTP, time.Now().UTC()) {
		s.loginFailure(loginKey)
		writeError(w, http.StatusUnauthorized, "authenticator code required")
		return
	}
	s.loginSuccess(loginKey)
	s.issueSession(w, u.ID)
	writeJSON(w, http.StatusOK, publicUser(u))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if token := sessionToken(r); token != "" {
		s.Sessions.Delete(token)
	}
	http.SetCookie(w, &http.Cookie{Name: "voxmail_session", MaxAge: -1, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed_out"})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if ok {
		writeJSON(w, http.StatusOK, publicUser(u))
	}
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, publicUser(u))
	}
	writeJSON(w, http.StatusOK, out)
}

type userRequest struct{ Username, Password, PIN, Role string }

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var req userRequest
	if !decode(w, r, &req) {
		return
	}
	if err := validateCredentials(req.Username, req.Password, req.PIN); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	pw, err := auth.Hash(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "password cannot be hashed")
		return
	}
	pin, err := auth.Hash(req.PIN)
	if err != nil {
		writeError(w, http.StatusBadRequest, "PIN cannot be hashed")
		return
	}
	role := "user"
	if req.Role == "admin" {
		role = "admin"
	}
	u := store.User{ID: newID(), Username: strings.TrimSpace(req.Username), PasswordHash: pw, PINHash: pin, Role: role, Enabled: true}
	if err := s.Store.CreateUser(r.Context(), u); err != nil {
		writeError(w, http.StatusConflict, "username already exists")
		return
	}
	writeJSON(w, http.StatusCreated, publicUser(u))
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.PathValue("id") == admin.ID {
		writeError(w, http.StatusBadRequest, "admin cannot delete itself")
		return
	}
	if err := s.Store.DeleteUser(r.Context(), r.PathValue("id")); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type accountRequest struct {
	ID, CanonicalName, Email, SenderName, IMAPHost, IMAPUser, IMAPPassword string
	SMTPHost, SMTPUser, SMTPPassword                                       string
	IMAPPort, SMTPPort, SyncIntervalMinutes, DisplayOrder                  int
	FolderMap                                                              map[string]string
	AlertFolders                                                           []string
	InitialCutoff                                                          *string
	RetentionDays                                                          *int
	CallAlertEnabled                                                       bool
}

type accountView struct {
	ID                  string            `json:"id"`
	CanonicalName       string            `json:"canonical_name"`
	Email               string            `json:"email"`
	SenderName          string            `json:"sender_name"`
	IMAPHost            string            `json:"imap_host"`
	IMAPPort            int               `json:"imap_port"`
	IMAPUser            string            `json:"imap_user"`
	SMTPHost            string            `json:"smtp_host"`
	SMTPPort            int               `json:"smtp_port"`
	SMTPUser            string            `json:"smtp_user"`
	FolderMap           map[string]string `json:"folder_map"`
	AlertFolders        []string          `json:"alert_folders"`
	SyncIntervalMinutes int               `json:"sync_interval_minutes"`
	DisplayOrder        int               `json:"display_order"`
	InitialCutoff       *string           `json:"initial_cutoff,omitempty"`
	RetentionDays       *int              `json:"retention_days,omitempty"`
	CallAlertEnabled    bool              `json:"call_alert_enabled"`
}

func accountJSON(a store.Account) accountView {
	v := accountView{ID: a.ID, CanonicalName: a.CanonicalName, Email: a.Email, SenderName: a.SenderName, IMAPHost: a.IMAPHost, IMAPPort: a.IMAPPort, IMAPUser: a.IMAPUser, SMTPHost: a.SMTPHost, SMTPPort: a.SMTPPort, SMTPUser: a.SMTPUser, SyncIntervalMinutes: a.SyncIntervalMinutes, DisplayOrder: a.DisplayOrder, InitialCutoff: a.InitialCutoff, RetentionDays: a.RetentionDays, CallAlertEnabled: a.CallAlertEnabled}
	if err := json.Unmarshal([]byte(a.FolderMap), &v.FolderMap); err != nil || v.FolderMap == nil {
		v.FolderMap = map[string]string{}
	}
	if err := json.Unmarshal([]byte(a.AlertFolders), &v.AlertFolders); err != nil || v.AlertFolders == nil {
		v.AlertFolders = []string{}
	}
	return v
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]accountView, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, accountJSON(account))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) saveAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var req accountRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CanonicalName == "" || req.Email == "" || req.IMAPHost == "" || req.IMAPUser == "" || req.SMTPHost == "" || req.SMTPUser == "" {
		writeError(w, http.StatusBadRequest, "canonical name, email, IMAP, and SMTP fields are required")
		return
	}
	if _, err := mail.ParseAddress(req.Email); err != nil {
		writeError(w, http.StatusBadRequest, "email address is invalid")
		return
	}
	if req.IMAPPort < 0 || req.IMAPPort > 65535 || req.SMTPPort < 0 || req.SMTPPort > 65535 {
		writeError(w, http.StatusBadRequest, "mail ports must be between 1 and 65535")
		return
	}
	if req.DisplayOrder < 0 || req.SyncIntervalMinutes < 0 || (req.RetentionDays != nil && *req.RetentionDays < 1) {
		writeError(w, http.StatusBadRequest, "order, sync interval, and retention values are invalid")
		return
	}
	if req.InitialCutoff != nil && strings.TrimSpace(*req.InitialCutoff) != "" {
		if _, err := time.Parse(time.RFC3339, strings.TrimSpace(*req.InitialCutoff)); err != nil {
			writeError(w, http.StatusBadRequest, "initial cutoff must be RFC3339")
			return
		}
	}
	if err := validateFolderMap(req.FolderMap); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, folder := range req.AlertFolders {
		if strings.TrimSpace(folder) == "" || strings.ContainsAny(folder, "\x00\r\n") {
			writeError(w, http.StatusBadRequest, "alert folders must be non-empty names")
			return
		}
	}
	if req.ID != "" {
		accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		found := false
		for _, account := range accounts {
			if account.ID == req.ID {
				found = true
				break
			}
		}
		if !found {
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
	} else if req.IMAPPassword == "" || req.SMTPPassword == "" {
		writeError(w, http.StatusBadRequest, "IMAP and SMTP passwords are required for a new account")
		return
	}
	if req.IMAPPort == 0 {
		req.IMAPPort = 993
	}
	if req.SMTPPort == 0 {
		req.SMTPPort = 465
	}
	if req.SyncIntervalMinutes < 1 {
		req.SyncIntervalMinutes = 5
	}
	folder, _ := json.Marshal(req.FolderMap)
	alerts, _ := json.Marshal(req.AlertFolders)
	id := req.ID
	if id == "" {
		id = newID()
	}
	a := store.Account{ID: id, UserID: u.ID, CanonicalName: req.CanonicalName, Email: req.Email, SenderName: req.SenderName, IMAPHost: req.IMAPHost, IMAPPort: req.IMAPPort, IMAPUser: req.IMAPUser, IMAPPassword: req.IMAPPassword, SMTPHost: req.SMTPHost, SMTPPort: req.SMTPPort, SMTPUser: req.SMTPUser, SMTPPassword: req.SMTPPassword, FolderMap: string(folder), AlertFolders: string(alerts), SyncIntervalMinutes: req.SyncIntervalMinutes, DisplayOrder: req.DisplayOrder, InitialCutoff: req.InitialCutoff, RetentionDays: req.RetentionDays, CallAlertEnabled: req.CallAlertEnabled}
	if err := s.Store.SaveAccount(r.Context(), s.Secrets, a); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if ok {
		if err := s.Store.DeleteAccount(r.Context(), u.ID, r.PathValue("id")); err != nil {
			serverError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type testAccountRequest struct {
	AccountID string `json:"account_id"`
}

func (s *Server) testAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var req testAccountRequest
	if !decode(w, r, &req) {
		return
	}
	if req.AccountID == "" {
		writeError(w, http.StatusBadRequest, "account id is required")
		return
	}
	accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	var account store.Account
	for _, candidate := range accounts {
		if candidate.ID == req.AccountID {
			account = candidate
			break
		}
	}
	if account.ID == "" {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	port := account.IMAPPort
	if port == 0 {
		port = 993
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(account.IMAPHost, strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, "connection failed")
		return
	}
	_ = conn.Close()
	writeJSON(w, http.StatusOK, map[string]string{"status": "reachable"})
}

func (s *Server) contacts(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if ok {
		out, err := s.Store.ListContacts(r.Context(), u.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}
func (s *Server) createContact(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var c store.Contact
	if !decode(w, r, &c) {
		return
	}
	c.UserID = u.ID
	if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Email) == "" {
		writeError(w, http.StatusBadRequest, "name and email are required")
		return
	}
	if err := s.Store.AddContact(r.Context(), c); err != nil {
		writeError(w, http.StatusConflict, "contact already exists")
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) updateContact(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	c := store.Contact{ID: parseID(r.PathValue("id"))}
	if c.ID < 1 || !decode(w, r, &c) {
		if c.ID < 1 {
			writeError(w, http.StatusBadRequest, "invalid contact id")
		}
		return
	}
	c.UserID = u.ID
	if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Email) == "" {
		writeError(w, http.StatusBadRequest, "name and email are required")
		return
	}
	if err := s.Store.UpdateContact(r.Context(), c); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}
func (s *Server) deleteContact(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	id := parseID(r.PathValue("id"))
	if id < 1 {
		writeError(w, http.StatusBadRequest, "invalid contact id")
		return
	}
	if err := s.Store.DeleteContact(r.Context(), u.ID, id); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) whitelist(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	out, err := s.Store.ListWhitelist(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) addWhitelist(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var e store.WhitelistEntry
	if !decode(w, r, &e) {
		return
	}
	e.UserID = u.ID
	e.Phone = normalizePhone(e.Phone)
	if e.Phone == "" {
		writeError(w, http.StatusBadRequest, "phone is required")
		return
	}
	if err := s.Store.AddWhitelist(r.Context(), e); err != nil {
		writeError(w, http.StatusConflict, "phone already belongs to a user")
		return
	}
	writeJSON(w, http.StatusCreated, e)
}
func (s *Server) deleteWhitelist(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	id := parseID(r.PathValue("id"))
	if id < 1 {
		writeError(w, http.StatusBadRequest, "invalid whitelist id")
		return
	}
	if err := s.Store.DeleteWhitelist(r.Context(), u.ID, id); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	var voice string
	var menu, email int
	var enabled int
	var phone *string
	err := s.Store.DB.QueryRowContext(r.Context(), `SELECT tts_voice,menu_speed,email_speed,alerts_enabled,alert_phone FROM settings WHERE user_id=?`, u.ID).Scan(&voice, &menu, &email, &enabled, &phone)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tts_voice": voice, "menu_speed": menu, "email_speed": email, "alerts_enabled": enabled != 0, "alert_phone": phone})
}
func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var body struct {
		TTSVoice      string  `json:"tts_voice"`
		MenuSpeed     int     `json:"menu_speed"`
		EmailSpeed    int     `json:"email_speed"`
		AlertsEnabled bool    `json:"alerts_enabled"`
		AlertPhone    *string `json:"alert_phone"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.TTSVoice == "" {
		body.TTSVoice = "en_US-hfc_male-medium"
	}
	if body.MenuSpeed < 1 || body.MenuSpeed > 5 {
		body.MenuSpeed = 3
	}
	if body.EmailSpeed < 1 || body.EmailSpeed > 5 {
		body.EmailSpeed = 2
	}
	body.TTSVoice = strings.TrimSpace(body.TTSVoice)
	if body.TTSVoice == "" || strings.ContainsAny(body.TTSVoice, `/\\\x00\r\n`) || strings.Contains(body.TTSVoice, "..") {
		writeError(w, http.StatusBadRequest, "invalid voice model")
		return
	}
	if body.AlertPhone != nil {
		phone := normalizePhone(*body.AlertPhone)
		if phone == "" {
			body.AlertPhone = nil
		} else {
			body.AlertPhone = &phone
		}
	}
	_, err := s.Store.DB.ExecContext(r.Context(), `INSERT INTO settings(user_id,tts_voice,menu_speed,email_speed,alerts_enabled,alert_phone) VALUES(?,?,?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET tts_voice=excluded.tts_voice,menu_speed=excluded.menu_speed,email_speed=excluded.email_speed,alerts_enabled=excluded.alerts_enabled,alert_phone=excluded.alert_phone`, u.ID, body.TTSVoice, body.MenuSpeed, body.EmailSpeed, body.AlertsEnabled, body.AlertPhone)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Healthy(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
func (s *Server) apiInfo(w http.ResponseWriter, r *http.Request) {
	setupAvailable := false
	if s.Store != nil {
		count, err := s.Store.UserCount(r.Context())
		if err != nil {
			serverError(w, err)
			return
		}
		setupAvailable = count == 0
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": "VOXMail", "api_version": "v1", "setup_available": setupAvailable})
}
func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) require(w http.ResponseWriter, r *http.Request, write bool) (store.User, bool) {
	token := sessionToken(r)
	session, ok := s.Sessions.Get(token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, false
	}
	if write && r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-CSRF-Token") != session.CSRF {
		writeError(w, http.StatusForbidden, "csrf token required")
		return store.User{}, false
	}
	user, err := s.Store.UserByID(r.Context(), session.UserID)
	if err != nil || !user.Enabled {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, false
	}
	return user, true
}
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	u, ok := s.require(w, r, r.Method != "GET")
	if !ok {
		return u, false
	}
	if u.Role != "admin" {
		writeError(w, http.StatusForbidden, "administrator access required")
		return store.User{}, false
	}
	return u, true
}
func (s *Server) issueSession(w http.ResponseWriter, userID string) {
	token := newID()
	csrf := newID()
	s.Sessions.Put(token, Session{UserID: userID, CSRF: csrf, Expires: time.Now().Add(12 * time.Hour)})
	http.SetCookie(w, &http.Cookie{Name: "voxmail_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	w.Header().Set("X-CSRF-Token", csrf)
}
func sessionToken(r *http.Request) string {
	c, err := r.Cookie("voxmail_session")
	if err != nil {
		return ""
	}
	return c.Value
}
func (s *SessionStore) Put(token string, session Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byToken[token] = session
}
func (s *SessionStore) Get(token string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.byToken[token]
	if !ok || time.Now().After(session.Expires) {
		delete(s.byToken, token)
		return Session{}, false
	}
	return session, true
}
func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byToken, token)
}

func (s *Server) loginBlocked(key string) time.Duration {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.logins == nil {
		s.logins = make(map[string]loginState)
	}
	state := s.logins[key]
	if time.Now().Before(state.Until) {
		return time.Until(state.Until)
	}
	return 0
}

func (s *Server) loginFailure(key string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.logins == nil {
		s.logins = make(map[string]loginState)
	}
	state := s.logins[key]
	state.Failures++
	if state.Failures >= 5 {
		state.Until = time.Now().Add(5 * time.Minute)
		state.Failures = 0
	}
	s.logins[key] = state
}

func (s *Server) loginSuccess(key string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	delete(s.logins, key)
}
func publicUser(u store.User) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Username, "role": u.Role, "enabled": u.Enabled, "two_factor_enabled": u.TOTPSecret != ""}
}
func validateCredentials(username, password, pin string) error {
	if len(strings.TrimSpace(username)) < 3 || len(strings.TrimSpace(username)) > 64 {
		return errors.New("username must be 3-64 characters")
	}
	if len(password) < 12 {
		return errors.New("password must be at least 12 characters")
	}
	if len(pin) < 4 || len(pin) > 12 {
		return errors.New("PIN must be 4-12 digits")
	}
	for _, r := range pin {
		if r < '0' || r > '9' {
			return errors.New("PIN must contain digits only")
		}
	}
	return nil
}

func validateFolderMap(mapping map[string]string) error {
	seen := make(map[string]struct{}, len(mapping))
	for remote, local := range mapping {
		remote, local = strings.TrimSpace(remote), strings.TrimSpace(local)
		if remote == "" || local == "" || strings.ContainsAny(remote+local, "\x00\r\n") || strings.Contains(remote, "..") || strings.Contains(local, "..") || strings.HasPrefix(remote, "/") || strings.HasPrefix(local, "/") {
			return errors.New("folder mappings must be relative, non-empty names")
		}
		if _, ok := seen[local]; ok {
			return errors.New("folder mappings must have unique local names")
		}
		seen[local] = struct{}{}
	}
	return nil
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func serverError(w http.ResponseWriter, _ error) {
	writeError(w, http.StatusInternalServerError, "internal server error")
}
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func parseID(value string) int64 {
	var id int64
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0
		}
		id = id*10 + int64(r-'0')
	}
	return id
}

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
