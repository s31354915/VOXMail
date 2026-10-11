package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"path/filepath"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/imapcheck"
	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/mailsecurity"
	"github.com/voxmail/voxmail/internal/mailsync"
	"github.com/voxmail/voxmail/internal/netguard"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

type accountRequest struct {
	ID                            string            `json:"id"`
	ConfigVersion                 *int              `json:"config_version,omitempty"`
	CanonicalName                 string            `json:"canonical_name"`
	Email                         string            `json:"email"`
	SenderName                    string            `json:"sender_name"`
	IMAPHost                      string            `json:"imap_host"`
	IMAPUser                      string            `json:"imap_user"`
	IMAPPassword                  string            `json:"imap_password"`
	IMAPSecurity                  string            `json:"imap_security"`
	SMTPHost                      string            `json:"smtp_host"`
	SMTPUser                      string            `json:"smtp_user"`
	SMTPPassword                  string            `json:"smtp_password"`
	SMTPSecurity                  string            `json:"smtp_security"`
	IMAPPort                      int               `json:"imap_port"`
	SMTPPort                      int               `json:"smtp_port"`
	SyncIntervalMinutes           int               `json:"sync_interval_minutes"`
	ReconciliationIntervalMinutes int               `json:"reconciliation_interval_minutes"`
	DisplayOrder                  int               `json:"display_order"`
	FolderMap                     map[string]string `json:"folder_map"`
	FolderRoles                   map[string]string `json:"folder_roles"`
	AlertFolders                  []string          `json:"alert_folders"`
	InitialCutoff                 *string           `json:"initial_cutoff,omitempty"`
	RetentionDays                 *int              `json:"retention_days,omitempty"`
	CallAlertEnabled              bool              `json:"call_alert_enabled"`
}

type accountView struct {
	ID                            string            `json:"id"`
	ConfigVersion                 int               `json:"config_version"`
	CanonicalName                 string            `json:"canonical_name"`
	Email                         string            `json:"email"`
	SenderName                    string            `json:"sender_name"`
	IMAPHost                      string            `json:"imap_host"`
	IMAPPort                      int               `json:"imap_port"`
	IMAPSecurity                  string            `json:"imap_security"`
	IMAPUser                      string            `json:"imap_user"`
	SMTPHost                      string            `json:"smtp_host"`
	SMTPPort                      int               `json:"smtp_port"`
	SMTPSecurity                  string            `json:"smtp_security"`
	SMTPUser                      string            `json:"smtp_user"`
	FolderMap                     map[string]string `json:"folder_map"`
	FolderRoles                   map[string]string `json:"folder_roles"`
	AlertFolders                  []string          `json:"alert_folders"`
	SyncIntervalMinutes           int               `json:"sync_interval_minutes"`
	ReconciliationIntervalMinutes int               `json:"reconciliation_interval_minutes"`
	DisplayOrder                  int               `json:"display_order"`
	InitialCutoff                 *string           `json:"initial_cutoff,omitempty"`
	RetentionDays                 *int              `json:"retention_days,omitempty"`
	CallAlertEnabled              bool              `json:"call_alert_enabled"`
	LastSync                      *syncRunView      `json:"last_sync,omitempty"`
	LastSuccessfulSync            *syncRunView      `json:"last_successful_sync,omitempty"`
}

type syncRunView struct {
	store.SyncRun
	State   string              `json:"state"`
	Failure *OperationalFailure `json:"failure,omitempty"`
}

func (s *Server) syncRunJSON(run store.SyncRun) *syncRunView {
	v := &syncRunView{SyncRun: run, State: "failed"}
	if run.ErrorCategory != "" {
		v.Failure = structuredFailure(run.ErrorCategory)
		// Preserve only a bounded numeric exit diagnostic, never provider text.
		if status := toolExitStatus.FindString(run.Error); status != "" {
			v.Failure.Diagnostic = "Child process " + status
		}
	} else {
		v.Failure = operationalFailure(run.Error) // legacy persisted runs
	}
	v.Error = ""
	if v.Failure != nil {
		v.Error = v.Failure.Reason
	}
	if run.FinishedAt == "" {
		v.State = "unfinished"
		if probe, ok := s.Sync.(interface{ SyncRunning(string) bool }); ok && probe.SyncRunning(run.AccountID) {
			v.State = "running"
		}
	} else if run.Success {
		v.State = "succeeded"
	} else if v.Failure == nil {
		v.Failure = operationalFailure("unknown failure")
		v.Error = v.Failure.Reason
	}
	return v
}

