package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedPathCollisionAccount(t *testing.T, db *Store, account, user string) {
	t.Helper()
	seedReadStateAccount(t, db, account, user)
}

func insertMailRow(t *testing.T, db *Store, account, folder, path, messageID string, read bool, alerted, attempts int) int64 {
	t.Helper()
	res, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,is_read,alerted,alert_attempts,updated_at,imap_uid,uid_validity)
 VALUES(?,?,?,?,?,?,?,?,?,?)`, account, folder, path, messageID, boolInt(read), alerted, attempts, "2026-10-19T10:00:00Z", 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func countRowsAt(t *testing.T, db *Store, account, path string) int {
	t.Helper()
	var n int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages WHERE account_id=? AND path=?`, account, path).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A flag rename indexed under the new path must not make the read toggle fail
// with UNIQUE constraint failed: mail_messages.path. The collision row is the
// same physical message, so it merges into the row the mutation anchors on.
func TestMarkMailReadAtPathMergesIndexedRename(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedPathCollisionAccount(t, db, "a20", "u20")
	base := "1699000000.M123P456.host"
	newPath := "/mail/Inbox/new/" + base
	curPath := "/mail/Inbox/cur/" + base + ":2,S"
	origID := insertMailRow(t, db, "a20", "Inbox", newPath, "", false, 0, 0)
	// The indexer saw the renamed file first and wrote its own row at the cur/
	// path, carrying a stronger alert state and a parsed Message-ID.
	insertMailRow(t, db, "a20", "Inbox", curPath, "<x@example.com>", true, 3, 2)

	if err := db.MarkMailReadAtPath(ctx, "u20", origID, true, curPath); err != nil {
		t.Fatalf("mark read at renamed path failed: %v", err)
	}
	if n := countRowsAt(t, db, "a20", curPath); n != 1 {
		t.Fatalf("expected a single row at the cur path, got %d", n)
	}
	var read, alerted, attempts int
	var messageID string
	if err := db.DB.QueryRow(`SELECT is_read,alerted,alert_attempts,COALESCE(message_id,'') FROM mail_messages WHERE id=?`, origID).Scan(&read, &alerted, &attempts, &messageID); err != nil {
		t.Fatal(err)
	}
	if read != 1 {
		t.Fatalf("read state was not applied: %d", read)
	}
	if alerted != 3 || attempts != 2 {
		t.Fatalf("alert state was not carried across: alerted=%d attempts=%d", alerted, attempts)
	}
	if messageID != "<x@example.com>" {
		t.Fatalf("message id was not carried across: %q", messageID)
	}
}

// Two genuinely different messages cannot share one physical path; the caller
// gets a descriptive conflict instead of a raw constraint error.
func TestMarkMailReadAtPathReportsForeignPathConflict(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedPathCollisionAccount(t, db, "a21", "u21")
	base := "1699000001.M1P1.host"
	origID := insertMailRow(t, db, "a21", "Inbox", "/mail/Inbox/new/"+base, "<a@example.com>", false, 0, 0)
	insertMailRow(t, db, "a21", "Inbox", "/mail/Inbox/cur/"+base+":2,S", "<b@example.com>", false, 0, 0)

	err = db.MarkMailReadAtPath(ctx, "u21", origID, true, "/mail/Inbox/cur/"+base+":2,S")
	if err == nil || !strings.Contains(err.Error(), "different message") {
		t.Fatalf("expected a descriptive conflict, got %v", err)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages WHERE account_id='a21'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("conflict must not delete rows, have %d", n)
	}
}

func TestMoveMailIndexMergesCollision(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedPathCollisionAccount(t, db, "a22", "u22")
	origID := insertMailRow(t, db, "a22", "Inbox", "/mail/Inbox/cur/1699000002.M1P1.host:2,", "", false, 1, 1)
	insertMailRow(t, db, "a22", "Archive", "/mail/Archive/cur/1699000002.M1P1.host:2,S", "<m@example.com>", false, 0, 0)

	if err := db.MoveMailIndex(ctx, "u22", origID, "Archive", "/mail/Archive/cur/1699000002.M1P1.host:2,S"); err != nil {
		t.Fatalf("move failed: %v", err)
	}
	if n := countRowsAt(t, db, "a22", "/mail/Archive/cur/1699000002.M1P1.host:2,S"); n != 1 {
		t.Fatalf("expected one row at the destination, got %d", n)
	}
}

// The guarded rollback path used by the sync worker must survive the same
// collision instead of surfacing a constraint error.
func TestMarkConfirmedReadAtPathMergesCollision(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedPathCollisionAccount(t, db, "a23", "u23")
	base := "1699000003.M1P1.host"
	origID := insertMailRow(t, db, "a23", "Inbox", "/mail/Inbox/new/"+base, "", false, 0, 0)
	insertMailRow(t, db, "a23", "Inbox", "/mail/Inbox/cur/"+base+":2,S", "", true, 0, 0)
	var origPath string
	if err := db.DB.QueryRow(`SELECT path FROM mail_messages WHERE id=?`, origID).Scan(&origPath); err != nil {
		t.Fatal(err)
	}
	item := MailSummary{ID: origID, AccountID: "a23", Folder: "Inbox", Path: origPath, UID: 42, UIDValidity: 7}

	if err := db.MarkConfirmedReadAtPath(ctx, item, true, "/mail/Inbox/cur/"+base+":2,S"); err != nil {
		t.Fatalf("confirmed read failed: %v", err)
	}
	if n := countRowsAt(t, db, "a23", "/mail/Inbox/cur/"+base+":2,S"); n != 1 {
		t.Fatalf("expected one row at the destination, got %d", n)
	}
}

