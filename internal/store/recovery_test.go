package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoveryTokenLifecycleIsAtomicAndSingleUse(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, user := range []User{
		{ID: "recovery-user", Username: "recovery-user", PasswordHash: "old", PINHash: "pin", Role: "user", Enabled: true},
		{ID: "disabled-recovery-user", Username: "disabled-recovery-user", PasswordHash: "old", PINHash: "pin", Role: "user", Enabled: false},
	} {
		if err := db.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	id1, placeholder1, reserved, err := db.ReserveRecoveryToken(ctx, "recovery-user", "user@example.com", "reset_password", now.Add(10*time.Minute), now, 15*time.Minute, 5)
	if err != nil || !reserved || id1 == 0 || placeholder1 == "" {
		t.Fatalf("first reservation id=%d placeholder=%q reserved=%v err=%v", id1, placeholder1, reserved, err)
	}
	if err := db.SetRecoveryTokenHash(ctx, id1, placeholder1, "hash-one"); err != nil {
		t.Fatal(err)
	}
	id2, placeholder2, reserved, err := db.ReserveRecoveryToken(ctx, "recovery-user", "user@example.com", "reset_password", now.Add(10*time.Minute), now.Add(time.Minute), 15*time.Minute, 5)
	if err != nil || !reserved {
		t.Fatalf("second reservation id=%d reserved=%v err=%v", id2, reserved, err)
	}
	if err := db.SetRecoveryTokenHash(ctx, id1, placeholder1, "late-hash"); !errors.Is(err, ErrRecoveryReservationSuperseded) {
		t.Fatalf("superseded reservation error=%v", err)
	}
	if err := db.SetRecoveryTokenHash(ctx, id2, placeholder2, "hash-two"); err != nil {
		t.Fatal(err)
	}
	candidate, ok, err := db.TakeRecoveryCandidate(ctx, "USER@example.com", "reset_password", nil, now.Add(2*time.Minute), 5)
	if err != nil || !ok || candidate.ID != id2 || candidate.TokenHash != "hash-two" {
		t.Fatalf("candidate=%+v ok=%v err=%v", candidate, ok, err)
	}
	result, err := db.RedeemRecoveryToken(ctx, candidate.ID, candidate.UserID, "new-password-hash", now.Add(3*time.Minute))
	if err != nil || !result.Redeemed || result.DisabledUser {
		t.Fatalf("redeem result=%+v err=%v", result, err)
	}
	if reused, ok, err := db.TakeRecoveryCandidate(ctx, "user@example.com", "reset_password", nil, now.Add(4*time.Minute), 5); err != nil || ok || reused.ID != 0 {
		t.Fatalf("reused candidate=%+v ok=%v err=%v", reused, ok, err)
	}
	updated, err := db.UserByID(ctx, "recovery-user")
	if err != nil {
		t.Fatal(err)
	}
	if updated.PasswordHash != "new-password-hash" {
		t.Fatalf("password hash=%q, want new hash", updated.PasswordHash)
	}

	disabledID, disabledPlaceholder, reserved, err := db.ReserveRecoveryToken(ctx, "disabled-recovery-user", "disabled@example.com", "bypass_2fa", now.Add(10*time.Minute), now, 15*time.Minute, 5)
	if err != nil || !reserved {
		t.Fatalf("disabled reservation id=%d reserved=%v err=%v", disabledID, reserved, err)
	}
	if err := db.SetRecoveryTokenHash(ctx, disabledID, disabledPlaceholder, "disabled-hash"); err != nil {
		t.Fatal(err)
	}
	disabledCandidate, ok, err := db.TakeRecoveryCandidate(ctx, "disabled@example.com", "bypass_2fa", nil, now.Add(time.Minute), 5)
	if err != nil || !ok {
		t.Fatalf("disabled candidate=%+v ok=%v err=%v", disabledCandidate, ok, err)
	}
	result, err = db.RedeemRecoveryToken(ctx, disabledCandidate.ID, disabledCandidate.UserID, "", now.Add(2*time.Minute))
	if err != nil || result.Redeemed || !result.DisabledUser {
		t.Fatalf("disabled redeem result=%+v err=%v", result, err)
	}
}

func TestRecoveryTokenLimitAndExpiry(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "recovery-limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateUser(ctx, User{ID: "limit-user", Username: "limit-user", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 13, 0, 0, 0, time.UTC)
	if _, _, reserved, err := db.ReserveRecoveryToken(ctx, "limit-user", "limit@example.com", "reset_password", now.Add(time.Minute), now, time.Hour, 1); err != nil || !reserved {
		t.Fatalf("initial limited reservation reserved=%v err=%v", reserved, err)
	}
	if _, _, reserved, err := db.ReserveRecoveryToken(ctx, "limit-user", "limit@example.com", "reset_password", now.Add(2*time.Minute), now.Add(time.Minute), time.Hour, 1); err != nil || reserved {
		t.Fatalf("limited second reservation reserved=%v err=%v", reserved, err)
	}
	id, placeholder, reserved, err := db.ReserveRecoveryToken(ctx, "limit-user", "expired@example.com", "bypass_2fa", now.Add(-time.Minute), now.Add(2*time.Hour), time.Hour, 5)
	if err != nil || !reserved {
		t.Fatalf("expired reservation id=%d reserved=%v err=%v", id, reserved, err)
	}
	if err := db.SetRecoveryTokenHash(ctx, id, placeholder, "expired-hash"); err != nil {
		t.Fatal(err)
	}
	if candidate, ok, err := db.TakeRecoveryCandidate(ctx, "expired@example.com", "bypass_2fa", nil, now.Add(2*time.Hour), 5); err != nil || ok || candidate.ID != 0 {
		t.Fatalf("expired candidate=%+v ok=%v err=%v", candidate, ok, err)
	}
}
