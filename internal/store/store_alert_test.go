package store

import (
	"context"
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
		folder, path string
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
	pending, err := db.PendingAlerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected unread/unalerted candidates from both folders, got %+v", pending)
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
	if _, ok := seen["Junk"]; !ok {
		t.Fatal("expected a Junk candidate (folder filtering happens in Go)")
	}
	if err := db.MarkAlertsNotified(ctx, []int64{pending[0].MessageID}); err != nil {
		t.Fatal(err)
	}
	if remaining, err := db.PendingAlerts(ctx); err != nil || len(remaining) != 1 {
		t.Fatalf("expected one remaining alert after mark, got %+v err=%v", remaining, err)
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