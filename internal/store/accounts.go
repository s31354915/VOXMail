package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/mailsecurity"
	"github.com/voxmail/voxmail/internal/secret"
)

func (s *Store) SaveAccount(ctx context.Context, box *secret.Box, account Account) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.saveAccountTx(ctx, tx, box, account); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveAccountWithRoles commits account fields and folder roles as one logical
// configuration update. External credential sealing happens before the SQL
// transaction is committed, but no partial database state is exposed.
func (s *Store) SaveAccountWithRoles(ctx context.Context, box *secret.Box, account Account, roles, mapping map[string]string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.saveAccountTx(ctx, tx, box, account); err != nil {
		return err
	}
	if err := s.saveFolderRolesTx(ctx, tx, account.UserID, account.ID, roles, mapping); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) saveAccountTx(ctx context.Context, tx *sql.Tx, box *secret.Box, account Account) error {
	if box == nil || account.ID == "" || account.UserID == "" {
		return fmt.Errorf("secret box and account ownership are required")
	}
	imapSecurity, err := mailsecurity.NormalizeIMAP(account.IMAPSecurity, account.IMAPPort)
	if err != nil {
		return fmt.Errorf("IMAP security: %w", err)
	}
	smtpSecurity, err := mailsecurity.NormalizeSMTP(account.SMTPSecurity, account.SMTPPort)
	if err != nil {
		return fmt.Errorf("SMTP security: %w", err)
	}
	var owner string
	var deleting, currentVersion int
	lookupErr := tx.QueryRowContext(ctx, `SELECT user_id,COALESCE(deleting,0),COALESCE(config_version,1) FROM accounts WHERE id = ?`, account.ID).Scan(&owner, &deleting, &currentVersion)
	if lookupErr == nil {
		if owner != account.UserID {
			return fmt.Errorf("account belongs to another user")
		}
		if deleting != 0 {
			return ErrAccountDeleting
		}
		if account.ConfigVersionSet {
			if account.ConfigVersion < 1 {
				return fmt.Errorf("%w: config version must be positive", ErrInvalidAccountConfig)
			}
			if account.ConfigVersion != currentVersion {
				return ErrAccountConfigConflict
			}
		}
	} else if lookupErr != sql.ErrNoRows {
		return lookupErr
	}
	var existingIMAP, existingSMTP string
	if account.IMAPPassword == "" || account.SMTPPassword == "" {
		_ = tx.QueryRowContext(ctx, `SELECT imap_password, smtp_password FROM accounts WHERE id = ? AND user_id = ?`, account.ID, account.UserID).Scan(&existingIMAP, &existingSMTP)
	}
	var imapPass, smtpPass string
	if account.IMAPPassword == "" && existingIMAP != "" {
		imapPass = existingIMAP
	} else {
		imapPass, err = box.Seal(account.IMAPPassword)
	}
	if err != nil {
		return err
	}
	if account.SMTPPassword == "" && existingSMTP != "" {
		smtpPass = existingSMTP
	} else {
		smtpPass, err = box.Seal(account.SMTPPassword)
	}
	if err != nil {
		return err
	}
	alertFolders := strings.TrimSpace(account.AlertFolders)
	folderMap, err := validateAccountFolderMap(account.FolderMap)
	if err != nil {
		return err
	}
	var selectedFolders []string
	if json.Unmarshal([]byte(alertFolders), &selectedFolders) != nil {
		selectedFolders = nil
	}
	if len(selectedFolders) == 0 && account.CallAlertEnabled {
		selectedFolders = []string{mappedInboxFolder(folderMap)}
	} else {
		for i, folder := range selectedFolders {
			for remote, local := range folderMap {
				if strings.EqualFold(strings.TrimSpace(folder), strings.TrimSpace(remote)) && local != "" {
					selectedFolders[i] = local
					break
				}
			}
		}
	}
	if encoded, marshalErr := json.Marshal(selectedFolders); marshalErr == nil {
		alertFolders = string(encoded)
	}
	configVersion := 1
	if lookupErr == nil && currentVersion > 0 {
		configVersion = currentVersion
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_port,imap_user,imap_password,imap_security,smtp_host,smtp_port,smtp_security,smtp_user,smtp_password,folder_map,sync_interval_minutes,reconciliation_interval_minutes,initial_cutoff,retention_days,call_alert_enabled,alert_folders,display_order,config_version,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET canonical_name=excluded.canonical_name,email=excluded.email,sender_name=excluded.sender_name,imap_host=excluded.imap_host,imap_port=excluded.imap_port,imap_user=excluded.imap_user,imap_password=excluded.imap_password,imap_security=excluded.imap_security,smtp_host=excluded.smtp_host,smtp_port=excluded.smtp_port,smtp_security=excluded.smtp_security,smtp_user=excluded.smtp_user,smtp_password=excluded.smtp_password,folder_map=excluded.folder_map,sync_interval_minutes=excluded.sync_interval_minutes,reconciliation_interval_minutes=excluded.reconciliation_interval_minutes,initial_cutoff=excluded.initial_cutoff,retention_days=excluded.retention_days,call_alert_enabled=excluded.call_alert_enabled,alert_folders=excluded.alert_folders,display_order=excluded.display_order,config_version=config_version+1`, account.ID, account.UserID, account.CanonicalName, account.Email, account.SenderName, account.IMAPHost, account.IMAPPort, account.IMAPUser, imapPass, imapSecurity, account.SMTPHost, account.SMTPPort, smtpSecurity, account.SMTPUser, smtpPass, account.FolderMap, account.SyncIntervalMinutes, account.ReconciliationIntervalMinutes, account.InitialCutoff, account.RetentionDays, account.CallAlertEnabled, alertFolders, account.DisplayOrder, configVersion, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func validateAccountFolderMap(raw string) (map[string]string, error) {
	mapping := make(map[string]string)
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &mapping); err != nil || mapping == nil {
			return nil, fmt.Errorf("%w: folder mappings must be valid JSON", ErrInvalidAccountConfig)
		}
	}
	seenRemote := make(map[string]struct{}, len(mapping))
	seenLocal := make(map[string]struct{}, len(mapping))
	for rawRemote, rawLocal := range mapping {
		remote := strings.TrimSpace(rawRemote)
		local := strings.TrimSpace(rawLocal)
		normalizedLocal := strings.ReplaceAll(local, "\\", "/")
		if remote == "" || local == "" || strings.ContainsAny(remote+local, "\x00\r\n") || strings.Contains(remote, "..") || strings.Contains(local, "..") || strings.HasPrefix(remote, "/") || strings.HasPrefix(normalizedLocal, "/") {
			return nil, fmt.Errorf("%w: folder mappings must be relative, non-empty names", ErrInvalidAccountConfig)
		}
		remoteKey := strings.ToLower(remote)
		if _, exists := seenRemote[remoteKey]; exists {
			return nil, fmt.Errorf("%w: remote folder %q is duplicated", ErrInvalidAccountConfig, remote)
		}
		localKey := strings.ToLower(local)
		if _, exists := seenLocal[localKey]; exists {
			return nil, fmt.Errorf("%w: local alias %q is duplicated", ErrInvalidAccountConfig, local)
		}
		seenRemote[remoteKey] = struct{}{}
		seenLocal[localKey] = struct{}{}
	}
	return mapping, nil
}

func mappedInboxFolder(mapping map[string]string) string {
	for remote, local := range mapping {
		if strings.EqualFold(strings.TrimSpace(remote), "INBOX") && strings.TrimSpace(local) != "" {
			return strings.TrimSpace(local)
		}
	}
	return "Inbox"
}

func (s *Store) ListAccounts(ctx context.Context, userID string) ([]Account, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user_id,canonical_name,email,sender_name,imap_host,imap_port,imap_user,imap_password,imap_security,smtp_host,smtp_port,smtp_security,smtp_user,smtp_password,folder_map,sync_interval_minutes,reconciliation_interval_minutes,initial_cutoff,retention_days,call_alert_enabled,alert_folders,display_order,config_version FROM accounts WHERE user_id = ? AND COALESCE(deleting,0)=0 ORDER BY display_order, canonical_name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Account
	for rows.Next() {
		var a Account
		var alerts int
		if err := rows.Scan(&a.ID, &a.UserID, &a.CanonicalName, &a.Email, &a.SenderName, &a.IMAPHost, &a.IMAPPort, &a.IMAPUser, &a.IMAPPassword, &a.IMAPSecurity, &a.SMTPHost, &a.SMTPPort, &a.SMTPSecurity, &a.SMTPUser, &a.SMTPPassword, &a.FolderMap, &a.SyncIntervalMinutes, &a.ReconciliationIntervalMinutes, &a.InitialCutoff, &a.RetentionDays, &alerts, &a.AlertFolders, &a.DisplayOrder, &a.ConfigVersion); err != nil {
			return nil, err
		}
		a.CallAlertEnabled = alerts != 0
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) ListAllAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user_id,canonical_name,email,sender_name,imap_host,imap_port,imap_user,imap_password,imap_security,smtp_host,smtp_port,smtp_security,smtp_user,smtp_password,folder_map,sync_interval_minutes,reconciliation_interval_minutes,initial_cutoff,retention_days,call_alert_enabled,alert_folders,display_order,config_version FROM accounts WHERE COALESCE(deleting,0)=0 ORDER BY user_id, display_order, canonical_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Account
	for rows.Next() {
		var a Account
		var alerts int
		if err := rows.Scan(&a.ID, &a.UserID, &a.CanonicalName, &a.Email, &a.SenderName, &a.IMAPHost, &a.IMAPPort, &a.IMAPUser, &a.IMAPPassword, &a.IMAPSecurity, &a.SMTPHost, &a.SMTPPort, &a.SMTPSecurity, &a.SMTPUser, &a.SMTPPassword, &a.FolderMap, &a.SyncIntervalMinutes, &a.ReconciliationIntervalMinutes, &a.InitialCutoff, &a.RetentionDays, &alerts, &a.AlertFolders, &a.DisplayOrder, &a.ConfigVersion); err != nil {
			return nil, err
		}
		a.CallAlertEnabled = alerts != 0
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) DeleteAccount(ctx context.Context, userID, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM accounts WHERE user_id = ? AND id = ?`, userID, id)
	return err
}

