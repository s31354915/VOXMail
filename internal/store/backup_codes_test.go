package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const backupUser = "backup-user"

func backupFixture(t *testing.T, legacy bool, hashes []string) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "backup-codes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.CreateUser(ctx, User{ID: backupUser, Username: backupUser, PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	codes := hashes
	if legacy {
		codes = nil
	}
	if err := s.EnableTOTP(ctx, backupUser, "sealed-secret", codes, time.Now()); err != nil {
		t.Fatal(err)
	}
	if legacy {
		raw, _ := json.Marshal(hashes)
		if _, err := s.DB.ExecContext(ctx, `UPDATE users SET backup_codes=? WHERE id=?`, string(raw), backupUser); err != nil {
			t.Fatal(err)
		}
	}
	if s.DB.Stats().MaxOpenConnections != 1 {
		t.Fatal("fixture no longer represents the single-connection store")
	}
	return s
}

type backupResult struct {
	ok  bool
	err error
}

func waitVerification(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("verification did not begin outside the connection lock")
	}
}

func waitBackupResult(t *testing.T, results <-chan backupResult) backupResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("backup-code consumption did not finish")
		return backupResult{}
	}
}

func TestBackupVerificationReleasesSoleConnectionAndHasOneConcurrentWinner(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "normalized"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			s := backupFixture(t, legacy, []string{"code-one", "code-two"})
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			results := make(chan backupResult, 2)
			for range 2 {
				go func() {
					ok, err := s.ConsumeBackupCode(context.Background(), backupUser, "code-one", func(hash, candidate string) bool {
						entered <- struct{}{}
						<-release
						return hash == candidate
					}, time.Now())
					results <- backupResult{ok, err}
				}()
			}
			waitVerification(t, entered)
			// Keep verification deliberately stalled. A real unrelated query
			// must finish on the sole connection before either hash returns.
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			if _, err := s.UserByID(ctx, backupUser); err != nil {
				t.Fatalf("unrelated read blocked behind verification: %v", err)
			}
			waitVerification(t, entered)
			unblock()
			winners := 0
			for range 2 {
				result := waitBackupResult(t, results)
				if result.err != nil {
					t.Fatal(result.err)
				}
				if result.ok {
					winners++
				}
			}
			if winners != 1 {
				t.Fatalf("concurrent winners=%d, want exactly one", winners)
			}
		})
	}
}

func TestBackupSnapshotCannotConsumeReplacementWithReusedID(t *testing.T) {
	s := backupFixture(t, false, []string{"old-code"})
	var oldID, newID int64
	if err := s.DB.QueryRow(`SELECT id FROM totp_backup_codes WHERE user_id=?`, backupUser).Scan(&oldID); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	results := make(chan backupResult, 1)
	go func() {
		ok, err := s.ConsumeBackupCode(context.Background(), backupUser, "old-code", func(hash, candidate string) bool {
			entered <- struct{}{}
			<-release
			return hash == candidate
		}, time.Now())
		results <- backupResult{ok, err}
	}()
	waitVerification(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.EnableTOTP(ctx, backupUser, "replacement-secret", []string{"new-code"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM totp_backup_codes WHERE user_id=?`, backupUser).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	if oldID != newID {
		t.Fatal("fixture did not reproduce SQLite row-ID reuse")
	}
	unblock()
	if result := waitBackupResult(t, results); result.err != nil || result.ok {
		t.Fatalf("stale snapshot consumed a replacement: %+v", result)
	}
	if ok, err := s.ConsumeBackupCode(ctx, backupUser, "new-code", func(hash, candidate string) bool { return hash == candidate }, time.Now()); err != nil || !ok {
		t.Fatalf("replacement code lost: ok=%v err=%v", ok, err)
	}
}

func TestCanceledBackupVerificationDoesNotConsumeCode(t *testing.T) {
	s := backupFixture(t, false, []string{"code"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ok, err := s.ConsumeBackupCode(ctx, backupUser, "code", func(hash, candidate string) bool {
		cancel()
		return true
	}, time.Now())
	if ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled consumption: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ConsumeBackupCode(context.Background(), backupUser, "code", func(hash, candidate string) bool { return hash == candidate }, time.Now()); err != nil || !ok {
		t.Fatalf("cancellation consumed code: ok=%v err=%v", ok, err)
	}
}

func TestLegacyNormalizationPreservesConsumedHistoryAndDeduplicates(t *testing.T) {
	s := backupFixture(t, true, []string{"already-used", "remaining", "remaining"})
	oldUse := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	for _, used := range []any{oldUse, nil} {
		if _, err := s.DB.Exec(`INSERT INTO totp_backup_codes(user_id,code_hash,used_at,created_at) VALUES(?,?,?,?)`, backupUser, "already-used", used, "now"); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := s.DB.Exec(`INSERT INTO totp_backup_codes(user_id,code_hash,created_at) VALUES(?,?,?)`, backupUser, "remaining", "now"); err != nil {
			t.Fatal(err)
		}
	}
	verify := func(hash, candidate string) bool { return hash == candidate }
	// Production startup already runs pruning. It must retain the old used
	// marker while its JSON copy remains available for migration.
	if err := s.PruneEphemeral(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	for i, candidate := range []string{"already-used", "remaining", "remaining"} {
		if ok, err := s.ConsumeBackupCode(context.Background(), backupUser, candidate, verify, time.Now()); err != nil || ok != (i == 1) {
			t.Fatalf("historical duplicate %q use %d: ok=%v err=%v", candidate, i, ok, err)
		}
	}
	var total, used int
	if err := s.DB.QueryRow(`SELECT COUNT(*),COUNT(used_at) FROM totp_backup_codes WHERE user_id=?`, backupUser).Scan(&total, &used); err != nil || total != 2 || used != 2 {
		t.Fatalf("normalized history: total=%d used=%d err=%v", total, used, err)
	}
	var raw string
	if err := s.DB.QueryRow(`SELECT backup_codes FROM users WHERE id=?`, backupUser).Scan(&raw); err != nil || raw != "" {
		t.Fatalf("legacy copies remain: raw=%q err=%v", raw, err)
	}
	if err := s.PruneEphemeral(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ConsumeBackupCode(context.Background(), backupUser, "already-used", verify, time.Now()); err != nil || ok {
		t.Fatalf("pruning after migration resurrected a code: ok=%v err=%v", ok, err)
	}
}

func TestLegacyNormalizationFailureRollsBackOriginalCodes(t *testing.T) {
	s := backupFixture(t, true, []string{"code-one", "code-two"})
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_backup_insert BEFORE INSERT ON totp_backup_codes BEGIN SELECT RAISE(ABORT,'fixture insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ConsumeBackupCode(context.Background(), backupUser, "code-one", func(hash, candidate string) bool { return hash == candidate }, time.Now()); ok || err == nil {
		t.Fatalf("failed migration: ok=%v err=%v", ok, err)
	}
	var raw string
	if err := s.DB.QueryRow(`SELECT backup_codes FROM users WHERE id=?`, backupUser).Scan(&raw); err != nil || raw != `["code-one","code-two"]` {
		t.Fatalf("failed migration destroyed legacy codes: raw=%q err=%v", raw, err)
	}
}
