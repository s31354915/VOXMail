package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestVoiceInstallJobsPersistRestartStateAndRetention(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "voice-jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateBootstrapUser(ctx, User{ID: "voice-owner", Username: "voice-owner", PasswordHash: "p", PINHash: "p", Role: "admin", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	created := "2026-09-29T10:00:00Z"
	if err := db.CreateVoiceInstallJob(ctx, VoiceInstallJob{ID: "voice-job", UserID: "voice-owner", Voice: "voice-a", CreatedAt: created, UpdatedAt: created}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateVoiceInstallJob(ctx, VoiceInstallJob{ID: "voice-job", Stage: "installing", Status: "running", Progress: 10, UpdatedAt: "2026-09-29T10:01:00Z"}); err != nil {
		t.Fatal(err)
	}
	job, err := db.VoiceInstallJob(ctx, "voice-job")
	if err != nil {
		t.Fatal(err)
	}
	if job.UserID != "voice-owner" || job.Voice != "voice-a" || job.Status != "running" || job.Progress != 10 {
		t.Fatalf("persisted job=%+v", job)
	}
	if err := db.MarkInterruptedVoiceInstallJobs(ctx, time.Date(2026, 9, 29, 10, 2, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	job, err = db.VoiceInstallJob(ctx, "voice-job")
	if err != nil {
		t.Fatal(err)
	}
	if job.Stage != "failed" || job.Status != "failed" || job.Progress != 100 || job.Error == "" {
		t.Fatalf("interrupted job=%+v", job)
	}
	if err := db.PurgeVoiceInstallJobs(ctx, time.Date(2026, 9, 29, 10, 3, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.VoiceInstallJob(ctx, "voice-job"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("purged job error=%v, want sql.ErrNoRows", err)
	}
}

func TestVoiceInstallJobUpdateCannotRecreateDeletedHistory(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "voice-jobs-missing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpdateVoiceInstallJob(context.Background(), VoiceInstallJob{ID: "missing", Stage: "failed", Status: "failed", Progress: 100}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing update error=%v, want sql.ErrNoRows", err)
	}
}
