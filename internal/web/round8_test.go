package web

import (
	"net/http"
	"testing"
	"time"
)

func TestOperationalFailureDoesNotMisreadNumericMailErrorsAsSIP(t *testing.T) {
	for _, signal := range []string{"554 Transaction failed", "454 Temporary failure", "NO Byepass 250 message accepted"} {
		failure := operationalFailure(signal)
		if failure == nil {
			t.Fatalf("expected a classification for %q", signal)
		}
		if failure.Diagnostic != "" {
			t.Fatalf("mail error mislabeled as SIP: %+v", failure)
		}
	}
}

func TestSipRegistrationFailureAppliesNumericResponses(t *testing.T) {
	failure := sipRegistrationFailure("401 Unauthorized")
	if failure == nil || failure.Category != "authentication" || failure.Diagnostic != "SIP response 401" {
		t.Fatalf("SIP response not classified: %+v", failure)
	}
	if failure := sipRegistrationFailure(""); failure != nil {
		t.Fatalf("empty message should be unclassified: %+v", failure)
	}
}

func TestSessionStoreSlidesExpiryOnActiveUse(t *testing.T) {
	s := NewSessionStore()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if !s.Put("token", Session{UserID: "u", Expires: now.Add(sessionLifetime)}) {
		t.Fatal("put failed")
	}
	if _, renewed, ok := s.GetRenew("token"); !ok || renewed {
		t.Fatalf("young session should not renew: renewed=%v ok=%v", renewed, ok)
	}
	now = now.Add(7 * time.Hour)
	session, renewed, ok := s.GetRenew("token")
	if !ok || !renewed {
		t.Fatalf("old session should renew: renewed=%v ok=%v", renewed, ok)
	}
	if !session.Expires.Equal(now.Add(sessionLifetime)) {
		t.Fatalf("renewed expiry wrong: %v", session.Expires)
	}
	if _, ok := s.Get("token"); !ok {
		t.Fatal("renewed session vanished")
	}
}

func TestContactEmailIsValidated(t *testing.T) {
	_, h := voiceWebFixture(t)
	if response := voiceRequest(h, "user", http.MethodPost, "/api/v1/contacts", map[string]string{"name": "Bad", "email": "not-an-email"}); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid contact email accepted: %d %s", response.Code, response.Body.String())
	}
	if response := voiceRequest(h, "user", http.MethodPost, "/api/v1/contacts", map[string]string{"name": "Good", "email": "good@example.test"}); response.Code != http.StatusCreated {
		t.Fatalf("valid contact email rejected: %d %s", response.Code, response.Body.String())
	}
}
