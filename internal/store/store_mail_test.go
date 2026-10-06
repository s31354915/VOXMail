package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/secret"
)

func TestCleanFolderRejectsNestedTraversal(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{input: "Inbox", want: "Inbox"},
		{input: "Archive/Projects", want: "Archive/Projects"},
		{input: "Archive/../../outside", want: "Inbox"},
		{input: `Archive\\..\\outside`, want: "Inbox"},
		{input: "/absolute", want: "Inbox"},
	} {
		if got := filepathCleanFolder(tc.input); got != tc.want {
			t.Errorf("filepathCleanFolder(%q)=%q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestSaveAccountWithRolesRollsBackAccountOnRoleFailure(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, User{ID: "u-atomic", Username: "atomic", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	box, err := secret.New("test-key-with-more-than-32-characters-123456")
	if err != nil {
		t.Fatal(err)
	}
	account := Account{ID: "a-atomic", UserID: "u-atomic", CanonicalName: "Work", Email: "a@example.com", SenderName: "A", IMAPHost: "imap.example", IMAPUser: "u", IMAPPassword: "imap", SMTPHost: "smtp.example", SMTPUser: "u", SMTPPassword: "smtp", FolderMap: `{"INBOX":"Inbox"}`}
	injected := errors.New("injected folder-role write failure")
	db.folderRoleInsertHook = func() error { return injected }
	err = db.SaveAccountWithRoles(ctx, box, account, map[string]string{"inbox": "INBOX", "sent": "Sent"}, map[string]string{"INBOX": "Inbox", "Sent": "Sent"})
	if !errors.Is(err, injected) {
		t.Fatalf("expected injected role failure, got %v", err)
	}
	var count int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id='a-atomic'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("account survived failed combined save: %d", count)
	}
}

func TestAccountConfigVersionPreventsStaleOverwrite(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "version.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, User{ID: "u-version", Username: "version", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	box, err := secret.New("test-key-with-more-than-32-characters-123456")
	if err != nil {
		t.Fatal(err)
	}
	account := Account{ID: "a-version", UserID: "u-version", CanonicalName: "Original", Email: "a@example.com", SenderName: "A", IMAPHost: "imap.example", IMAPUser: "u", IMAPPassword: "imap", SMTPHost: "smtp.example", SMTPUser: "u", SMTPPassword: "smtp", FolderMap: `{}`}
	if err := db.SaveAccount(ctx, box, account); err != nil {
		t.Fatal(err)
	}
	accounts, err := db.ListAccounts(ctx, account.UserID)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	if accounts[0].ConfigVersion != 1 {
		t.Fatalf("initial config version=%d", accounts[0].ConfigVersion)
	}
	account.CanonicalName = "Updated"
	account.ConfigVersion = accounts[0].ConfigVersion
	account.ConfigVersionSet = true
	if err := db.SaveAccount(ctx, box, account); err != nil {
		t.Fatal(err)
	}
	accounts, err = db.ListAccounts(ctx, account.UserID)
	if err != nil || len(accounts) != 1 || accounts[0].ConfigVersion != 2 || accounts[0].CanonicalName != "Updated" {
		t.Fatalf("updated account=%+v err=%v", accounts, err)
	}
	account.CanonicalName = "Stale"
	account.ConfigVersion = 1
	if err := db.SaveAccount(ctx, box, account); !errors.Is(err, ErrAccountConfigConflict) {
		t.Fatalf("stale save error=%v", err)
	}
	accounts, err = db.ListAccounts(ctx, account.UserID)
	if err != nil || accounts[0].CanonicalName != "Updated" || accounts[0].ConfigVersion != 2 {
		t.Fatalf("stale save changed account=%+v err=%v", accounts, err)
	}
}

func TestSaveFolderRolesRejectsAmbiguousAliases(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "roles.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, User{ID: "u-roles", Username: "roles", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a-roles','u-roles','roles','roles@example.com','roles','imap','u','p','smtp','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	err = db.SaveFolderRoles(ctx, "u-roles", "a-roles", map[string]string{"inbox": "INBOX", "sent": "Sent"}, map[string]string{"INBOX": "Inbox", "Sent": "inbox"})
	if !errors.Is(err, ErrInvalidFolderRole) {
		t.Fatalf("expected typed alias error, got %v", err)
	}
}

func TestSaveDraftRejectsCrossUserUpdate(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, user := range []string{"draft-owner", "draft-other"} {
		if err := db.CreateUser(ctx, User{ID: user, Username: user, PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('draft-owner-account','draft-owner','owner','owner@example.com','owner','imap','u','p','smtp','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDraft(ctx, DraftRecord{ID: "shared-draft", UserID: "draft-owner", AccountID: "draft-owner-account", Subject: "original"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDraft(ctx, DraftRecord{ID: "shared-draft", UserID: "draft-other", AccountID: "draft-owner-account", Subject: "overwritten"}); err == nil {
		t.Fatal("cross-user draft update unexpectedly succeeded")
	}
	draft, err := db.LoadDraft(ctx, "draft-owner", "shared-draft")
	if err != nil {
		t.Fatal(err)
	}
	if draft.Subject != "original" {
		t.Fatalf("cross-user update changed draft: %+v", draft)
	}
}

func TestSaveAccountWithBlankPasswordsPreservesOwnedSecrets(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, User{ID: "u-password", Username: "passwords", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	box, err := secret.New("test-key-with-more-than-32-characters-123456")
	if err != nil {
		t.Fatal(err)
	}
	account := Account{ID: "a-password", UserID: "u-password", CanonicalName: "Work", Email: "a@example.com", SenderName: "A", IMAPHost: "imap.example", IMAPUser: "u", IMAPPassword: "imap-secret", SMTPHost: "smtp.example", SMTPUser: "u", SMTPPassword: "smtp-secret", FolderMap: `{"INBOX":"Inbox"}`}
	if err := db.SaveAccount(ctx, box, account); err != nil {
		t.Fatal(err)
	}
	var beforeIMAP, beforeSMTP string
	if err := db.DB.QueryRowContext(ctx, `SELECT imap_password,smtp_password FROM accounts WHERE id=?`, account.ID).Scan(&beforeIMAP, &beforeSMTP); err != nil {
		t.Fatal(err)
	}
	account.IMAPHost = "new-imap.example"
	account.IMAPPassword = ""
	account.SMTPPassword = ""
	if err := db.SaveAccount(ctx, box, account); err != nil {
		t.Fatal(err)
	}
	var afterIMAP, afterSMTP string
	if err := db.DB.QueryRowContext(ctx, `SELECT imap_password,smtp_password FROM accounts WHERE id=?`, account.ID).Scan(&afterIMAP, &afterSMTP); err != nil {
		t.Fatal(err)
	}
	if beforeIMAP != afterIMAP || beforeSMTP != afterSMTP {
		t.Fatal("blank edit passwords replaced existing encrypted secrets")
	}
	gotIMAP, err := box.Open(afterIMAP)
	if err != nil {
		t.Fatal(err)
	}
	gotSMTP, err := box.Open(afterSMTP)
	if err != nil {
		t.Fatal(err)
	}
	if gotIMAP != "imap-secret" || gotSMTP != "smtp-secret" {
		t.Fatalf("preserved secrets=%q,%q", gotIMAP, gotSMTP)
	}
}

func TestReorderAccountsRequiresCompleteOwnedSet(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, User{ID: "u-order", Username: "order", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a-order-1", "a-order-2"} {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,display_order,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, "u-order", id, id+"@example.com", id, "imap", "u", "p", "smtp", "u", "p", "{}", 0, "now"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ReorderAccounts(ctx, "u-order", []string{"a-order-2", "a-order-1"}); err != nil {
		t.Fatal(err)
	}
	var first, second int
	if err := db.DB.QueryRowContext(ctx, `SELECT display_order FROM accounts WHERE id='a-order-1'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRowContext(ctx, `SELECT display_order FROM accounts WHERE id='a-order-2'`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != 0 {
		t.Fatalf("orders=%d,%d", first, second)
	}
	if err := db.ReorderAccounts(ctx, "u-order", []string{"a-order-1", "foreign"}); err == nil {
		t.Fatal("foreign account ID was accepted")
	}
	if err := db.DB.QueryRowContext(ctx, `SELECT display_order FROM accounts WHERE id='a-order-1'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRowContext(ctx, `SELECT display_order FROM accounts WHERE id='a-order-2'`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != 0 {
		t.Fatalf("failed reorder was not rolled back: orders=%d,%d", first, second)
	}
	if err := db.ReorderAccounts(ctx, "u-order", []string{"a-order-1", "a-order-1"}); err == nil {
		t.Fatal("duplicate account ID was accepted")
	}
}

func TestListMailUnreadOnlyUsesMappedInbox(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u','u','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','u','Work','a@example.com','Work','imap.example','u','p','smtp.example','u','p','{"INBOX":"Inbox","Junk":"Spam","Trash":"Trash"}','now')`); err != nil {
		t.Fatal(err)
	}
	for i, folder := range []string{"Inbox", "Spam", "Trash"} {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,subject,is_read,updated_at) VALUES('a',?,?,0,0,'now')`, folder, filepath.Join(t.TempDir(), string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := db.ListMail(ctx, "u", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Folder != "Inbox" {
		t.Fatalf("unread result=%+v, want only Inbox", messages)
	}
	count, err := db.CountUnreadForAccount(ctx, "u", "a")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("unread count=%d, want 1", count)
	}
}

func TestMailDateOrderingAndKeysetPagination(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('date-user','date-user','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('date-account','date-user','date','date@example.com','date','imap','u','p','smtp','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	dates := []string{
		"Wed, 01 Jan 2025 12:00:00 +0200",
		"Wed, 01 Jan 2025 11:00:00 +0000",
		"not-a-date",
	}
	for index, raw := range dates {
		var normalized any
		if value := NormalizeMessageDate(raw); value != nil {
			normalized = *value
		}
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,subject,message_date,message_date_utc,updated_at) VALUES(?,?,?,?,?,?,?)`, "date-account", "Inbox", fmt.Sprintf("/date/%d", index), fmt.Sprintf("message-%d", index), raw, normalized, "now"); err != nil {
			t.Fatal(err)
		}
	}
	all, err := db.ListMailForAccount(ctx, "date-user", "date-account", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Subject != "message-0" || all[1].Subject != "message-1" || all[2].Subject != "message-2" {
		t.Fatalf("date ordering=%+v", all)
	}
	if all[0].DateUTC == nil || all[1].DateUTC == nil || all[2].DateUTC != nil {
		t.Fatalf("normalized dates=%v,%v,%v", all[0].DateUTC, all[1].DateUTC, all[2].DateUTC)
	}
	first, err := db.ListMailPage(ctx, "date-user", "date-account", "", false, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 2 || first.Next == nil {
		t.Fatalf("first page=%+v", first)
	}
	second, err := db.ListMailPage(ctx, "date-user", "date-account", "", false, first.Next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 1 || second.Messages[0].Subject != "message-2" || second.Next != nil {
		t.Fatalf("second page=%+v", second)
	}
}

func TestMailPageQueryPlanDiagnostics(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.DB.Query(`EXPLAIN QUERY PLAN
SELECT m.id FROM mail_messages m
JOIN accounts a ON a.id=m.account_id
WHERE a.user_id=? AND COALESCE(a.deleting,0)=0
  AND m.account_id=? AND m.folder=? AND m.is_read=0
ORDER BY CASE WHEN m.message_date_utc IS NULL THEN 1 ELSE 0 END,m.message_date_utc,m.id DESC
LIMIT ?`, "u", "a", "Inbox", 51)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	usedPageIndex := false
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		usedPageIndex = usedPageIndex || strings.Contains(detail, "mail_message_page_idx")
		t.Logf("query plan: id=%d parent=%d detail=%s", id, parent, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !usedPageIndex {
		t.Fatal("mail page query did not use mail_message_page_idx")
	}
}

func BenchmarkListMailPageRepresentative(b *testing.B) {
	db, err := Open(filepath.Join(b.TempDir(), "mail.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('bench-user','bench-user','p','p','user','now')`); err != nil {
		b.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('bench-account','bench-user','bench','bench@example.com','bench','imap','u','p','smtp','u','p','{}','now')`); err != nil {
		b.Fatal(err)
	}
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 10000; index++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,subject,message_date,message_date_utc,is_read,updated_at) VALUES(?,?,?,?,?,?,?,'now')`, "bench-account", "Inbox", fmt.Sprintf("/bench/%d", index), "subject", fmt.Sprintf("date-%d", index), index+1, 0); err != nil {
			_ = tx.Rollback()
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		page, err := db.ListMailPage(ctx, "bench-user", "bench-account", "Inbox", false, nil, 100)
		if err != nil {
			b.Fatal(err)
		}
		if len(page.Messages) != 100 {
			b.Fatalf("page length=%d, want 100", len(page.Messages))
		}
	}
}

func TestUpdateMailIdentitySurvivesRemoteFolderChange(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u2','u2','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a2','u2','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,message_id,is_read,updated_at) VALUES('a2','Inbox','/mail/old','<stable@example.com>',0,'now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailIdentity(ctx, "a2", "Archive/Projects", "<stable@example.com>", 42, 7, []string{"\\Seen"}); err != nil {
		t.Fatal(err)
	}
	var folder, path, flags string
	var read, uid, validity int
	if err := db.DB.QueryRowContext(ctx, `SELECT folder,path,message_flags,is_read,imap_uid,uid_validity FROM mail_messages WHERE account_id='a2'`).Scan(&folder, &path, &flags, &read, &uid, &validity); err != nil {
		t.Fatal(err)
	}
	if folder != "Archive/Projects" || path != "/mail/old" || flags != "\\Seen" || read != 1 || uid != 42 || validity != 7 {
		t.Fatalf("identity update=%q %q %q read=%d uid=%d validity=%d", folder, path, flags, read, uid, validity)
	}
}

func TestUpdateMailIdentityDoesNotGuessDuplicateMessageIDs(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('dup-user','dup-user','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('dup-account','dup-user','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	for i, folder := range []string{"Inbox", "Archive"} {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,message_id,is_read,updated_at) VALUES('dup-account',?,?,?,0,'now')`, folder, filepath.Join(t.TempDir(), string(rune('a'+i))), "<duplicate@example.com>"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpdateMailIdentity(ctx, "dup-account", "Other", "<duplicate@example.com>", 42, 7, []string{"\\Seen"}); err != nil {
		t.Fatal(err)
	}
	var updated int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_messages WHERE account_id='dup-account' AND imap_uid=42`).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if updated != 0 {
		t.Fatalf("duplicate Message-ID update changed %d rows", updated)
	}
}

func TestListMailFoldersIncludesEmptyDiscoveredFolders(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u3','u3','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a3','u3','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a3", "Archive/Projects", "Archive/Projects", ""); err != nil {
		t.Fatal(err)
	}
	folders, err := db.ListMailFolders(ctx, "u3", "a3")
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0] != "Archive/Projects" {
		t.Fatalf("folders=%v, want discovered empty folder", folders)
	}
}

func TestUpdateMailIdentityByUIDUpdatesFlagsWithoutMessageID(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u4','u4','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a4','u4','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,imap_uid,uid_validity,is_read,updated_at) VALUES('a4','Inbox','/mail/no-message-id',9,4,0,'now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailIdentityByUID(ctx, "a4", "Inbox", 9, 4, []string{"\\Seen", "\\Answered"}); err != nil {
		t.Fatal(err)
	}
	var flags string
	var read int
	if err := db.DB.QueryRowContext(ctx, `SELECT message_flags,is_read FROM mail_messages WHERE account_id='a4'`).Scan(&flags, &read); err != nil {
		t.Fatal(err)
	}
	if flags != "\\Seen \\Answered" || read != 1 {
		t.Fatalf("flags=%q read=%d", flags, read)
	}
}

func TestPruneRemoteFoldersRemovesDeletedFolders(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u5','u5','p','p','user','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a5','u5','a','a@e','a','h','u','p','h','u','p','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a5", "INBOX", "Inbox", "inbox"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRemoteFolder(ctx, "a5", "Archive", "Archive", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.PruneRemoteFolders(ctx, "a5", []string{"INBOX"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_folders WHERE account_id='a5'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("remaining folder count=%d, want 1", count)
	}
}