// Indexing the renamed file must re-home the existing row by the Maildir base
// filename even when the message carries no Message-ID header, instead of
// inserting the duplicate that collides with the read toggle.
func TestUpsertMailIndexRehomesByMaildirBase(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedPathCollisionAccount(t, db, "a24", "u24")
	base := "1699000004.M1P1.host"
	oldPath := "/mail/Inbox/new/" + base
	newPath := "/mail/Inbox/cur/" + base + ":2,S"
	origID := insertMailRow(t, db, "a24", "Inbox", oldPath, "", false, 2, 1)

	// The rename removed the file from new/, so only the cur/ path is live.
	id, err := db.UpsertMailIndex(ctx, MailIndexInput{AccountID: "a24", Folder: "Inbox", Path: newPath, Read: true, PathIsLive: func(p string) bool { return p == newPath }})
	if err != nil {
		t.Fatal(err)
	}
	if id != origID {
		t.Fatalf("expected the existing row to be re-homed, got new id %d", id)
	}
	if n := countRowsAt(t, db, "a24", newPath); n != 1 {
		t.Fatalf("expected a single row after re-home, got %d", n)
	}
	var read, alerted, attempts int
	if err := db.DB.QueryRow(`SELECT is_read,alerted,alert_attempts FROM mail_messages WHERE id=?`, origID).Scan(&read, &alerted, &attempts); err != nil {
		t.Fatal(err)
	}
	if read != 1 {
		t.Fatalf("renamed file's read flag was not applied: %d", read)
	}
	if alerted != 2 || attempts != 1 {
		t.Fatalf("alert state lost on re-home: %d/%d", alerted, attempts)
	}
}

// A second live copy of the same message is a legitimate duplicate and keeps
// its own row.
func TestUpsertMailIndexKeepsLiveCopies(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedPathCollisionAccount(t, db, "a25", "u25")
	base := "1699000005.M1P1.host"
	first := insertMailRow(t, db, "a25", "Inbox", "/mail/Inbox/cur/"+base+":2,", "", false, 0, 0)
	_, err = db.UpsertMailIndex(ctx, MailIndexInput{AccountID: "a25", Folder: "Inbox", Path: "/mail/Inbox/cur/" + base + ":2,S", Read: true, PathIsLive: func(p string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages WHERE account_id='a25'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("live copies must each keep a row, have %d", n)
	}
	_ = first
}

// The startup cleanup collapses already-stuck duplicate pairs: same account,
// same folder, same Maildir base filename. The row matching the file on disk
// survives, and the stronger alert state carries across.
func TestMergeMaildirPathDuplicates(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedPathCollisionAccount(t, db, "a26", "u26")
	base := "1699000006.M1P1.host"
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Inbox", "cur"), 0o755); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(root, "Inbox", "cur", base+":2,S")
	if err := os.WriteFile(livePath, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	origID := insertMailRow(t, db, "a26", "Inbox", "/gone/Inbox/new/"+base, "", false, 0, 0)
	insertMailRow(t, db, "a26", "Inbox", livePath, "", true, 4, 3)
	// A message in a different folder with the same base must survive.
	insertMailRow(t, db, "a26", "Archive", "/mail/Archive/cur/"+base+":2,S", "<keep@example.com>", false, 0, 0)

	if err := db.mergeMaildirPathDuplicates(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRowsAt(t, db, "a26", livePath); n != 1 {
		t.Fatalf("expected one row at the live path, got %d", n)
	}
	var read, alerted, attempts int
	var id int64
	if err := db.DB.QueryRow(`SELECT id,is_read,alerted,alert_attempts FROM mail_messages WHERE path=?`, livePath).Scan(&id, &read, &alerted, &attempts); err != nil {
		t.Fatal(err)
	}
	if id == origID {
		t.Fatalf("the stale new/ row was kept instead of being removed (survivor id %d)", id)
	}
	if read != 1 || alerted != 4 || attempts != 3 {
		t.Fatalf("merged state wrong: read=%d alerted=%d attempts=%d", read, alerted, attempts)
	}
	var keep int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages WHERE account_id='a26' AND folder='Archive'`).Scan(&keep); err != nil {
		t.Fatal(err)
	}
	if keep != 1 {
		t.Fatalf("other folders must not be touched, have %d", keep)
	}
	// Running it again must be a no-op.
	if err := db.mergeMaildirPathDuplicates(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRowsAt(t, db, "a26", livePath); n != 1 {
		t.Fatalf("second pass changed the index, got %d rows at the live path", n)
	}
}

func TestMaildirBaseName(t *testing.T) {
	cases := map[string]string{
		"/mail/Inbox/new/1699.M1P1.host":            "1699.M1P1.host",
		"/mail/Inbox/cur/1699.M1P1.host:2,S":        "1699.M1P1.host",
		"/mail/Inbox/cur/1699.M1P1.host,S=100:2,RS": "1699.M1P1.host",
		"1699.M1P1.host":                            "1699.M1P1.host",
	}
	for in, want := range cases {
		if got := maildirBaseName(in); got != want {
			t.Fatalf("maildirBaseName(%q) = %q, want %q", in, got, want)
		}
	}
	if maildirFlagsRead("/mail/Inbox/cur/1699.M1P1.host:2,S") != true {
		t.Fatal("cur/:2,S should read as seen")
	}
	if maildirFlagsRead("/mail/Inbox/cur/1699.M1P1.host:2,") != false {
		t.Fatal("cur/:2, without S should read as unseen")
	}
	if maildirFlagsRead("/mail/Inbox/new/1699.M1P1.host") != false {
		t.Fatal("new/ should read as unseen")
	}
}

// Silence unused-import guards for future edits.
var _ = errors.Is
