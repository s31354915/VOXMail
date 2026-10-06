package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenConfiguresSQLiteConnectionPolicy(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	var foreignKeys, busyTimeout int
	if err := db.DB.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys=%d, want 1", foreignKeys)
	}
	if busyTimeout != sqliteBusyTimeout {
		t.Fatalf("busy_timeout=%d, want %d", busyTimeout, sqliteBusyTimeout)
	}
	var journalMode string
	if err := db.DB.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal_mode=%q, want wal", journalMode)
	}
}

func TestOpenReadOnlyDoesNotMigrateOrWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readonly.db")
	created, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	var tables int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='users'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 1 {
		t.Fatalf("users table count=%d, want 1", tables)
	}
	var foreignKeys, busyTimeout, queryOnly int
	if err := db.DB.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&queryOnly); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || busyTimeout != sqliteBusyTimeout || queryOnly != 1 {
		t.Fatalf("read-only policy foreign_keys=%d busy_timeout=%d query_only=%d", foreignKeys, busyTimeout, queryOnly)
	}
	if _, err := db.DB.ExecContext(ctx, `CREATE TABLE should_not_exist(id INTEGER)`); err == nil {
		t.Fatal("read-only database accepted a schema write")
	}
}

func TestOpenReadOnlyRequiresExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if _, err := OpenReadOnly(path); err == nil {
		t.Fatal("OpenReadOnly created or accepted a missing database")
	}
}

func TestMigrateHistoricalSchemaFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "historical.db")
	legacy, err := openDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.ExecContext(context.Background(), `
CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
CREATE TABLE users(id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, pin_hash TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'user', enabled INTEGER NOT NULL DEFAULT 1, totp_secret TEXT, backup_codes TEXT, created_at TEXT NOT NULL);
CREATE TABLE accounts(id TEXT PRIMARY KEY, user_id TEXT NOT NULL, canonical_name TEXT NOT NULL, email TEXT NOT NULL, sender_name TEXT NOT NULL, imap_host TEXT NOT NULL, imap_port INTEGER NOT NULL DEFAULT 993, imap_user TEXT NOT NULL, imap_password TEXT NOT NULL, imap_security TEXT NOT NULL DEFAULT 'implicit_tls', smtp_host TEXT NOT NULL, smtp_port INTEGER NOT NULL DEFAULT 465, smtp_security TEXT NOT NULL DEFAULT 'implicit_tls', smtp_user TEXT NOT NULL, smtp_password TEXT NOT NULL, folder_map TEXT NOT NULL DEFAULT '{}', sync_interval_minutes INTEGER NOT NULL DEFAULT 5, reconciliation_interval_minutes INTEGER NOT NULL DEFAULT 1440, initial_cutoff TEXT, retention_days INTEGER, call_alert_enabled INTEGER NOT NULL DEFAULT 0, alert_folders TEXT NOT NULL DEFAULT '[]', display_order INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
CREATE TABLE settings(user_id TEXT PRIMARY KEY, tts_voice TEXT NOT NULL DEFAULT 'en_US-hfc_male-medium', menu_speed INTEGER NOT NULL DEFAULT 3, email_speed INTEGER NOT NULL DEFAULT 2, alerts_enabled INTEGER NOT NULL DEFAULT 0, alert_phone TEXT, alert_next_dial TEXT NOT NULL DEFAULT '');
CREATE TABLE system_settings(id INTEGER PRIMARY KEY CHECK(id=1), alerts_available INTEGER NOT NULL DEFAULT 1, setup_completed INTEGER NOT NULL DEFAULT 0);
CREATE TABLE sip_settings(id INTEGER PRIMARY KEY CHECK(id=1), domain TEXT NOT NULL DEFAULT '', username TEXT NOT NULL DEFAULT '', password TEXT NOT NULL DEFAULT '', port INTEGER NOT NULL DEFAULT 5060, transport TEXT NOT NULL DEFAULT 'udp', reg_interval INTEGER NOT NULL DEFAULT 300, enabled INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL);
CREATE TABLE mail_messages(id INTEGER PRIMARY KEY, account_id TEXT NOT NULL, folder TEXT NOT NULL, path TEXT NOT NULL UNIQUE, message_id TEXT, sender TEXT, recipients TEXT, cc TEXT NOT NULL DEFAULT '', subject TEXT, message_date TEXT, is_read INTEGER NOT NULL DEFAULT 0, attachment_count INTEGER NOT NULL DEFAULT 0, alerted INTEGER NOT NULL DEFAULT 0, imap_uid INTEGER, uid_validity INTEGER, message_flags TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL, last_seen_scan TEXT NOT NULL DEFAULT '', message_date_utc INTEGER);
CREATE TABLE pin_lockouts(phone TEXT PRIMARY KEY, locked_until TEXT NOT NULL, updated_at TEXT NOT NULL);
INSERT INTO schema_migrations(version,applied_at) VALUES(1,'legacy'),(2,'legacy'),(3,'legacy'),(4,'legacy'),(5,'legacy'),(6,'legacy'),(7,'legacy'),(8,'legacy'),(9,'legacy');
INSERT INTO users(id,username,password_hash,pin_hash,created_at) VALUES('legacy-user','legacy','p','p','now');
INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,created_at) VALUES('legacy-account','legacy-user','Legacy','legacy@example.com','Legacy','imap','u','p','smtp','u','p','now');
INSERT INTO settings(user_id) VALUES('legacy-user');
INSERT INTO mail_messages(account_id,folder,path,subject,updated_at) VALUES('legacy-account','Inbox','/legacy/message','legacy','now');`)
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var sourceSize, deleting, configVersion int
	if err := db.DB.QueryRowContext(ctx, `SELECT source_size FROM mail_messages WHERE path='/legacy/message'`).Scan(&sourceSize); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRowContext(ctx, `SELECT deleting,config_version FROM accounts WHERE id='legacy-account'`).Scan(&deleting, &configVersion); err != nil {
		t.Fatal(err)
	}
	if sourceSize != -1 || deleting != 0 || configVersion != 1 {
		t.Fatalf("historical defaults source_size=%d deleting=%d config_version=%d", sourceSize, deleting, configVersion)
	}
	var migrations int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=20`).Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations != 1 {
		t.Fatal("latest migrations were not applied to historical schema")
	}
}

func TestMigrationFailureRollsBackDDLAndLedger(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.applyMigration(ctx, 9000, `CREATE TABLE migration_rollback_probe(id INTEGER); CREATE INDEX migration_rollback_bad ON table_that_does_not_exist(id)`); err == nil {
		t.Fatal("failed migration unexpectedly succeeded")
	}
	var tables, ledger int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='migration_rollback_probe'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=9000`).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || ledger != 0 {
		t.Fatalf("failed migration left tables=%d ledger=%d", tables, ledger)
	}
	if err := db.applyMigration(ctx, 9000, `CREATE TABLE migration_rollback_probe(id INTEGER)`); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkSQLiteSingleConnectionContention(b *testing.B) {
	db, err := Open(filepath.Join(b.TempDir(), "contention.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateBootstrapUser(ctx, User{ID: "contention-user", Username: "contention", PasswordHash: "p", PINHash: "p", Role: "admin", Enabled: true}); err != nil {
		b.Fatal(err)
	}
	db.DB.SetMaxOpenConns(1)
	db.DB.SetMaxIdleConns(1)
	b.SetParallelism(8)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := db.DB.ExecContext(ctx, `UPDATE settings SET menu_speed=CASE WHEN menu_speed=5 THEN 1 ELSE menu_speed+1 END WHERE user_id=?`, "contention-user"); err != nil {
				b.Errorf("contention update: %v", err)
				return
			}
			var count int
			if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE user_id=?`, "contention-user").Scan(&count); err != nil {
				b.Errorf("contention query: %v", err)
				return
			}
			if count != 1 {
				b.Errorf("settings count=%d, want 1", count)
				return
			}
		}
	})
}
