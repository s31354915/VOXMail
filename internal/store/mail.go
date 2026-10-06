package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (s *Store) ListMail(ctx context.Context, userID string, unreadOnly bool) ([]MailSummary, error) {
	query := `SELECT m.id,m.account_id,m.folder,m.path,COALESCE(m.message_id,''),COALESCE(m.sender,''),COALESCE(m.recipients,''),COALESCE(m.cc,''),COALESCE(m.subject,''),COALESCE(m.message_date,''),m.message_date_utc,m.is_read,m.attachment_count,COALESCE(m.imap_uid,0),COALESCE(m.uid_validity,0),COALESCE(m.message_flags,'') FROM mail_messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=? AND COALESCE(a.deleting,0)=0`
	args := []any{userID}
	if unreadOnly {
		accounts, err := s.ListAccounts(ctx, userID)
		if err != nil {
			return nil, err
		}
		if len(accounts) == 0 {
			return []MailSummary{}, nil
		}
		parts := make([]string, 0, len(accounts))
		for _, account := range accounts {
			parts = append(parts, `(m.account_id=? AND m.folder=? AND m.is_read=0)`)
			inbox, err := s.InboxFolder(ctx, account)
			if err != nil {
				return nil, err
			}
			args = append(args, account.ID, inbox)
		}
		query += ` AND (` + strings.Join(parts, ` OR `) + `)`
	}
	query += ` ORDER BY CASE WHEN m.message_date_utc IS NULL THEN 1 ELSE 0 END,m.message_date_utc,m.id DESC`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailSummary
	for rows.Next() {
		var m MailSummary
		if err := scanMailSummary(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ListMailForAccount(ctx context.Context, userID, accountID, folder string, unreadOnly bool) ([]MailSummary, error) {
	query := `SELECT m.id,m.account_id,m.folder,m.path,COALESCE(m.message_id,''),COALESCE(m.sender,''),COALESCE(m.recipients,''),COALESCE(m.cc,''),COALESCE(m.subject,''),COALESCE(m.message_date,''),m.message_date_utc,m.is_read,m.attachment_count,COALESCE(m.imap_uid,0),COALESCE(m.uid_validity,0),COALESCE(m.message_flags,'') FROM mail_messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=? AND COALESCE(a.deleting,0)=0 AND m.account_id=?`
	args := []any{userID, accountID}
	if folder != "" {
		query += ` AND m.folder=?`
		args = append(args, folder)
	}
	if unreadOnly {
		if folder == "" {
			accounts, err := s.ListAccounts(ctx, userID)
			if err != nil {
				return nil, err
			}
			for _, account := range accounts {
				if account.ID == accountID {
					var err error
					folder, err = s.InboxFolder(ctx, account)
					if err != nil {
						return nil, err
					}
					break
				}
			}
			query += ` AND m.folder=?`
			args = append(args, folder)
		}
		query += ` AND m.is_read=0`
	}
	query += ` ORDER BY CASE WHEN m.message_date_utc IS NULL THEN 1 ELSE 0 END,m.message_date_utc,m.id DESC`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailSummary
	for rows.Next() {
		var m MailSummary
		if err := scanMailSummary(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanMailSummary(rows *sql.Rows, m *MailSummary) error {
	var read int
	var uid, uidValidity int64
	var dateUTC sql.NullInt64
	if err := rows.Scan(&m.ID, &m.AccountID, &m.Folder, &m.Path, &m.MessageID, &m.Sender, &m.Recipients, &m.Cc, &m.Subject, &m.Date, &dateUTC, &read, &m.Attachments, &uid, &uidValidity, &m.Flags); err != nil {
		return err
	}
	m.Read = read != 0
	m.UID, m.UIDValidity = uint32(uid), uint32(uidValidity)
	if dateUTC.Valid {
		value := dateUTC.Int64
		m.DateUTC = &value
	}
	return nil
}

// ListMailPage returns one bounded keyset page. Valid normalized dates sort
// first in chronological order; rows with invalid or missing dates are kept
// in deterministic ID order after them. A nil Next cursor means completion.
func (s *Store) ListMailPage(ctx context.Context, userID, accountID, folder string, unreadOnly bool, after *MailCursor, limit int) (MailPage, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	query := `SELECT m.id,m.account_id,m.folder,m.path,COALESCE(m.message_id,''),COALESCE(m.sender,''),COALESCE(m.recipients,''),COALESCE(m.cc,''),COALESCE(m.subject,''),COALESCE(m.message_date,''),m.message_date_utc,m.is_read,m.attachment_count,COALESCE(m.imap_uid,0),COALESCE(m.uid_validity,0),COALESCE(m.message_flags,'') FROM mail_messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=? AND COALESCE(a.deleting,0)=0`
	args := []any{userID}
	if accountID != "" {
		query += ` AND m.account_id=?`
		args = append(args, accountID)
	}
	if folder != "" {
		query += ` AND m.folder=?`
		args = append(args, folder)
	}
	if unreadOnly {
		if folder == "" {
			if accountID != "" {
				accounts, err := s.ListAccounts(ctx, userID)
				if err != nil {
					return MailPage{}, err
				}
				var account *Account
				for index := range accounts {
					if accounts[index].ID == accountID {
						account = &accounts[index]
						break
					}
				}
				if account == nil {
					return MailPage{}, nil
				}
				inbox, err := s.InboxFolder(ctx, *account)
				if err != nil {
					return MailPage{}, err
				}
				query += ` AND m.folder=?`
				args = append(args, inbox)
			} else {
				accounts, err := s.ListAccounts(ctx, userID)
				if err != nil {
					return MailPage{}, err
				}
				if len(accounts) == 0 {
					return MailPage{}, nil
				}
				parts := make([]string, 0, len(accounts))
				for _, account := range accounts {
					inbox, err := s.InboxFolder(ctx, account)
					if err != nil {
						return MailPage{}, err
					}
					parts = append(parts, `(m.account_id=? AND m.folder=?)`)
					args = append(args, account.ID, inbox)
				}
				query += ` AND (` + strings.Join(parts, ` OR `) + `)`
			}
		}
		query += ` AND m.is_read=0`
	}
	if after != nil {
		if after.DateUTC == nil {
			query += ` AND m.message_date_utc IS NULL AND m.id < ?`
			args = append(args, after.ID)
		} else {
			query += ` AND ((m.message_date_utc IS NOT NULL AND (m.message_date_utc > ? OR (m.message_date_utc=? AND m.id < ?))) OR m.message_date_utc IS NULL)`
			args = append(args, *after.DateUTC, *after.DateUTC, after.ID)
		}
	}
	query += ` ORDER BY CASE WHEN m.message_date_utc IS NULL THEN 1 ELSE 0 END,m.message_date_utc,m.id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return MailPage{}, err
	}
	defer rows.Close()
	page := MailPage{Messages: make([]MailSummary, 0, limit)}
	for rows.Next() {
		var message MailSummary
		if err := scanMailSummary(rows, &message); err != nil {
			return MailPage{}, err
		}
		page.Messages = append(page.Messages, message)
	}
	if err := rows.Err(); err != nil {
		return MailPage{}, err
	}
	if len(page.Messages) > limit {
		page.Messages = page.Messages[:limit]
		last := page.Messages[len(page.Messages)-1]
		page.Next = &MailCursor{DateUTC: last.DateUTC, ID: last.ID}
	}
	return page, nil
}

// CountUnread returns an aggregate count without loading message rows.
func (s *Store) CountUnread(ctx context.Context, userID string) (int, error) {
	accounts, err := s.ListAccounts(ctx, userID)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, account := range accounts {
		count, err := s.CountUnreadForAccount(ctx, userID, account.ID)
		if err != nil {
			return 0, err
		}
		total += count
	}
	return total, nil
}

func (s *Store) CountUnreadForAccount(ctx context.Context, userID, accountID string) (int, error) {
	accounts, err := s.ListAccounts(ctx, userID)
	if err != nil {
		return 0, err
	}
	var account *Account
	for index := range accounts {
		if accounts[index].ID == accountID {
			account = &accounts[index]
			break
		}
	}
	if account == nil {
		return 0, nil
	}
	inbox, err := s.InboxFolder(ctx, *account)
	if err != nil {
		return 0, err
	}
	var count int
	err = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_messages WHERE account_id=? AND folder=? AND is_read=0`, accountID, inbox).Scan(&count)
	return count, err
}

// inboxFolder keeps the compatibility folder_map format useful while the
// normalized folder_roles table is introduced. New records should map the
// remote INBOX explicitly; old records continue to use the stable Inbox
// default. This function is intentionally the only fallback used by unread
// queries, so Spam and Trash are never counted by name guessing.
func (s *Store) InboxFolder(ctx context.Context, account Account) (string, error) {
	var local string
	err := s.DB.QueryRowContext(ctx, `SELECT local_path FROM folder_roles WHERE account_id=? AND role='inbox'`, account.ID).Scan(&local)
	if err == nil && strings.TrimSpace(local) != "" {
		return filepathCleanFolder(local), nil
	}
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	return inboxFolderLegacy(account), nil
}

// FolderRole returns the explicitly configured remote/local folder pair. It
// intentionally has no name-based guessing: callers must not accidentally
// treat a folder named "Trash" or "Junk" as a role the user did not map.
func (s *Store) FolderRole(ctx context.Context, account Account, role string) (FolderRole, error) {
	var result FolderRole
	err := s.DB.QueryRowContext(ctx, `SELECT account_id,role,remote_path,local_path FROM folder_roles WHERE account_id=? AND role=?`, account.ID, strings.ToLower(strings.TrimSpace(role))).Scan(&result.AccountID, &result.Role, &result.RemotePath, &result.LocalPath)
	if err == nil {
		result.LocalPath = filepathCleanFolder(result.LocalPath)
		return result, nil
	}
	if err != sql.ErrNoRows {
		return FolderRole{}, err
	}
	// Compatibility for pre-role accounts: only an exact conventional remote
	// mapping is accepted, never substring matching such as "Deleted Items".
	var mapping map[string]string
	if json.Unmarshal([]byte(account.FolderMap), &mapping) == nil {
		for remote, local := range mapping {
			if strings.EqualFold(strings.TrimSpace(remote), role) {
				return FolderRole{AccountID: account.ID, Role: role, RemotePath: remote, LocalPath: filepathCleanFolder(local)}, nil
			}
		}
	}
	return FolderRole{}, sql.ErrNoRows
}

func inboxFolderLegacy(account Account) string {
	var mapping map[string]string
	if json.Unmarshal([]byte(account.FolderMap), &mapping) == nil {
		for remote, local := range mapping {
			if strings.EqualFold(strings.TrimSpace(remote), "INBOX") && strings.TrimSpace(local) != "" {
				return filepathCleanFolder(local)
			}
		}
	}
	return "Inbox"
}

func (s *Store) ListFolderRoles(ctx context.Context, userID, accountID string) ([]FolderRole, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT r.account_id,r.role,r.remote_path,r.local_path FROM folder_roles r JOIN accounts a ON a.id=r.account_id WHERE a.user_id=? AND r.account_id=? ORDER BY r.role`, userID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roles []FolderRole
	for rows.Next() {
		var role FolderRole
		if err := rows.Scan(&role.AccountID, &role.Role, &role.RemotePath, &role.LocalPath); err != nil {
			return nil, err
		}
		roles = append(roles, role)
	}
	return roles, rows.Err()
}

func (s *Store) SaveFolderRoles(ctx context.Context, userID, accountID string, roles map[string]string, mapping map[string]string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.saveFolderRolesTx(ctx, tx, userID, accountID, roles, mapping); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) saveFolderRolesTx(ctx context.Context, tx *sql.Tx, userID, accountID string, roles map[string]string, mapping map[string]string) error {
	if accountID == "" {
		return fmt.Errorf("account ID is required")
	}
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT user_id FROM accounts WHERE id=?`, accountID).Scan(&owner); err != nil {
		return err
	} else if owner != userID {
		return fmt.Errorf("account does not belong to user")
	}
	normalized, err := validateFolderRoleMappings(roles, mapping)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM folder_roles WHERE account_id=?`, accountID); err != nil {
		return err
	}
	for _, item := range normalized {
		if s.folderRoleInsertHook != nil {
			if err := s.folderRoleInsertHook(); err != nil {
				return fmt.Errorf("insert folder role: %w", err)
			}
		}
		role, remote, local := item.role, item.remote, item.local
		if _, err := tx.ExecContext(ctx, `INSERT INTO folder_roles(account_id,role,remote_path,local_path) VALUES(?,?,?,?)`, accountID, role, remote, local); err != nil {
			return err
		}
	}
	return nil
}

type normalizedFolderRole struct {
	role, remote, local string
}

func validateFolderRoleMappings(roles, mapping map[string]string) ([]normalizedFolderRole, error) {
	allowed := map[string]bool{"inbox": true, "sent": true, "drafts": true, "spam": true, "trash": true, "archive": true}
	aliases := make(map[string]string, len(mapping))
	for rawRemote, rawLocal := range mapping {
		remote := strings.TrimSpace(rawRemote)
		local := filepathCleanFolder(rawLocal)
		key := strings.ToLower(remote)
		if remote == "" || strings.ContainsAny(remote, "\x00\r\n") || local == "Inbox" && strings.TrimSpace(rawLocal) == "" {
			return nil, fmt.Errorf("%w: folder mappings must have non-empty names", ErrInvalidFolderRole)
		}
		if _, exists := aliases[key]; exists {
			return nil, fmt.Errorf("%w: remote folder %q is duplicated", ErrInvalidFolderRole, remote)
		}
		aliases[key] = strings.TrimSpace(rawLocal)
	}
	seenRoles := make(map[string]struct{}, len(roles))
	seenRemotes := make(map[string]struct{}, len(roles))
	seenLocals := make(map[string]struct{}, len(roles))
	result := make([]normalizedFolderRole, 0, len(roles))
	for rawRole, rawRemote := range roles {
		role := strings.ToLower(strings.TrimSpace(rawRole))
		remote := strings.TrimSpace(rawRemote)
		if !allowed[role] || remote == "" || strings.ContainsAny(remote, "\x00\r\n") {
			return nil, fmt.Errorf("%w: role %q is invalid", ErrInvalidFolderRole, rawRole)
		}
		if _, exists := seenRoles[role]; exists {
			return nil, fmt.Errorf("%w: role %q is duplicated", ErrInvalidFolderRole, role)
		}
		remoteKey := strings.ToLower(remote)
		if _, exists := seenRemotes[remoteKey]; exists {
			return nil, fmt.Errorf("%w: remote folder %q is assigned to multiple roles", ErrInvalidFolderRole, remote)
		}
		local, ok := aliases[remoteKey]
		if !ok || strings.TrimSpace(local) == "" {
			return nil, fmt.Errorf("%w: role %q is missing from folder mappings", ErrInvalidFolderRole, role)
		}
		local = filepathCleanFolder(local)
		if local == "" {
			return nil, fmt.Errorf("%w: role %q has an empty local alias", ErrInvalidFolderRole, role)
		}
		localKey := strings.ToLower(local)
		if _, exists := seenLocals[localKey]; exists {
			return nil, fmt.Errorf("%w: local alias %q is assigned to multiple roles", ErrInvalidFolderRole, local)
		}
		seenRoles[role] = struct{}{}
		seenRemotes[remoteKey] = struct{}{}
		seenLocals[localKey] = struct{}{}
		result = append(result, normalizedFolderRole{role: role, remote: remote, local: local})
	}
	return result, nil
}

func (s *Store) SaveDraft(ctx context.Context, draft DraftRecord) error {
	if draft.UserID == "" || draft.AccountID == "" {
		return fmt.Errorf("draft ownership and account are required")
	}
	if draft.ID == "" {
		draft.ID = fmt.Sprintf("draft-%d", time.Now().UnixNano())
	}
	var owner string
	if err := s.DB.QueryRowContext(ctx, `SELECT user_id FROM accounts WHERE id=?`, draft.AccountID).Scan(&owner); err != nil {
		return err
	} else if owner != draft.UserID {
		return fmt.Errorf("draft account does not belong to user")
	}
	if draft.ID != "" {
		var existingUser string
		err := s.DB.QueryRowContext(ctx, `SELECT user_id FROM drafts WHERE id=?`, draft.ID).Scan(&existingUser)
		if err == nil && existingUser != draft.UserID {
			return fmt.Errorf("draft belongs to another user")
		}
		if err != nil && err != sql.ErrNoRows {
			return err
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO drafts(id,user_id,account_id,subject,body,forward_mode,original_message_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET subject=excluded.subject,body=excluded.body,forward_mode=excluded.forward_mode,original_message_id=excluded.original_message_id,updated_at=excluded.updated_at`, draft.ID, draft.UserID, draft.AccountID, draft.Subject, draft.Body, draft.ForwardMode, draft.OriginalMessageID, now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM draft_recipients WHERE draft_id=?`, draft.ID); err != nil {
		return err
	}
	for _, recipient := range draft.To {
		if _, err := tx.ExecContext(ctx, `INSERT INTO draft_recipients(draft_id,kind,address) VALUES(?,?,?)`, draft.ID, "to", recipient); err != nil {
			return err
		}
	}
	for _, recipient := range draft.Cc {
		if _, err := tx.ExecContext(ctx, `INSERT INTO draft_recipients(draft_id,kind,address) VALUES(?,?,?)`, draft.ID, "cc", recipient); err != nil {
			return err
		}
	}
	for _, recipient := range draft.Bcc {
		if _, err := tx.ExecContext(ctx, `INSERT INTO draft_recipients(draft_id,kind,address) VALUES(?,?,?)`, draft.ID, "bcc", recipient); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM draft_attachments WHERE draft_id=?`, draft.ID); err != nil {
		return err
	}
	for _, attachment := range draft.Attachments {
		if _, err := tx.ExecContext(ctx, `INSERT INTO draft_attachments(draft_id,filename,content_type,path,size) VALUES(?,?,?,?,?)`, draft.ID, attachment.Filename, attachment.ContentType, attachment.Path, attachment.Size); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListDrafts returns durable drafts owned by the user. Account ownership is
// checked in the query as well as by SaveDraft so a deleted or reassigned
// account can never make another user's draft visible.
func (s *Store) ListDrafts(ctx context.Context, userID, accountID string) ([]DraftRecord, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("draft user is required")
	}
	query := `SELECT d.id,d.user_id,d.account_id,d.subject,d.body,d.forward_mode,d.original_message_id,d.created_at,d.updated_at
		FROM drafts d JOIN accounts a ON a.id=d.account_id WHERE d.user_id=? AND a.user_id=?`
	args := []any{userID, userID}
	if strings.TrimSpace(accountID) != "" {
		query += ` AND d.account_id=?`
		args = append(args, accountID)
	}
	query += ` ORDER BY d.updated_at DESC,d.id DESC`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DraftRecord
	for rows.Next() {
		var draft DraftRecord
		if err := rows.Scan(&draft.ID, &draft.UserID, &draft.AccountID, &draft.Subject, &draft.Body, &draft.ForwardMode, &draft.OriginalMessageID, &draft.CreatedAt, &draft.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, draft)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Load child rows only after the parent result set is closed. SQLite is
	// configured with a single connection, so querying children while rows is
	// still open would wait forever for the same connection.
	rows.Close()
	for i := range out {
		if err := s.loadDraftChildren(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListDraftStorageIDs returns the durable draft identities whose private
// attachment generations belong to an account. Callers use DraftStorageKey
// to collect the exact owner-scoped directories before account metadata is
// removed.
func (s *Store) ListDraftStorageIDs(ctx context.Context, userID, accountID string) ([]string, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(accountID) == "" {
		return nil, fmt.Errorf("draft ownership is required")
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM drafts WHERE user_id=? AND account_id=?`, userID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DraftStorageKey is the stable private-directory component used for draft
// attachment generations. It is deliberately non-reversible and contains no
// user-controlled path separators.
func DraftStorageKey(id string) string {
	hash := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%x", hash[:12])
}

// LoadDraft loads one draft only when it belongs to userID.
func (s *Store) LoadDraft(ctx context.Context, userID, draftID string) (DraftRecord, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(draftID) == "" {
		return DraftRecord{}, fmt.Errorf("draft identity is required")
	}
	var draft DraftRecord
	err := s.DB.QueryRowContext(ctx, `SELECT d.id,d.user_id,d.account_id,d.subject,d.body,d.forward_mode,d.original_message_id,d.created_at,d.updated_at
		FROM drafts d JOIN accounts a ON a.id=d.account_id WHERE d.id=? AND d.user_id=? AND a.user_id=?`, draftID, userID, userID).
		Scan(&draft.ID, &draft.UserID, &draft.AccountID, &draft.Subject, &draft.Body, &draft.ForwardMode, &draft.OriginalMessageID, &draft.CreatedAt, &draft.UpdatedAt)
	if err != nil {
		return DraftRecord{}, err
	}
	if err := s.loadDraftChildren(ctx, &draft); err != nil {
		return DraftRecord{}, err
	}
	return draft, nil
}

func (s *Store) loadDraftChildren(ctx context.Context, draft *DraftRecord) error {
	recipientRows, err := s.DB.QueryContext(ctx, `SELECT kind,address FROM draft_recipients WHERE draft_id=? ORDER BY id`, draft.ID)
	if err != nil {
		return err
	}
	for recipientRows.Next() {
		var kind, address string
		if err := recipientRows.Scan(&kind, &address); err != nil {
			recipientRows.Close()
			return err
		}
		switch kind {
		case "to":
			draft.To = append(draft.To, address)
		case "cc":
			draft.Cc = append(draft.Cc, address)
		case "bcc":
			draft.Bcc = append(draft.Bcc, address)
		}
	}
	if err := recipientRows.Err(); err != nil {
		recipientRows.Close()
		return err
	}
	recipientRows.Close()
	attachmentRows, err := s.DB.QueryContext(ctx, `SELECT filename,content_type,path,size FROM draft_attachments WHERE draft_id=? ORDER BY id`, draft.ID)
	if err != nil {
		return err
	}
	defer attachmentRows.Close()
	for attachmentRows.Next() {
		var attachment DraftAttachment
		if err := attachmentRows.Scan(&attachment.Filename, &attachment.ContentType, &attachment.Path, &attachment.Size); err != nil {
			return err
		}
		draft.Attachments = append(draft.Attachments, attachment)
	}
	return attachmentRows.Err()
}

func (s *Store) DeleteDraft(ctx context.Context, userID, draftID string) error {
	result, err := s.DB.ExecContext(ctx, `DELETE FROM drafts WHERE id=? AND user_id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, draftID, userID, userID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("draft was not found")
	}
	return nil
}

func filepathCleanFolder(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || value == "." || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\x00\r\n") {
		return "Inbox"
	}
	value = strings.Trim(value, "/")
	parts := strings.Split(value, "/")
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			return "Inbox"
		default:
			cleaned = append(cleaned, part)
		}
	}
	if len(cleaned) == 0 {
		return "Inbox"
	}
	return strings.Join(cleaned, "/")
}

func (s *Store) ListMailFolders(ctx context.Context, userID, accountID string) ([]string, error) {
	// Include discovered folders even when they currently contain no indexed
	// messages.  Otherwise an empty but valid destination (or a nested folder
	// whose messages have not arrived yet) disappears from the phone menu.
	rows, err := s.DB.QueryContext(ctx, `SELECT folder FROM (
		SELECT DISTINCT m.folder AS folder
		FROM mail_messages m JOIN accounts a ON a.id=m.account_id
		WHERE a.user_id=? AND m.account_id=?
		UNION
		SELECT DISTINCT f.local_path AS folder
		FROM remote_folders f JOIN accounts a ON a.id=f.account_id
		WHERE a.user_id=? AND f.account_id=? AND f.local_path<>''
	) ORDER BY folder`, userID, accountID, userID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var folders []string
	for rows.Next() {
		var folder string
		if err := rows.Scan(&folder); err != nil {
			return nil, err
		}
		folders = append(folders, folder)
	}
	return folders, rows.Err()
}

func (s *Store) MarkMailRead(ctx context.Context, userID string, id int64, read bool) error {
	result, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET is_read=?,updated_at=? WHERE id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, read, time.Now().UTC().Format(time.RFC3339Nano), id, userID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("message was not found")
	}
	return nil
}

// MarkMailReadAtPath updates the local Maildir path and read state together.
// The path changes when the Maildir filename gains or loses the S flag; both
// values must be durable before mbsync gets a chance to propagate the local
// mutation.
func (s *Store) MarkMailReadAtPath(ctx context.Context, userID string, id int64, read bool, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("maildir path is required")
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET is_read=?,path=?,updated_at=? WHERE id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, read, path, time.Now().UTC().Format(time.RFC3339Nano), id, userID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("message was not found")
	}
	return nil
}

func (s *Store) MoveMailIndex(ctx context.Context, userID string, id int64, folder, path string) error {
	result, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET folder=?,path=?,updated_at=? WHERE id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, folder, path, time.Now().UTC().Format(time.RFC3339Nano), id, userID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("message was not found")
	}
	return nil
}

func (s *Store) UpdateMailIdentity(ctx context.Context, accountID, folder, messageID string, uid, uidValidity uint32, flags []string) error {
	if accountID == "" || folder == "" || messageID == "" || uid == 0 || uidValidity == 0 {
		return nil
	}
	// Prefer a unique row in the current folder. If a provider reused a
	// Message-ID, never guess between multiple indexed messages; UID plus
	// UIDVALIDITY is only safe when the row is already uniquely identifiable.
	var id int64
	var count int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(id),0) FROM mail_messages WHERE account_id=? AND folder=? AND message_id=?`, accountID, folder, messageID).Scan(&count, &id)
	if err != nil {
		return err
	}
	if count == 0 {
		err = s.DB.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(id),0) FROM mail_messages WHERE account_id=? AND message_id=?`, accountID, messageID).Scan(&count, &id)
		if err != nil {
			return err
		}
	}
	if count != 1 || id == 0 {
		return nil
	}
	seen := 0
	for _, flag := range flags {
		if strings.EqualFold(strings.TrimSpace(flag), imapSeenFlag) {
			seen = 1
			break
		}
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE mail_messages SET folder=?,imap_uid=?,uid_validity=?,message_flags=?,is_read=?,updated_at=? WHERE id=? AND account_id=?`, folder, uid, uidValidity, strings.Join(flags, " "), seen, time.Now().UTC().Format(time.RFC3339Nano), id, accountID)
	return err
}

// UpdateMailIdentityByUID records flags for messages whose provider omitted a
// Message-ID header. UID plus UIDVALIDITY is the only stable identity
// available in that case, and the folder is part of the lookup because UIDs
// are scoped to an IMAP mailbox.
func (s *Store) UpdateMailIdentityByUID(ctx context.Context, accountID, folder string, uid, uidValidity uint32, flags []string) error {
	if accountID == "" || folder == "" || uid == 0 || uidValidity == 0 {
		return nil
	}
	seen := 0
	for _, flag := range flags {
		if strings.EqualFold(strings.TrimSpace(flag), imapSeenFlag) {
			seen = 1
			break
		}
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE mail_messages SET imap_uid=?,uid_validity=?,message_flags=?,is_read=?,updated_at=? WHERE account_id=? AND folder=? AND imap_uid=? AND uid_validity=?`, uid, uidValidity, strings.Join(flags, " "), seen, time.Now().UTC().Format(time.RFC3339Nano), accountID, folder, uid, uidValidity)
	return err
}

