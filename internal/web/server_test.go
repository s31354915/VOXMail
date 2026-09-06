package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

func TestSetupAndCSRFProtectedContact(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, _ := secret.New("test-key-with-more-than-32-characters-123456")
	h := (&Server{Store: db, Secrets: box}).Handler()
	body := bytes.NewBufferString(`{"username":"admin","password":"a-strong-password","pin":"1234"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/setup", body)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	response := recorder.Result()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("setup status %d", response.StatusCode)
	}
	csrf := response.Header.Get("X-CSRF-Token")
	if csrf == "" {
		t.Fatal("missing csrf token")
	}
	payload, _ := json.Marshal(map[string]string{"name": "Ada", "email": "ada@example.com"})
	request = httptest.NewRequest(http.MethodPost, "/api/v1/contacts", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range response.Cookies() {
		request.AddCookie(cookie)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	response = recorder.Result()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("contact status %d", response.StatusCode)
	}
}