// BeginAccountDeletion makes an account invisible to new work and persists
// the exact owned paths that must be cleaned. A repeated request returns the
// existing job without creating a second cleanup record.
func (s *Store) BeginAccountDeletion(ctx context.Context, userID, accountID, maildir string) (CleanupJob, error) {
	paths := []string{}
	if strings.TrimSpace(maildir) != "" {
		paths = append(paths, maildir)
	}
	return s.BeginAccountDeletionWithPaths(ctx, userID, accountID, paths)
}

// BeginAccountDeletionWithPaths is the owner-scoped deletion boundary for
// account artifacts that are not represented by a database foreign key, such
// as generated configuration, private draft generations, and quarantine.
func (s *Store) BeginAccountDeletionWithPaths(ctx context.Context, userID, accountID string, ownedPaths []string) (CleanupJob, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(accountID) == "" {
		return CleanupJob{}, fmt.Errorf("account ownership is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return CleanupJob{}, err
	}
	defer tx.Rollback()
	var owner string
	var deleting int
	if err := tx.QueryRowContext(ctx, `SELECT user_id,COALESCE(deleting,0) FROM accounts WHERE id=?`, accountID).Scan(&owner, &deleting); err != nil {
		if err == sql.ErrNoRows {
			return CleanupJob{}, ErrAccountNotFound
		}
		return CleanupJob{}, err
	}
	if owner != userID {
		// Do not reveal whether an ID belongs to another user. The HTTP layer
		// presents both cases as the same not-found result.
		return CleanupJob{}, ErrAccountNotFound
	}
	if deleting != 0 {
		var job CleanupJob
		var encoded string
		if err := tx.QueryRowContext(ctx, `SELECT id,user_id,account_id,paths FROM cleanup_jobs WHERE user_id=? AND account_id=? AND status<>? ORDER BY created_at DESC LIMIT 1`, userID, accountID, "complete").Scan(&job.ID, &job.UserID, &job.AccountID, &encoded); err != nil {
			return CleanupJob{}, fmt.Errorf("%w: cleanup job unavailable: %v", ErrAccountDeleting, err)
		}
		if err := json.Unmarshal([]byte(encoded), &job.Paths); err != nil {
			return CleanupJob{}, fmt.Errorf("decode cleanup job: %w", err)
		}
		return job, fmt.Errorf("%w: %s", ErrAccountDeleting, job.ID)
	}
	paths := make([]string, 0, len(ownedPaths)+1)
	seenPaths := make(map[string]struct{}, len(ownedPaths)+1)
	for _, path := range ownedPaths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, exists := seenPaths[path]; exists {
			continue
		}
		seenPaths[path] = struct{}{}
		paths = append(paths, path)
	}
	rows, err := tx.QueryContext(ctx, `SELECT da.path FROM draft_attachments da JOIN drafts d ON d.id=da.draft_id WHERE d.user_id=? AND d.account_id=? AND TRIM(da.path)<>''`, userID, accountID)
	if err != nil {
		return CleanupJob{}, err
	}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			_ = rows.Close()
			return CleanupJob{}, err
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return CleanupJob{}, err
	}
	if err := rows.Close(); err != nil {
		return CleanupJob{}, err
	}
	pathsJSON, err := json.Marshal(paths)
	if err != nil {
		return CleanupJob{}, err
	}
	job := CleanupJob{ID: fmt.Sprintf("cleanup-%d-%s", time.Now().UnixNano(), strings.ReplaceAll(accountID, "-", "")), UserID: userID, AccountID: accountID, Paths: paths}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO cleanup_jobs(id,user_id,account_id,paths,status,attempts,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?, ?,?)`, job.ID, userID, accountID, string(pathsJSON), "pending", 0, "", now, now); err != nil {
		return CleanupJob{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET deleting=1 WHERE id=? AND user_id=? AND COALESCE(deleting,0)=0`, accountID, userID); err != nil {
		return CleanupJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return CleanupJob{}, err
	}
	return job, nil
}