// PruneRemoteFolders removes folder records no longer returned by a complete
// IMAP discovery pass. Indexed messages are reconciled separately by the
// Maildir indexer, so an empty deleted folder cannot remain in the phone menu.
func (s *Store) PruneRemoteFolders(ctx context.Context, accountID string, remotePaths []string) error {
	if accountID == "" {
		return fmt.Errorf("account identity is required")
	}
	if len(remotePaths) == 0 {
		_, err := s.DB.ExecContext(ctx, `DELETE FROM remote_folders WHERE account_id=?`, accountID)
		return err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(remotePaths)), ",")
	args := make([]any, 0, len(remotePaths)+1)
	args = append(args, accountID)
	for _, path := range remotePaths {
		args = append(args, path)
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM remote_folders WHERE account_id=? AND remote_path NOT IN (`+placeholders+`)`, args...)
	return err
}

const imapSeenFlag = "\\Seen"

func (s *Store) ReplaceAttachmentMetadata(ctx context.Context, messageID int64, attachments []AttachmentMetadata) error {
	if messageID == 0 {
		return fmt.Errorf("message identity is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM attachment_metadata WHERE message_id=?`, messageID); err != nil {
		return err
	}
	for _, attachment := range attachments {
		if strings.TrimSpace(attachment.PartPath) == "" || strings.TrimSpace(attachment.Filename) == "" {
			return fmt.Errorf("attachment metadata identity is required")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachment_metadata(message_id,part_path,filename,content_type,size,content_id,disposition,playable) VALUES(?,?,?,?,?,?,?,?)`, messageID, attachment.PartPath, attachment.Filename, attachment.ContentType, attachment.Size, attachment.ContentID, attachment.Disposition, attachment.Playable); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpsertRemoteFolder(ctx context.Context, accountID, remotePath, localPath, role string) error {
	remotePath = strings.TrimSpace(remotePath)
	localPath = filepathCleanFolder(localPath)
	if accountID == "" || remotePath == "" {
		return fmt.Errorf("remote folder identity is required")
	}
	parent := ""
	if slash := strings.LastIndex(strings.ReplaceAll(remotePath, "\\", "/"), "/"); slash > 0 {
		parent = remotePath[:slash]
	}
	depth := 0
	if parent != "" {
		depth = strings.Count(strings.ReplaceAll(remotePath, "\\", "/"), "/")
	}
	display := remotePath
	if slash := strings.LastIndex(strings.ReplaceAll(remotePath, "\\", "/"), "/"); slash >= 0 && slash+1 < len(remotePath) {
		display = remotePath[slash+1:]
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO remote_folders(account_id,remote_path,local_path,parent_path,depth,role,display_name,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(account_id,remote_path) DO UPDATE SET local_path=excluded.local_path,parent_path=excluded.parent_path,depth=excluded.depth,role=excluded.role,display_name=excluded.display_name,updated_at=excluded.updated_at`, accountID, remotePath, localPath, parent, depth, role, display, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) DeleteMailIndex(ctx context.Context, userID string, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM mail_messages WHERE id=? AND account_id IN (SELECT id FROM accounts WHERE user_id=?)`, id, userID)
	return err
}

func (s *Store) RecordMessageMutation(ctx context.Context, messageID int64, userID, operation, fromFolder, toFolder, status string, mutationErr error) error {
	if messageID == 0 || userID == "" || operation == "" || status == "" {
		return fmt.Errorf("message mutation identity is required")
	}
	errText := ""
	if mutationErr != nil {
		errText = mutationErr.Error()
	}
	result, err := s.DB.ExecContext(ctx, `
INSERT INTO message_mutations(message_id,user_id,operation,from_folder,to_folder,status,error,created_at)
SELECT m.id, a.user_id, ?, ?, ?, ?, ?, ?
FROM mail_messages m JOIN accounts a ON a.id=m.account_id
WHERE m.id=? AND a.user_id=?`, operation, fromFolder, toFolder, status, errText, time.Now().UTC().Format(time.RFC3339Nano), messageID, userID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("message was not found for user")
	}
	return nil
}

// CreateOutboundSubmission journals an SMTP attempt before any network bytes
// are sent. The Message-ID is the durable reconciliation key.
func (s *Store) CreateOutboundSubmission(ctx context.Context, submission OutboundSubmission) error {
	if strings.TrimSpace(submission.MessageID) == "" || strings.TrimSpace(submission.UserID) == "" || strings.TrimSpace(submission.AccountID) == "" {
		return fmt.Errorf("outbound submission identity is required")
	}
	if strings.TrimSpace(submission.Status) == "" {
		submission.Status = "pending"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if submission.CreatedAt == "" {
		submission.CreatedAt = now
	}
	if submission.UpdatedAt == "" {
		submission.UpdatedAt = now
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO outbound_submissions(message_id,user_id,account_id,draft_id,status,error,created_at,updated_at,accepted_at) VALUES(?,?,?,?,?,?,?,?,?)`, submission.MessageID, submission.UserID, submission.AccountID, submission.DraftID, submission.Status, submission.Error, submission.CreatedAt, submission.UpdatedAt, submission.AcceptedAt)
	return err
}

