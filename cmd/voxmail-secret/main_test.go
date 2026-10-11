package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

func TestCredentialCheckVerifiesRestoredAccountAndSIPSecrets(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, err := secret.New("test-key-with-more-than-32-characters-123456")
	if err != nil {
		t.Fatal(err)
	}
	imapPassword, err := box.Seal("imap-secret")
	if err != nil {
		t.Fatal(err)
	}
	smtpPassword, err := box.Seal("smtp-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,enabled,created_at) VALUES('u1','restore-user','hash','hash','admin',1,'now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a1','u1','restore','restore@example.test','Restore','imap.example.test','restore',?,'smtp.example.test','restore',?,'{}','now')`, imapPassword, smtpPassword); err != nil {
		t.Fatal(err)
	}
	if err := checkAccountSecrets(context.Background(), db, box, "a1"); err != nil {
		t.Fatalf("valid account secrets: %v", err)
	}
	if err := db.UpsertSIP(context.Background(), box, store.SIPSettings{Domain: "sip.example.test", Username: "restore", Password: "sip-secret", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := checkSIPSecret(context.Background(), db, box); err != nil {
		t.Fatalf("valid SIP secret: %v", err)
	}
	if _, err := db.DB.Exec(`UPDATE accounts SET smtp_password='not-ciphertext' WHERE id='a1'`); err != nil {
		t.Fatal(err)
	}
	if err := checkAccountSecrets(context.Background(), db, box, "a1"); err == nil {
		t.Fatal("credential check accepted an undecryptable SMTP password")
	}
	if _, err := db.DB.Exec(`UPDATE sip_settings SET password='not-ciphertext' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := checkSIPSecret(context.Background(), db, box); err == nil {
		t.Fatal("credential check accepted an undecryptable SIP password")
	}
}
