package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/store"
)

func TestVoiceInstallStatusSurvivesHandlerRestartAndRespectsOwnership(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "voice-status.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateBootstrapUser(ctx, store.User{ID: "job-admin", Username: "job-admin", PasswordHash: "p", PINHash: "p", Role: "admin", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(ctx, store.User{ID: "job-user", Username: "job-user", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, job := range []store.VoiceInstallJob{
		{ID: "admin-job", UserID: "job-admin", Voice: "voice-a", Stage: "running", Status: "running", Progress: 35, UpdatedAt: "2026-09-29T10:00:00Z"},
		{ID: "user-job", UserID: "job-user", Voice: "voice-b", Stage: "running", Status: "running", Progress: 35, UpdatedAt: "2026-09-29T10:00:00Z"},
	} {
		if err := db.CreateVoiceInstallJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	srv := &Server{Store: db, Sessions: NewSessionStore()}
	h := srv.Handler() // startup recovery marks both in-flight rows interrupted
	srv.Sessions.Put("admin-token", Session{UserID: "job-admin", CSRF: "admin-csrf", Expires: time.Now().Add(time.Hour)})
	srv.Sessions.Put("user-token", Session{UserID: "job-user", CSRF: "user-csrf", Expires: time.Now().Add(time.Hour)})

	request := httptest.NewRequest(http.MethodGet, "/api/v1/voices/jobs/user-job", nil)
	request.AddCookie(&http.Cookie{Name: "voxmail_session", Value: "admin-token"})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("admin persistent status=%d, want 200", recorder.Code)
	}
	var status map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status["status"] != "failed" || status["stage"] != "failed" || status["progress"] != float64(100) {
		t.Fatalf("recovered job status=%v", status)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/voices/jobs/admin-job", nil)
	request.AddCookie(&http.Cookie{Name: "voxmail_session", Value: "user-token"})
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("ordinary user read admin job status=%d, want 404", recorder.Code)
	}
}