func accountJSON(a store.Account) accountView {
	v := accountView{ID: a.ID, ConfigVersion: a.ConfigVersion, CanonicalName: a.CanonicalName, Email: a.Email, SenderName: a.SenderName, IMAPHost: a.IMAPHost, IMAPPort: a.IMAPPort, IMAPSecurity: a.IMAPSecurity, IMAPUser: a.IMAPUser, SMTPHost: a.SMTPHost, SMTPPort: a.SMTPPort, SMTPSecurity: a.SMTPSecurity, SMTPUser: a.SMTPUser, SyncIntervalMinutes: a.SyncIntervalMinutes, ReconciliationIntervalMinutes: a.ReconciliationIntervalMinutes, DisplayOrder: a.DisplayOrder, InitialCutoff: a.InitialCutoff, RetentionDays: a.RetentionDays, CallAlertEnabled: a.CallAlertEnabled}
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
		view := accountJSON(account)
		view.FolderRoles = make(map[string]string)
		if roles, err := s.Store.ListFolderRoles(r.Context(), u.ID, account.ID); err == nil {
			for _, role := range roles {
				view.FolderRoles[role.Role] = role.RemotePath
			}
		}
		if run, err := s.Store.LatestSyncRun(r.Context(), account.ID, false); err == nil {
			view.LastSync = s.syncRunJSON(run)
		} else if !errors.Is(err, sql.ErrNoRows) {
			serverError(w, err)
			return
		}
		if run, err := s.Store.LatestSyncRun(r.Context(), account.ID, true); err == nil {
			view.LastSuccessfulSync = s.syncRunJSON(run)
		} else if !errors.Is(err, sql.ErrNoRows) {
			serverError(w, err)
			return
		}
		out = append(out, view)
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
	if err := validateAccountInput(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	alertsAvailable, err := s.Store.AlertsAvailable(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if !alertsAvailable && u.Role != "admin" {
		req.CallAlertEnabled = false
		req.AlertFolders = nil
	}
	if req.CanonicalName == "" || req.Email == "" || req.IMAPHost == "" || req.IMAPUser == "" || req.SMTPHost == "" || req.SMTPUser == "" {
		writeError(w, http.StatusBadRequest, "canonical name, email, IMAP, and SMTP fields are required")
		return
	}
	if _, err := mail.ParseAddress(req.Email); err != nil {
		writeError(w, http.StatusBadRequest, "email address is invalid")
		return
	}
	if req.IMAPPort < 0 || req.IMAPPort > 65535 || req.SMTPPort < 0 || req.SMTPPort > 65535 || req.SMTPPort == 25 {
		writeError(w, http.StatusBadRequest, "mail ports must be between 1 and 65535; SMTP port 25 requires an explicit plaintext policy")
		return
	}
	if req.DisplayOrder < 0 || req.SyncIntervalMinutes < 0 || req.ReconciliationIntervalMinutes < 0 || (req.RetentionDays != nil && *req.RetentionDays < 1) {
		writeError(w, http.StatusBadRequest, "order, sync interval, and retention values are invalid")
		return
	}
	if req.InitialCutoff != nil && strings.TrimSpace(*req.InitialCutoff) != "" {
		if _, err := time.Parse(time.RFC3339, strings.TrimSpace(*req.InitialCutoff)); err != nil {
			writeError(w, http.StatusBadRequest, "initial cutoff must be RFC3339")
			return
		}
	}
	if req.FolderMap == nil {
		req.FolderMap = make(map[string]string)
	}
	for role, remote := range req.FolderRoles {
		role = strings.ToLower(strings.TrimSpace(role))
		remote = strings.TrimSpace(remote)
		if role != "inbox" && role != "sent" && role != "drafts" && role != "spam" && role != "trash" && role != "archive" {
			writeError(w, http.StatusBadRequest, "invalid folder role")
			return
		}
		if remote == "" {
			continue
		}
		if req.FolderMap[remote] == "" {
			name := role
			if len(name) > 0 {
				name = strings.ToUpper(name[:1]) + name[1:]
			}
			req.FolderMap[remote] = name
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
	req.AlertFolders = normalizeAlertFolders(req.AlertFolders, req.FolderMap)
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
	var securityErr error
	req.IMAPSecurity, securityErr = mailsecurity.NormalizeIMAP(req.IMAPSecurity, req.IMAPPort)
	if securityErr != nil {
		writeError(w, http.StatusBadRequest, "invalid IMAP security mode")
		return
	}
	req.SMTPSecurity, securityErr = mailsecurity.NormalizeSMTP(req.SMTPSecurity, req.SMTPPort)
	if securityErr != nil {
		writeError(w, http.StatusBadRequest, "invalid SMTP security mode")
		return
	}
	if req.SyncIntervalMinutes < 1 {
		req.SyncIntervalMinutes = 5
	}
	if req.ReconciliationIntervalMinutes < 1 {
		req.ReconciliationIntervalMinutes = 1440
	}
	folder, _ := json.Marshal(req.FolderMap)
	alerts, _ := json.Marshal(req.AlertFolders)
	id := req.ID
	if id == "" {
		id = newID()
	}
	configVersion := 0
	if req.ConfigVersion != nil {
		configVersion = *req.ConfigVersion
	}
	a := store.Account{ID: id, UserID: u.ID, ConfigVersion: configVersion, ConfigVersionSet: req.ConfigVersion != nil, CanonicalName: req.CanonicalName, Email: req.Email, SenderName: req.SenderName, IMAPHost: req.IMAPHost, IMAPPort: req.IMAPPort, IMAPSecurity: req.IMAPSecurity, IMAPUser: req.IMAPUser, IMAPPassword: req.IMAPPassword, SMTPHost: req.SMTPHost, SMTPPort: req.SMTPPort, SMTPSecurity: req.SMTPSecurity, SMTPUser: req.SMTPUser, SMTPPassword: req.SMTPPassword, FolderMap: string(folder), AlertFolders: string(alerts), SyncIntervalMinutes: req.SyncIntervalMinutes, ReconciliationIntervalMinutes: req.ReconciliationIntervalMinutes, DisplayOrder: req.DisplayOrder, InitialCutoff: req.InitialCutoff, RetentionDays: req.RetentionDays, CallAlertEnabled: req.CallAlertEnabled}
	var saveErr error
	if req.FolderRoles != nil {
		saveErr = s.Store.SaveAccountWithRoles(r.Context(), s.Secrets, a, req.FolderRoles, req.FolderMap)
	} else {
		saveErr = s.Store.SaveAccount(r.Context(), s.Secrets, a)
	}
	if saveErr != nil {
		if errors.Is(saveErr, store.ErrAccountConfigConflict) {
			writeError(w, http.StatusConflict, "account changed; reload before saving")
			return
		}
		if errors.Is(saveErr, store.ErrInvalidAccountConfig) || errors.Is(saveErr, store.ErrInvalidFolderRole) {
			writeError(w, http.StatusBadRequest, saveErr.Error())
			return
		}
		serverError(w, saveErr)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "account_saved", id)
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *Server) reorderAccounts(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.Store.ReorderAccounts(r.Context(), u.ID, body.IDs); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "accounts_reordered", fmt.Sprintf("count=%d", len(body.IDs)))
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// validateAccount authenticates the values currently in the account form
// without persisting them. When editing an existing account, blank password
// fields reuse that account's saved credentials for this validation only.
func (s *Server) validateAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	if !s.requireOnline(w, "account validation") {
		return
	}
	var req accountRequest
	if !decode(w, r, &req) {
		return
	}
	if err := validateAccountInput(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ID != "" {
		accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		var saved *store.Account
		for i := range accounts {
			if accounts[i].ID == req.ID {
				saved = &accounts[i]
				break
			}
		}
		if saved == nil {
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
		req, err = fillAccountValidationPasswords(req, *saved, s.Secrets)
		if err != nil {
			if errors.Is(err, errAccountValidationSecretsUnavailable) {
				writeError(w, http.StatusServiceUnavailable, err.Error())
			} else if errors.Is(err, errStoredAccountCredentialsUnavailable) {
				writeOperationalError(w, http.StatusBadGateway, err)
			} else {
				writeError(w, http.StatusBadRequest, err.Error())
			}
			return
		}
	}
	if strings.TrimSpace(req.IMAPPassword) == "" || strings.TrimSpace(req.SMTPPassword) == "" {
		writeError(w, http.StatusBadRequest, "IMAP and SMTP passwords are required for validation")
		return
	}
	if req.IMAPHost == "" || req.IMAPUser == "" || req.SMTPHost == "" || req.SMTPUser == "" {
		writeError(w, http.StatusBadRequest, "IMAP and SMTP hosts and usernames are required")
		return
	}
	if req.IMAPPort == 0 {
		req.IMAPPort = 993
	}
	if req.SMTPPort == 0 {
		req.SMTPPort = 465
	}
	var err error
	if req.IMAPSecurity, err = mailsecurity.NormalizeIMAP(req.IMAPSecurity, req.IMAPPort); err != nil {
		writeError(w, http.StatusBadRequest, "invalid IMAP security mode")
		return
	}
	if req.SMTPSecurity, err = mailsecurity.NormalizeSMTP(req.SMTPSecurity, req.SMTPPort); err != nil {
		writeError(w, http.StatusBadRequest, "invalid SMTP security mode")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	folders, err := s.checkAccountCredentials(ctx, req)
	if err != nil {
		if s.log() != nil {
			s.log().Warn("new account validation failed", "error", err)
		}
		writeOperationalError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "authenticated", "folders": folders})
}

var (
	errAccountValidationSecretsUnavailable = errors.New("mail secret storage is unavailable")
	errStoredAccountCredentialsUnavailable = errors.New("stored account credentials are unavailable")
)

func fillAccountValidationPasswords(req accountRequest, saved store.Account, box *secret.Box) (accountRequest, error) {
	if req.ID == "" || req.ID != saved.ID {
		return req, store.ErrAccountNotFound
	}
	if req.IMAPPassword == "" {
		if box == nil {
			return req, errAccountValidationSecretsUnavailable
		}
		password, err := box.Open(saved.IMAPPassword)
		if err != nil {
			return req, errStoredAccountCredentialsUnavailable
		}
		req.IMAPPassword = password
	}
	if req.SMTPPassword == "" {
		if box == nil {
			return req, errAccountValidationSecretsUnavailable
		}
		password, err := box.Open(saved.SMTPPassword)
		if err != nil {
			return req, errStoredAccountCredentialsUnavailable
		}
		req.SMTPPassword = password
	}
	return req, nil
}

func (s *Server) checkAccountCredentials(ctx context.Context, req accountRequest) ([]string, error) {
	imapAddress, err := s.accountAddress(ctx, req.IMAPHost, req.IMAPPort)
	if err != nil {
		return nil, fmt.Errorf("IMAP endpoint: %w", err)
	}
	folders, err := s.checkIMAPAccount(ctx, imapcheck.Config{Host: req.IMAPHost, Address: imapAddress, Port: req.IMAPPort, Security: req.IMAPSecurity, Username: req.IMAPUser, Password: req.IMAPPassword})
	if err != nil {
		return nil, err
	}
	smtpAddress, err := s.accountAddress(ctx, req.SMTPHost, req.SMTPPort)
	if err != nil {
		return nil, fmt.Errorf("SMTP endpoint: %w", err)
	}
	if err := s.checkSMTPAccount(ctx, mailer.Config{Host: req.SMTPHost, Address: smtpAddress, Port: req.SMTPPort, Security: req.SMTPSecurity, Username: req.SMTPUser, Password: req.SMTPPassword}); err != nil {
		return nil, err
	}
	return folders, nil
}

func (s *Server) deleteAccountData(ctx context.Context, userID, accountID string, invalidate bool) (bool, error) {
	ownedPaths := make([]string, 0, 4)
	if s.DataRoot != "" {
		ownedPaths = append(ownedPaths,
			filepath.Join(s.DataRoot, "mail", accountID),
			filepath.Join(s.DataRoot, "config", "mbsync", accountID+".conf"),
			filepath.Join(s.DataRoot, "quarantine", accountID),
		)
		draftIDs, err := s.Store.ListDraftStorageIDs(ctx, userID, accountID)
		if err != nil {
			return false, err
		}
		for _, draftID := range draftIDs {
			ownedPaths = append(ownedPaths, filepath.Join(s.DataRoot, "drafts", store.DraftStorageKey(draftID)))
		}
	}
	job, beginErr := s.Store.BeginAccountDeletionWithPaths(ctx, userID, accountID, ownedPaths)
	if beginErr != nil && !errors.Is(beginErr, store.ErrAccountDeleting) {
		return false, beginErr
	}
	if job.ID == "" {
		return false, fmt.Errorf("account cleanup job is unavailable")
	}
	if coordinator, ok := s.Sync.(AccountDeletionCoordinator); s.Sync != nil && !ok {
		return false, fmt.Errorf("mail synchronization cannot be stopped safely")
	} else if coordinator != nil {
		if err := coordinator.StopAccount(ctx, accountID); err != nil {
			return false, err
		}
	}
	if invalidate {
		s.invalidateDeletedUserCalls(userID)
	}
	if err := s.Store.FinalizeAccountDeletion(ctx, userID, accountID); err != nil {
		return false, err
	}
	// The durable job becomes eligible only after coordination and metadata
	// removal. Filesystem size never extends the HTTP response lifetime.
	return true, nil
}

func (s *Server) invalidateDeletedUserCalls(userID string) {
	if s.Calls == nil {
		return
	}
	if deletion, ok := s.Calls.(DeletionCallSessionInvalidator); ok {
		deletion.InvalidateUserSessionsForDeletion(userID)
		return
	}
	s.Calls.InvalidateUserSessions(userID)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if ok {
		accountID := r.PathValue("id")
		if !validAccountPathID(accountID) {
			writeError(w, http.StatusBadRequest, "invalid account id")
			return
		}
		if s.Sync != nil {
			if _, ok := s.Sync.(AccountDeletionCoordinator); !ok {
				writeError(w, http.StatusServiceUnavailable, "mail synchronization cannot be stopped safely")
				return
			}
		}
		cleanupPending, err := s.deleteAccountData(r.Context(), u.ID, accountID, true)
		if err != nil {
			if errors.Is(err, store.ErrAccountNotFound) {
				writeError(w, http.StatusNotFound, "account not found")
				return
			}
			serverError(w, err)
			return
		}
		_ = s.Store.Audit(r.Context(), u.ID, "account_deleted", accountID)
		if cleanupPending {
			writeJSON(w, http.StatusAccepted, map[string]any{"status": "deleted", "cleanup_pending": true})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func validAccountPathID(accountID string) bool {
	return accountID != "" && filepath.Base(accountID) == accountID && !strings.ContainsAny(accountID, "/\\\x00")
}

func (s *Server) syncAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	if !s.requireOnline(w, "mail synchronization") {
		return
	}
	if s.Sync == nil {
		writeError(w, http.StatusServiceUnavailable, "mail synchronization is not running")
		return
	}
	accountID := r.PathValue("id")
	accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	found := false
	for _, account := range accounts {
		if account.ID == accountID {
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	starter, ok := s.Sync.(AccountSyncStarter)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "asynchronous mail synchronization is unavailable")
		return
	}
	runID, err := starter.StartRefreshAccount(r.Context(), s.voiceCtx, u.ID, accountID)
	if errors.Is(err, mailsync.ErrSyncBusy) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, mailsync.ErrSyncCapacity) {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		writeOperationalError(w, http.StatusBadGateway, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "account_sync_requested", accountID)
	w.Header().Set("Location", "/api/v1/accounts")
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "running", "run_id": runID, "account_id": accountID})
}

func (s *Server) cancelManualSync(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	accountID := r.PathValue("id")
	accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	found := false
	for _, account := range accounts {
		if account.ID == accountID {
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	canceler, ok := s.Sync.(ManualSyncCanceler)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "manual synchronization cancellation is unavailable")
		return
	}
	canceled, err := canceler.CancelManualSync(r.Context(), accountID)
	if err != nil {
		serverError(w, err)
		return
	}
	if !canceled {
		writeError(w, http.StatusConflict, "no manual synchronization is currently running")
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "account_sync_cancel_requested", accountID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancellation requested"})
}

func (s *Server) accountMutations(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	accountID := strings.TrimSpace(r.PathValue("id"))
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account id is required")
		return
	}
	accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	found := false
	for _, account := range accounts {
		if account.ID == accountID {
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	mutations, err := s.Store.ListMessageMutations(r.Context(), u.ID, accountID, 50)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mutations)
}

type testAccountRequest struct {
	AccountID string `json:"account_id"`
}

func (s *Server) testAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	if !s.requireOnline(w, "account testing") {
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
	if s.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "mail secret storage is unavailable")
		return
	}
	imapPassword, err := s.Secrets.Open(account.IMAPPassword)
	if err != nil {
		writeError(w, http.StatusBadGateway, "stored IMAP credentials are unavailable")
		return
	}
	smtpPassword, err := s.Secrets.Open(account.SMTPPassword)
	if err != nil {
		writeError(w, http.StatusBadGateway, "stored SMTP credentials are unavailable")
		return
	}
	imapPort := account.IMAPPort
	if imapPort == 0 {
		imapPort = 993
	}
	smtpPort := account.SMTPPort
	if smtpPort == 0 {
		smtpPort = 465
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	imapAddress, err := s.accountAddress(ctx, account.IMAPHost, imapPort)
	if err != nil {
		writeError(w, http.StatusBadGateway, "IMAP host is not reachable by policy")
		return
	}
	folders, err := s.checkIMAPAccount(ctx, imapcheck.Config{Host: account.IMAPHost, Address: imapAddress, Port: imapPort, Security: account.IMAPSecurity, Username: account.IMAPUser, Password: imapPassword})
	if err != nil {
		if s.log() != nil {
			s.log().Warn("authenticated IMAP test failed", "account_id", account.ID, "error", err)
		}
		writeOperationalError(w, http.StatusBadGateway, err)
		return
	}
	smtpAddress, err := s.accountAddress(ctx, account.SMTPHost, smtpPort)
	if err != nil {
		writeError(w, http.StatusBadGateway, "SMTP host is not reachable by policy")
		return
	}
	if err := s.checkSMTPAccount(ctx, mailer.Config{Host: account.SMTPHost, Address: smtpAddress, Port: smtpPort, Security: account.SMTPSecurity, Username: account.SMTPUser, Password: smtpPassword}); err != nil {
		if s.log() != nil {
			s.log().Warn("authenticated SMTP test failed", "account_id", account.ID, "error", err)
		}
		writeOperationalError(w, http.StatusBadGateway, err)
		return
	}
	if err := s.Store.RecordFolderDiscovery(ctx, u.ID, account.ID, account.ConfigVersion, folders); err != nil {
		switch {
		case errors.Is(err, store.ErrAccountConfigConflict):
			writeError(w, http.StatusConflict, "account settings changed during discovery; test saved settings again")
		case errors.Is(err, store.ErrAccountNotFound):
			writeError(w, http.StatusNotFound, "account not found")
		case errors.Is(err, store.ErrInvalidAccountConfig):
			writeError(w, http.StatusBadGateway, "provider folders cannot be mapped: "+err.Error())
		default:
			serverError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "authenticated", "folders": folders})
}

// ssrfSafeDial resolves host and connects to a validated, globally routable
// address so the glance test can never be redirected at loopback, private,
// link-local, metadata, or CGNAT ranges.
func ssrfSafeDial(ctx context.Context, host string, port int) error {
	_, err := ssrfSafeAddress(ctx, host, port)
	return err
}

func ssrfSafeAddress(ctx context.Context, host string, port int) (string, error) {
	return netguard.CheckAndPin(ctx, host, port, false)
}

func (s *Server) checkIMAPAccount(ctx context.Context, cfg imapcheck.Config) ([]string, error) {
	if s.imapAccountCheck != nil {
		return s.imapAccountCheck(ctx, cfg)
	}
	return imapcheck.Check(ctx, cfg)
}

func (s *Server) accountAddress(ctx context.Context, host string, port int) (string, error) {
	if s.accountEndpoint == nil {
		return ssrfSafeAddress(ctx, host, port)
	}
	address, err := s.accountEndpoint(ctx, host, port)
	if err != nil {
		return "", err
	}
	// Even an injected reachability adapter cannot return a private endpoint.
	if err := netguard.ValidateAddress(address, port, false); err != nil {
		return "", err
	}
	return address, nil
}

func (s *Server) checkSMTPAccount(ctx context.Context, cfg mailer.Config) error {
	if s.smtpAccountCheck != nil {
		return s.smtpAccountCheck(ctx, cfg)
	}
	return mailer.Check(ctx, cfg)
}
