package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/speech"
	"github.com/voxmail/voxmail/internal/store"
)

type fixtureVoiceInstaller struct {
	source string
	client *http.Client
	seen   chan speech.VoiceSpec
}

func (i *fixtureVoiceInstaller) Install(ctx context.Context, root string, spec speech.VoiceSpec) error {
	i.seen <- spec
	spec.ModelURL = i.source + "/voice.onnx"
	spec.ConfigURL = spec.ModelURL + ".json"
	return (speech.AdministratorVoiceInstaller{Client: i.client}).Install(ctx, root, spec)
}

type installedVoiceActivator struct {
	root      string
	activated chan string
}

func (v *installedVoiceActivator) ActivateVoice(_ context.Context, name string, _, _ int) error {
	if !speech.IsInstalledVoice(v.root, name) {
		return fmt.Errorf("activation did not receive an installed pair")
	}
	v.activated <- name
	return nil
}

func voiceWebFixture(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "voice-install.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, role := range []string{"admin", "user"} {
		if err := db.CreateUser(context.Background(), store.User{ID: role, Username: role, PasswordHash: "p", PINHash: "p", Role: role, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	srv := &Server{Store: db, VoiceDir: t.TempDir(), Sessions: NewSessionStore()}
	h := srv.Handler()
	for _, role := range []string{"admin", "user"} {
		if !srv.Sessions.Put(role+"-token", Session{UserID: role, CSRF: role + "-csrf", Expires: time.Now().Add(time.Hour)}) {
			t.Fatal("session fixture failed")
		}
	}
	t.Cleanup(srv.Close)
	return srv, h
}

func voiceRequest(h http.Handler, role, method, path string, body any) *httptest.ResponseRecorder {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	if role != "" {
		r.AddCookie(&http.Cookie{Name: "voxmail_session", Value: role + "-token"})
		r.Header.Set("X-CSRF-Token", role+"-csrf")
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func waitVoiceStatus(t *testing.T, h http.Handler, id, status string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response := voiceRequest(h, "admin", http.MethodGet, "/api/v1/voices/jobs/"+id, nil)
		var job struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if job.Status == status {
			return
		}
		if job.Status == "failed" {
			t.Fatalf("voice job failed: %s", job.Error)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("voice job did not reach", status)
}

func TestAdministratorCanInstallUnlistedPiperVoiceWhileUsersCannot(t *testing.T) {
	for _, input := range []string{"en_US-lessac-low", "https://models.example/custom.onnx?download=true"} {
		t.Run(input, func(t *testing.T) {
			srv, h := voiceWebFixture(t)
			release := make(chan struct{})
			entered := make(chan struct{}, 1)
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, ".json") {
					select {
					case entered <- struct{}{}:
					default:
					}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					_, _ = w.Write([]byte(`{"audio":{"sample_rate":22050}}`))
				} else {
					_, _ = w.Write([]byte("model fixture"))
				}
			}))
			t.Cleanup(source.Close)
			installer := &fixtureVoiceInstaller{source: source.URL, client: source.Client(), seen: make(chan speech.VoiceSpec, 2)}
			srv.VoiceInstaller = installer
			activator := &installedVoiceActivator{root: srv.VoiceDir, activated: make(chan string, 1)}
			srv.Voice = activator
			body := map[string]any{"voice": input, "allow_untrusted": true}
			for _, role := range []string{"user", ""} {
				response := voiceRequest(h, role, http.MethodPost, "/api/v1/voices/install", body)
				want := http.StatusForbidden
				if role == "" {
					want = http.StatusUnauthorized
				}
				if response.Code != want {
					t.Fatalf("%q install=%d want=%d", role, response.Code, want)
				}
			}
			select {
			case <-installer.seen:
				t.Fatal("denied request reached the installer")
			default:
			}
			delete(body, "allow_untrusted") // Unknown client override flags are not part of the administrator API.
			response := voiceRequest(h, "admin", http.MethodPost, "/api/v1/voices/install", body)
			if response.Code != http.StatusAccepted {
				t.Fatalf("administrator unlisted install=%d %s", response.Code, response.Body.String())
			}
			var accepted struct {
				ID string `json:"job_id"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
				t.Fatal(err)
			}
			var resolved speech.VoiceSpec
			select {
			case resolved = <-installer.seen:
			case <-time.After(2 * time.Second):
				t.Fatal("administrator install did not begin")
			}
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("real sidecar download did not begin")
			}
			if _, trusted := speech.FindTrustedVoice(resolved.Voice); trusted {
				t.Fatal("fixture unexpectedly in bundled catalog")
			}
			if input == "en_US-lessac-low" && !strings.Contains(resolved.ModelURL, "/en/en_US/lessac/low/") {
				t.Fatal("named voice not resolved through Piper repository", resolved)
			}
			if response := voiceRequest(h, "admin", http.MethodPost, "/api/v1/voices/install", body); response.Code != http.StatusConflict {
				t.Fatalf("concurrent voice install=%d want=409", response.Code)
			}
			if response := voiceRequest(h, "user", http.MethodPost, "/api/v1/voices/install", body); response.Code != http.StatusForbidden {
				t.Fatal("busy worker bypassed role restriction", response.Code)
			}
			var jobs int
			if err := srv.Store.DB.QueryRow(`SELECT COUNT(*) FROM voice_install_jobs`).Scan(&jobs); err != nil || jobs != 1 {
				t.Fatalf("denied/conflicting requests created jobs: %d %v", jobs, err)
			}
			if speech.IsInstalledVoice(srv.VoiceDir, resolved.Voice) {
				t.Fatal("partial model became installed before metadata")
			}
			unblock()
			waitVoiceStatus(t, h, accepted.ID, "complete")
			select {
			case name := <-activator.activated:
				if name != resolved.Voice {
					t.Fatal("activation received URL instead of canonical name", name)
				}
			default:
				t.Fatal("installed custom voice was not activated")
			}
			if !speech.IsInstalledVoice(srv.VoiceDir, resolved.Voice) {
				t.Fatal("unlisted voice not installed")
			}
			if response := voiceRequest(h, "user", http.MethodGet, "/api/v1/voices", nil); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), resolved.Voice) || strings.Contains(response.Body.String(), `"downloadable":true`) {
				t.Fatal("user voice list is not installed-only", response.Code, response.Body.String())
			}
			if response := voiceRequest(h, "user", http.MethodPut, "/api/v1/settings", map[string]any{"tts_voice": resolved.Voice, "menu_speed": 3, "email_speed": 2}); response.Code != http.StatusOK {
				t.Fatal("ordinary user cannot select installed custom voice", response.Code, response.Body.String())
			}
		})
	}
}

func TestAdministratorCanCancelAndRecoverVoiceInstall(t *testing.T) {
	srv, h := voiceWebFixture(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".json") {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
				_, _ = w.Write([]byte(`{"audio":{"sample_rate":22050}}`))
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write([]byte("model fixture"))
	}))
	t.Cleanup(source.Close)
	srv.VoiceInstaller = &fixtureVoiceInstaller{source: source.URL, client: source.Client(), seen: make(chan speech.VoiceSpec, 1)}
	response := voiceRequest(h, "admin", http.MethodPost, "/api/v1/voices/install", map[string]string{"voice": "en_US-lessac-low"})
	if response.Code != http.StatusAccepted {
		t.Fatal("install was not accepted", response.Code, response.Body.String())
	}
	var accepted struct {
		ID string `json:"job_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("voice installer did not reach the blocking download")
	}

	response = voiceRequest(h, "user", http.MethodGet, "/api/v1/voices/jobs/active", nil)
	if response.Code != http.StatusForbidden {
		t.Fatal("ordinary user could inspect an administrator job", response.Code)
	}
	response = voiceRequest(h, "admin", http.MethodGet, "/api/v1/voices/jobs/active", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), accepted.ID) {
		t.Fatal("active job could not be recovered after page reload", response.Code, response.Body.String())
	}
	for _, role := range []string{"user", ""} {
		response = voiceRequest(h, role, http.MethodPost, "/api/v1/voices/jobs/"+accepted.ID+"/cancel", nil)
		want := http.StatusForbidden
		if role == "" {
			want = http.StatusUnauthorized
		}
		if response.Code != want {
			t.Fatalf("%q cancel=%d, want %d", role, response.Code, want)
		}
	}
	response = voiceRequest(h, "admin", http.MethodPost, "/api/v1/voices/jobs/"+accepted.ID+"/cancel", nil)
	if response.Code != http.StatusAccepted {
		t.Fatal("administrator cancel was not accepted", response.Code, response.Body.String())
	}
	waitVoiceStatus(t, h, accepted.ID, "failed")
	response = voiceRequest(h, "admin", http.MethodGet, "/api/v1/voices/jobs/"+accepted.ID, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "voice installation canceled") {
		t.Fatal("cancellation outcome was not persisted", response.Code, response.Body.String())
	}
	response = voiceRequest(h, "admin", http.MethodGet, "/api/v1/voices/jobs/active", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"idle"`) {
		t.Fatal("terminal voice job still appeared active", response.Code, response.Body.String())
	}
	response = voiceRequest(h, "admin", http.MethodPost, "/api/v1/voices/jobs/"+accepted.ID+"/cancel", nil)
	if response.Code != http.StatusConflict {
		t.Fatal("completed cancellation was not rejected", response.Code, response.Body.String())
	}
}

func TestOfflineModeRejectsAdministratorInstallationBeforeWork(t *testing.T) {
	srv, h := voiceWebFixture(t)
	srv.OfflineMode = true
	installer := &fixtureVoiceInstaller{seen: make(chan speech.VoiceSpec, 1)}
	srv.VoiceInstaller = installer
	response := voiceRequest(h, "admin", http.MethodPost, "/api/v1/voices/install", map[string]string{"voice": "en_US-lessac-low"})
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "offline mode") {
		t.Fatal("offline installation was admitted", response.Code, response.Body.String())
	}
	select {
	case <-installer.seen:
		t.Fatal("offline request reached model download")
	default:
	}
	var jobs int
	if err := srv.Store.DB.QueryRow(`SELECT COUNT(*) FROM voice_install_jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatal("offline request created a job", jobs, err)
	}
}

type failingVoiceInstaller struct{ err error }

func (i failingVoiceInstaller) Install(context.Context, string, speech.VoiceSpec) error {
	return i.err
}

func TestAdministratorVoiceFailureReportsReasonWithoutSourceSecrets(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		err          error
	}{
		{"http", "voice download returned HTTP 404", &speech.VoiceInstallError{Reason: "voice download returned HTTP 404"}},
		{"network", "voice download failed", &speech.VoiceInstallError{Reason: "voice download failed", Cause: errors.New("Get https://models.example/custom.onnx?token=source-secret: connection failed")}},
		{"piper", "Piper could not load the downloaded voice", &speech.VoiceInstallError{Reason: "Piper could not load the downloaded voice", Cause: errors.New("invalid model at /private/staged/model")}},
		{"cancel", "voice installation canceled", &speech.VoiceInstallError{Reason: "voice download failed", Cause: context.Canceled}},
		{"timeout", "voice installation timed out", &speech.VoiceInstallError{Reason: "voice download failed", Cause: context.DeadlineExceeded}},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, h := voiceWebFixture(t)
			var log bytes.Buffer
			srv.Log = slog.New(slog.NewJSONHandler(&log, nil))
			srv.VoiceInstaller = failingVoiceInstaller{err: test.err}
			response := voiceRequest(h, "admin", http.MethodPost, "/api/v1/voices/install", map[string]string{"voice": "https://models.example/custom.onnx?token=source-secret"})
			if response.Code != http.StatusAccepted {
				t.Fatal(response.Code, response.Body.String())
			}
			var accepted struct {
				ID string `json:"job_id"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
				t.Fatal(err)
			}
			waitVoiceStatus(t, h, accepted.ID, "failed")
			response = voiceRequest(h, "admin", http.MethodGet, "/api/v1/voices/jobs/"+accepted.ID, nil)
			srv.Close() // Join the worker before inspecting its log buffer.
			var job struct {
				Voice string `json:"voice"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil || job.Error != test.reason || job.Voice != "custom" {
				t.Fatalf("public failure=%s: %v", response.Body.String(), err)
			}
			persisted, err := srv.Store.VoiceInstallJob(context.Background(), accepted.ID)
			if err != nil || persisted.Error != test.reason {
				t.Fatalf("durable failure=%+v: %v", persisted, err)
			}
			combined := response.Body.String() + log.String() + persisted.Error
			for _, private := range []string{"source-secret", "models.example", "/private/staged"} {
				if strings.Contains(combined, private) {
					t.Fatal("failure disclosed source or process details", private)
				}
			}
			if !strings.Contains(log.String(), test.reason) {
				t.Fatal("operator log lost the failure reason")
			}
		})
	}
}
