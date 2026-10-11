package web

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/diagnostics"
)

func TestVerboseHTTPRecordsRouteAndOutcomeWithoutCredentials(t *testing.T) {
	original := slog.Default()
	defer slog.SetDefault(original)
	for _, mode := range []string{"false", "true"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("VOXMAIL_VERBOSE", mode)
			var logs bytes.Buffer
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, diagnostics.HandlerOptions())))
			mux := http.NewServeMux()
			mux.HandleFunc("POST /fixture/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
			req := httptest.NewRequest("POST", "/fixture/private-path?token=private-query", strings.NewReader("password=private-body"))
			req.Header.Set("Authorization", "Bearer private-header")
			response := httptest.NewRecorder()
			withRequestID(mux).ServeHTTP(response, req)
			if response.Code != http.StatusForbidden || response.Header().Get("X-Request-ID") == "" {
				t.Fatal("request behavior changed")
			}
			if mode == "false" && logs.Len() != 0 {
				t.Fatalf("normal trace=%s", logs.String())
			}
			if mode == "true" {
				for _, want := range []string{`"status":403`, `"route":"POST /fixture/{id}"`, "duration_ms", response.Header().Get("X-Request-ID")} {
					if !strings.Contains(logs.String(), want) {
						t.Fatalf("missing %q in %s", want, logs.String())
					}
				}
			}
			for _, secret := range []string{"private-path", "private-query", "private-body", "private-header"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("secret leaked: %s", logs.String())
				}
			}
		})
	}
}
