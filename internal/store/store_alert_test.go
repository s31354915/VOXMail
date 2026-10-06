package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/voxmail/voxmail/internal/secret"
)

func TestEnsureColumn(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.ExecContext(ctx, `CREATE TABLE probes (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := db.ensureColumn(ctx, "probes", "newness", "TEXT NOT NULL DEFAULT 'x'"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('probes') WHERE name='newness'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("column was not added")
	}
	if err := db.ensureColumn(ctx, "probes", "newness", "TEXT"); err != nil {
		t.Fatalf("re-running ensureColumn must be idempotent: %v", err)
	}
}

func TestBootstrapUserHasOneConcurrentWinner(t *testing.T) {
	db, err := Open(t.TempDir() + "/bootstrap.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	users := []User{
		{ID: "one", Username: "one", PasswordHash: "password", PINHash: "pin", Role: "admin", Enabled: true},
		{ID: "two", Username: "two", PasswordHash: "password", PINHash: "pin", Role: "admin", Enabled: true},
	}
	start := make(chan struct{})
	results := make(chan error, len(users))
	var wg sync.WaitGroup
	for _, user := range users {
		user := user
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- db.CreateBootstrapUser(context.Background(), user)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case err == ErrSetupCompleted:
			conflicts++
		default:
			t.Fatalf("unexpected bootstrap result: %v", err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d, want one of each", success, conflicts)
	}
	count, err := db.UserCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("user count=%d, want 1", count)
	}
	var completed int
	if err := db.DB.QueryRow(`SELECT setup_completed FROM system_settings WHERE id=1`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if completed != 1 {
		t.Fatalf("setup_completed=%d, want 1", completed)
	}
}

func TestCreateUserRollsBackWhenSettingsInsertFails(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.ExecContext(ctx, `CREATE TRIGGER reject_settings BEFORE INSERT ON settings BEGIN SELECT RAISE(ABORT, 'settings unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	err = db.CreateUser(ctx, User{ID: "atomic-user", Username: "atomic", PasswordHash: "hash", PINHash: "pin", Enabled: true})
	if err == nil {
		t.Fatal("CreateUser unexpectedly succeeded")
	}
	if _, lookupErr := db.UserByID(ctx, "atomic-user"); lookupErr == nil {
		t.Fatal("user row survived failed settings insert")
	}
}

func TestPendingAlertsAndMark(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, _ := secret.New("test-key-with-more-than-32-characters-123456")
	u := User{ID: "u1", Username: "ada", PasswordHash: "x", PINHash: "y", Enabled: true}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `UPDATE settings SET alerts_enabled=1, alert_phone='+15551212' WHERE user_id='u1'`); err != nil {
		t.Fatal(err)
	}
	account := Account{ID: "a1", UserID: "u1", CanonicalName: "Work", Email: "ada@example.com", SenderName: "Ada", IMAPHost: "imap.example.com", IMAPPort: 993, IMAPUser: "ada", IMAPPassword: "pw", SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPUser: "ada", SMTPPassword: "pw", AlertFolders: `["INBOX"]`, CallAlertEnabled: true}
	if err := db.SaveAccount(ctx, box, account); err != nil {
		t.Fatal(err)
	}
	mail := []struct {
		folder, path  string
		read, alerted int
	}{
		{"INBOX", "/m/a1/1", 0, 0},
		{"INBOX", "/m/a1/2", 0, 1},
		{"INBOX", "/m/a1/3", 1, 0},
		{"Junk", "/m/a1/4", 0, 0},
	}
	for _, m := range mail {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,is_read,alerted,updated_at) VALUES(?,?,?,?,?,'now')`, account.ID, m.folder, m.path, m.read, m.alerted); err != nil {
			t.Fatal(err)
		}
	}
	// More than the old candidate limit of unrelated messages must not hide
	// the configured INBOX candidate behind a Go-side filter.
	for i := 0; i < 150; i++ {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,is_read,alerted,updated_at) VALUES('a1','Junk',?,0,0,'now')`, filepath.Join(t.TempDir(), "junk", fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := db.PendingAlerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected only configured-folder candidates, got %+v", pending)
	}
	seen := map[string]struct{}{}
	for _, c := range pending {
		seen[c.Folder] = struct{}{}
		if c.Phone != "+15551212" {
			t.Fatalf("unexpected candidate %+v", c)
		}
	}
	if _, ok := seen["INBOX"]; !ok {
		t.Fatal("expected an INBOX candidate")
	}
	if _, ok := seen["Junk"]; ok {
		t.Fatal("unconfigured Junk folder should be filtered before the candidate limit")
	}
	if err := db.SetAlertsAvailable(ctx, false); err != nil {
		t.Fatal(err)
	}
	if disabledPending, err := db.PendingAlerts(ctx); err != nil || len(disabledPending) != 0 {
		t.Fatalf("global alert disable must suppress all candidates: %+v err=%v", disabledPending, err)
	}
	if err := db.SetAlertsAvailable(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkAlertsNotified(ctx, []int64{pending[0].MessageID}); err != nil {
		t.Fatal(err)
	}
	if remaining, err := db.PendingAlerts(ctx); err != nil || len(remaining) != 0 {
		t.Fatalf("expected no remaining configured-folder alerts after mark, got %+v err=%v", remaining, err)
	}

	disabled := Account{ID: "a2", UserID: "u1", CanonicalName: "Personal", Email: "p@example.com", SenderName: "P", IMAPHost: "imap.example.com", IMAPPort: 993, IMAPUser: "p", IMAPPassword: "pw", SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPUser: "p", SMTPPassword: "pw", AlertFolders: `["INBOX"]`, CallAlertEnabled: false}
	if err := db.SaveAccount(ctx, box, disabled); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,is_read,updated_at) VALUES(?,?,?,0,'now')`, "a2", "INBOX", "/m/a2/1"); err != nil {
		t.Fatal(err)
	}
	pending, err = db.PendingAlerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range pending {
		if c.AccountID == "a2" {
			t.Fatalf("disabled account must not yield alerts, got %+v", pending)
		}
	}
}

func TestPendingAlertsIncludesCompetingUsers(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir() + "/fair-alerts.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, _ := secret.New("test-key-with-more-than-32-characters-123456")
	for _, user := range []User{
		{ID: "fair-user-1", Username: "fair-one", PasswordHash: "x", PINHash: "y", Enabled: true},
		{ID: "fair-user-2", Username: "fair-two", PasswordHash: "x", PINHash: "y", Enabled: true},
	} {
		if err := db.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.ExecContext(ctx, `UPDATE settings SET alerts_enabled=1, alert_phone=? WHERE user_id=?`, "+1555"+user.ID[len(user.ID)-1:], user.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []Account{
		{ID: "fair-account-1", UserID: "fair-user-1", CanonicalName: "one", Email: "one@example.com", IMAPHost: "imap", IMAPUser: "one", IMAPPassword: "pw", SMTPHost: "smtp", SMTPUser: "one", SMTPPassword: "pw", AlertFolders: `["INBOX"]`, CallAlertEnabled: true},
		{ID: "fair-account-2", UserID: "fair-user-2", CanonicalName: "two", Email: "two@example.com", IMAPHost: "imap", IMAPUser: "two", IMAPPassword: "pw", SMTPHost: "smtp", SMTPUser: "two", SMTPPassword: "pw", AlertFolders: `["INBOX"]`, CallAlertEnabled: true},
	} {
		if err := db.SaveAccount(ctx, box, account); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 150; i++ {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,is_read,alerted,updated_at) VALUES('fair-account-1','INBOX',?,0,0,'now')`, filepath.Join(t.TempDir(), fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,is_read,alerted,updated_at) VALUES('fair-account-2','INBOX','/fair/one',0,0,'now')`); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingAlerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	users := make(map[string]int)
	for _, candidate := range pending {
		users[candidate.UserID]++
	}
	if users["fair-user-1"] == 0 || users["fair-user-2"] == 0 {
		t.Fatalf("one user monopolized pending candidates: counts=%v", users)
	}
	if users["fair-user-1"] > 10 {
		t.Fatalf("per-user candidate bound exceeded: counts=%v", users)
	}
}

func TestAlertClaimsSurviveAndReleaseLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir() + "/claims.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, _ := secret.New("test-key-with-more-than-32-characters-123456")
	u := User{ID: "claim-user", Username: "claim", PasswordHash: "x", PINHash: "y", Enabled: true}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `UPDATE settings SET alerts_enabled=1 WHERE user_id='claim-user'`); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAlertNumber(ctx, u.ID, "+15550001111", true); err != nil {
		t.Fatal(err)
	}
	account := Account{ID: "claim-account", UserID: u.ID, CanonicalName: "Work", Email: "claim@example.com", SenderName: "Claim", IMAPHost: "imap.example.com", IMAPUser: "claim", IMAPPassword: "pw", SMTPHost: "smtp.example.com", SMTPUser: "claim", SMTPPassword: "pw", AlertFolders: `["INBOX"]`, CallAlertEnabled: true}
	if err := db.SaveAccount(ctx, box, account); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,is_read,alerted,updated_at) VALUES('claim-account','INBOX','/claim/1',0,0,'now')`); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingAlerts(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	ids, err := db.ClaimAlertMessages(ctx, u.ID, []int64{pending[0].MessageID})
	if err != nil || len(ids) != 1 {
		t.Fatalf("claimed=%v err=%v", ids, err)
	}
	if again, err := db.PendingAlerts(ctx); err != nil || len(again) != 0 {
		t.Fatalf("claimed message remained pending: %+v err=%v", again, err)
	}
	if err := db.ReleaseAlertClaims(ctx, u.ID, ids); err != nil {
		t.Fatal(err)
	}
	if again, err := db.PendingAlerts(ctx); err != nil || len(again) != 1 {
		t.Fatalf("released message did not return: %+v err=%v", again, err)
	}
	ids, err = db.ClaimAlertMessages(ctx, u.ID, []int64{pending[0].MessageID})
	if err != nil || len(ids) != 1 {
		t.Fatalf("reclaim failed: %v %v", ids, err)
	}
	if err := db.MarkAlertMessagesNotified(ctx, u.ID, ids); err != nil {
		t.Fatal(err)
	}
	if remaining, err := db.PendingAlerts(ctx); err != nil || len(remaining) != 0 {
		t.Fatalf("announced message remained pending: %+v err=%v", remaining, err)
	}
}
