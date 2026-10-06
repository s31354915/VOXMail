package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTypedCallSettingsMethodsEnforceOwnership(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, user := range []User{
		{ID: "settings-owner", Username: "settings-owner", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true},
		{ID: "settings-other", Username: "settings-other", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true},
	} {
		if err := db.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('settings-account','settings-owner','Work','work@example.com','Work','imap.example','user','sealed','smtp.example','user','sealed','{}','now')`); err != nil {
		t.Fatal(err)
	}

	if got, err := db.SpeechSettings(ctx, "settings-owner"); err != nil {
		t.Fatal(err)
	} else if got.Voice == "" || got.MenuSpeed != 3 || got.EmailSpeed != 2 {
		t.Fatalf("default speech settings=%+v", got)
	}
	phone := "+15551234567"
	if err := db.SaveUserSettings(ctx, "settings-owner", UserSettings{TTSVoice: "voice-a", MenuSpeed: 4, EmailSpeed: 5, AlertsEnabled: true, AlertPhone: &phone}); err != nil {
		t.Fatal(err)
	}
	if got, err := db.UserSettings(ctx, "settings-owner"); err != nil {
		t.Fatal(err)
	} else if got.TTSVoice != "voice-a" || got.MenuSpeed != 4 || got.EmailSpeed != 5 || !got.AlertsEnabled || got.AlertPhone == nil || *got.AlertPhone != phone {
		t.Fatalf("saved user settings=%+v", got)
	}
	if err := db.SetAccountCallAlertEnabled(ctx, "settings-owner", "settings-account", true); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountCallAlertEnabled(ctx, "settings-other", "settings-account", false); err != sql.ErrNoRows {
		t.Fatalf("foreign account update error=%v, want sql.ErrNoRows", err)
	}
	if err := db.SetAccountAlertFolders(ctx, "settings-owner", "settings-account", []string{"Inbox", "Alerts"}); err != nil {
		t.Fatal(err)
	}
	var enabled int
	var folders string
	if err := db.DB.QueryRowContext(ctx, `SELECT call_alert_enabled,alert_folders FROM accounts WHERE id='settings-account'`).Scan(&enabled, &folders); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || folders != `["Inbox","Alerts"]` {
		t.Fatalf("persisted account settings enabled=%d folders=%q", enabled, folders)
	}
	if err := db.SetAlertsEnabled(ctx, "settings-owner", true); err != nil {
		t.Fatal(err)
	}
	if got, err := db.AlertsEnabled(ctx, "settings-owner"); err != nil || !got {
		t.Fatalf("alerts enabled=%v err=%v", got, err)
	}
	if err := db.SetAlertsEnabled(ctx, "settings-other", false); err != nil {
		t.Fatal(err)
	}
	if got, err := db.AlertsEnabled(ctx, "settings-other"); err != nil || got {
		t.Fatalf("other alerts enabled=%v err=%v", got, err)
	}
	if err := db.SetAccountAlertFolders(ctx, "settings-other", "settings-account", []string{"Private"}); err != sql.ErrNoRows {
		t.Fatalf("foreign folder update error=%v, want sql.ErrNoRows", err)
	}
	if err := db.UpdatePasswordHash(ctx, "settings-owner", "password-hash"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdatePINHash(ctx, "settings-owner", "pin-hash"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdatePasswordHash(ctx, "settings-other", ""); err == nil {
		t.Fatal("empty credential hash was accepted")
	}
	var passwordHash, pinHash string
	if err := db.DB.QueryRowContext(ctx, `SELECT password_hash,pin_hash FROM users WHERE id=?`, "settings-owner").Scan(&passwordHash, &pinHash); err != nil {
		t.Fatal(err)
	}
	if passwordHash != "password-hash" || pinHash != "pin-hash" {
		t.Fatalf("credential hashes=%q,%q", passwordHash, pinHash)
	}
	if got, err := db.SpeechSettings(ctx, "missing-user"); err != sql.ErrNoRows || !reflect.DeepEqual(got, SpeechSettings{}) {
		t.Fatalf("missing speech settings=%+v err=%v", got, err)
	}
}