// UpdateOutboundSubmission records the outcome without changing the stable
// Message-ID. Accepted rows retain their acceptance timestamp even if a later
// diagnostic update is attempted.
func (s *Store) UpdateOutboundSubmission(ctx context.Context, messageID, status, detail string) error {
	if strings.TrimSpace(messageID) == "" || strings.TrimSpace(status) == "" {
		return fmt.Errorf("outbound submission status identity is required")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.DB.ExecContext(ctx, `UPDATE outbound_submissions SET status=?,error=?,updated_at=?,accepted_at=CASE WHEN ?='accepted' AND COALESCE(accepted_at,'')='' THEN ? ELSE accepted_at END WHERE message_id=?`, status, detail, now, status, now, messageID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// OutboundSubmission returns one journal row by its stable Message-ID.
func (s *Store) OutboundSubmission(ctx context.Context, messageID string) (OutboundSubmission, error) {
	var submission OutboundSubmission
	err := s.DB.QueryRowContext(ctx, `SELECT message_id,user_id,account_id,draft_id,status,error,created_at,updated_at,COALESCE(accepted_at,'') FROM outbound_submissions WHERE message_id=?`, messageID).
		Scan(&submission.MessageID, &submission.UserID, &submission.AccountID, &submission.DraftID, &submission.Status, &submission.Error, &submission.CreatedAt, &submission.UpdatedAt, &submission.AcceptedAt)
	return submission, err
}

// MarkQueuedMutationsSynchronized closes the retry/reporting loop for local
// Maildir-first flag changes. A successful mbsync run is the confirmation;
// failed runs leave queued rows untouched so the next scheduled run retries
// them naturally.
func (s *Store) MarkQueuedMutationsSynchronized(ctx context.Context, accountID string) error {
	if strings.TrimSpace(accountID) == "" {
		return fmt.Errorf("account identity is required")
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE message_mutations SET status='synchronized',error='' WHERE status='queued' AND message_id IN (SELECT id FROM mail_messages WHERE account_id=?)`, accountID)
	return err
}

func (s *Store) ListMessageMutations(ctx context.Context, userID, accountID string, limit int) ([]MessageMutation, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("user identity is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `SELECT m.id,m.message_id,mm.account_id,m.operation,m.from_folder,m.to_folder,m.status,m.error,m.created_at
		FROM message_mutations m JOIN mail_messages mm ON mm.id=m.message_id JOIN accounts a ON a.id=mm.account_id
		WHERE m.user_id=? AND a.user_id=?`
	args := []any{userID, userID}
	if strings.TrimSpace(accountID) != "" {
		query += ` AND mm.account_id=?`
		args = append(args, accountID)
	}
	query += ` ORDER BY m.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageMutation
	for rows.Next() {
		var item MessageMutation
		if err := rows.Scan(&item.ID, &item.MessageID, &item.AccountID, &item.Operation, &item.FromFolder, &item.ToFolder, &item.Status, &item.Error, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