// FinalizeAccountDeletion removes metadata only after active work has been
// stopped. The cleanup job remains so filesystem failures are recoverable.
func (s *Store) FinalizeAccountDeletion(ctx context.Context, userID, accountID string) error {
	result, err := s.DB.ExecContext(ctx, `DELETE FROM accounts WHERE user_id=? AND id=? AND COALESCE(deleting,0)=1`, userID, accountID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		var exists int
		if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM cleanup_jobs WHERE user_id=? AND account_id=?`, userID, accountID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return sql.ErrNoRows
		}
	}
	return nil
}

// FinishCleanupJob records whether every owned path was removed. Failed jobs
// stay durable and are eligible for an operator/retry worker.
func (s *Store) FinishCleanupJob(ctx context.Context, jobID string, cleanupErr error) error {
	if strings.TrimSpace(jobID) == "" {
		return fmt.Errorf("cleanup job is required")
	}
	status, detail := "complete", ""
	if cleanupErr != nil {
		status, detail = "failed", cleanupErr.Error()
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE cleanup_jobs SET status=?,attempts=attempts+1,last_error=?,updated_at=? WHERE id=?`, status, detail, time.Now().UTC().Format(time.RFC3339Nano), jobID)
	return err
}

// ReorderAccounts applies a complete, owner-scoped order in one transaction.
func (s *Store) ReorderAccounts(ctx context.Context, userID string, ids []string) error {
	if strings.TrimSpace(userID) == "" || len(ids) == 0 {
		return fmt.Errorf("user and account order are required")
	}
	seen := make(map[string]struct{}, len(ids))
	for i, id := range ids {
		ids[i] = strings.TrimSpace(id)
		if ids[i] == "" {
			return fmt.Errorf("account order contains an empty ID")
		}
		if _, ok := seen[ids[i]]; ok {
			return fmt.Errorf("account order contains a duplicate ID")
		}
		seen[ids[i]] = struct{}{}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	owned := make(map[string]struct{}, len(ids))
	rows, err := tx.QueryContext(ctx, `SELECT id FROM accounts WHERE user_id=?`, userID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		owned[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(owned) != len(ids) {
		return fmt.Errorf("account order must include every owned account")
	}
	for _, id := range ids {
		if _, ok := owned[id]; !ok {
			return fmt.Errorf("account %q is not owned by user", id)
		}
	}
	for order, id := range ids {
		result, err := tx.ExecContext(ctx, `UPDATE accounts SET display_order=? WHERE id=? AND user_id=?`, order, id, userID)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return fmt.Errorf("account %q is not owned by user", id)
		}
	}
	return tx.Commit()
}
