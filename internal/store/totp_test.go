package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestTypedTOTPMethodsResetReplayAndConsumeLegacyCodes(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "totp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, User{ID: "totp-store-user", Username: "totp-store-user", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	if err := db.EnableTOTP(ctx, "totp-store-user", "sealed-secret", []string{"code-one", "code-two"}, now); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ConsumeTOTPReplay(ctx, "totp-store-user", 100); err != nil || !ok {
		t.Fatalf("first replay result=%v err=%v", ok, err)
	}
	if ok, err := db.ConsumeTOTPReplay(ctx, "totp-store-user", 100); err != nil || ok {
		t.Fatalf("duplicate replay result=%v err=%v", ok, err)
	}
	verify := func(hash, candidate string) bool { return hash == candidate }
	if ok, err := db.ConsumeBackupCode(ctx, "totp-store-user", "code-one", verify, now); err != nil || !ok {
		t.Fatalf("normalized backup result=%v err=%v", ok, err)
	}
	if ok, err := db.ConsumeBackupCode(ctx, "totp-store-user", "code-one", verify, now); err != nil || ok {
		t.Fatalf("reused normalized backup result=%v err=%v", ok, err)
	}
	if err := db.EnableTOTP(ctx, "totp-store-user", "sealed-secret-2", []string{"fresh-code"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ConsumeTOTPReplay(ctx, "totp-store-user", 100); err != nil || !ok {
		t.Fatalf("replay was not reset by re-enable: ok=%v err=%v", ok, err)
	}
	if err := db.EnableTOTP(ctx, "totp-store-user", "sealed-secret-3", nil, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var legacyJSON string
	legacy, _ := json.Marshal([]string{"legacy-one", "legacy-two"})
	if _, err := db.DB.ExecContext(ctx, `UPDATE users SET backup_codes=? WHERE id=?`, string(legacy), "totp-store-user"); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ConsumeBackupCode(ctx, "totp-store-user", "legacy-one", verify, now.Add(2*time.Minute)); err != nil || !ok {
		t.Fatalf("legacy backup result=%v err=%v", ok, err)
	}
	if err := db.DB.QueryRowContext(ctx, `SELECT backup_codes FROM users WHERE id=?`, "totp-store-user").Scan(&legacyJSON); err != nil {
		t.Fatal(err)
	}
	if legacyJSON != `["legacy-two"]` {
		t.Fatalf("remaining legacy JSON=%q", legacyJSON)
	}
	var migrated int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM totp_backup_codes WHERE user_id=?`, "totp-store-user").Scan(&migrated); err != nil {
		t.Fatal(err)
	}
	if migrated != 1 {
		t.Fatalf("migrated backup rows=%d, want 1", migrated)
	}
	if err := db.DisableTOTP(ctx, "totp-store-user"); err != nil {
		t.Fatal(err)
	}
	user, err := db.UserByID(ctx, "totp-store-user")
	if err != nil {
		t.Fatal(err)
	}
	if user.TOTPSecret != "" || user.BackupCodes != "" {
		t.Fatalf("disabled TOTP fields secret=%q backup=%q", user.TOTPSecret, user.BackupCodes)
	}
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM totp_backup_codes WHERE user_id=?`, "totp-store-user").Scan(&migrated); err != nil {
		t.Fatal(err)
	}
	if migrated != 0 {
		t.Fatalf("backup rows survived disable: %d", migrated)
	}
}
