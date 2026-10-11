package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/store"
)

type previewFunction func(context.Context, string, int) ([]byte, error)

func (f previewFunction) PreviewVoice(ctx context.Context, voice string, speed int) ([]byte, error) {
	return f(ctx, voice, speed)
}

func previewID(t *testing.T, responseBody []byte) string {
	t.Helper()
	var result struct {
		ID string `json:"job_id"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil || result.ID == "" {
		t.Fatalf("missing preview ID: %s", responseBody)
	}
	return result.ID
}

func TestPreviewJobReturnsBeforeSpeechAndEnforcesOwnership(t *testing.T) {
	srv, h := voiceWebFixture(t)
	srv.VoiceDir = ""
	release, entered := make(chan struct{}), make(chan struct{}, 1)
	srv.Preview = previewFunction(func(ctx context.Context, _ string, _ int) ([]byte, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return []byte("fixture WAV"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	response := voiceRequest(h, "user", "POST", "/api/v1/voices/preview", map[string]any{"voice": "en_US-hfc_male-medium", "speed": 3})
	if response.Code != http.StatusAccepted {
		t.Fatalf("start=%d %s", response.Code, response.Body.String())
	}
	id := previewID(t, response.Body.Bytes())
	path := "/api/v1/voices/previews/" + id
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("preview worker not started")
	}
	for _, suffix := range []string{"", "/audio"} {
		foreign := voiceRequest(h, "admin", "GET", path+suffix, nil)
		if foreign.Code != http.StatusNotFound {
			t.Fatalf("foreign %s=%d", suffix, foreign.Code)
		}
	}
	if foreign := voiceRequest(h, "admin", "POST", path+"/cancel", nil); foreign.Code != http.StatusNotFound {
		t.Fatal("foreign cancellation allowed")
	}
	pending := voiceRequest(h, "user", "GET", path, nil)
	if pending.Code != 200 || !strings.Contains(pending.Body.String(), `"status":"running"`) {
		t.Fatalf("pending=%s", pending.Body.String())
	}
	if pendingAudio := voiceRequest(h, "user", "GET", path+"/audio", nil); pendingAudio.Code != http.StatusConflict {
		t.Fatal("unfinished audio exposed")
	}
	if duplicate := voiceRequest(h, "user", "POST", "/api/v1/voices/preview", map[string]any{"voice": "en_US-hfc_male-medium"}); duplicate.Code != http.StatusConflict {
		t.Fatal("duplicate preview admitted")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		status := voiceRequest(h, "user", "GET", path, nil)
		if strings.Contains(status.Body.String(), `"status":"complete"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("preview did not complete: %s", status.Body.String())
		}
		time.Sleep(time.Millisecond)
	}
	audio := voiceRequest(h, "user", "GET", path+"/audio", nil)
	if audio.Code != 200 || audio.Header().Get("Content-Type") != "audio/wav" || audio.Header().Get("Cache-Control") != "no-store" || audio.Body.String() != "fixture WAV" {
		t.Fatalf("bad audio=%d %s", audio.Code, audio.Body.String())
	}
	srv.previewMu.Lock()
	srv.previewJobs[id].UpdatedAt = time.Now().Add(-previewLifetime - time.Second)
	srv.previewMu.Unlock()
	if expired := voiceRequest(h, "user", "GET", path, nil); expired.Code != http.StatusNotFound {
		t.Fatal("expired preview accessible")
	}
}

func TestPreviewCapacityAndShutdownAreBounded(t *testing.T) {
	srv, h := voiceWebFixture(t)
	srv.VoiceDir = ""
	entered := make(chan struct{}, maxPreviewJobs)
	srv.Preview = previewFunction(func(ctx context.Context, _ string, _ int) ([]byte, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	for i := 0; i <= maxPreviewJobs; i++ {
		id := fmt.Sprintf("preview-%d", i)
		if err := srv.Store.CreateUser(context.Background(), store.User{ID: id, Username: id, PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
			t.Fatal(err)
		}
		srv.Sessions.Put(id+"-token", Session{UserID: id, CSRF: id + "-csrf", Expires: time.Now().Add(time.Hour)})
		response := voiceRequest(h, id, "POST", "/api/v1/voices/preview", map[string]any{"voice": "en_US-hfc_male-medium"})
		want := http.StatusAccepted
		if i == maxPreviewJobs {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("admission %d=%d %s", i, response.Code, response.Body.String())
		}
	}
	for i := 0; i < maxPreviewJobs; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("worker not started")
		}
	}
	srv.Close()
	srv.previewMu.Lock()
	defer srv.previewMu.Unlock()
	if len(srv.previewJobs) != maxPreviewJobs {
		t.Fatal("preview records unbounded")
	}
	for _, job := range srv.previewJobs {
		if job.Status != "failed" || job.Failure.Category != "canceled" || len(job.Audio) != 0 {
			t.Fatalf("job escaped shutdown: %+v", job)
		}
	}
}

func TestPreviewFailuresAreSafeAndCancellationWorks(t *testing.T) {
	for _, mode := range []string{"failure", "oversized", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			srv, h := voiceWebFixture(t)
			srv.VoiceDir = ""
			srv.Preview = previewFunction(func(ctx context.Context, _ string, _ int) ([]byte, error) {
				switch mode {
				case "failure":
					return nil, fmt.Errorf("password=SensitiveProviderError")
				case "oversized":
					return make([]byte, maxPreviewAudio+1), nil
				default:
					<-ctx.Done()
					return nil, ctx.Err()
				}
			})
			response := voiceRequest(h, "user", "POST", "/api/v1/voices/preview", map[string]any{"voice": "en_US-hfc_male-medium"})
			if response.Code != 202 {
				t.Fatalf("start=%d", response.Code)
			}
			id := previewID(t, response.Body.Bytes())
			path := "/api/v1/voices/previews/" + id
			if mode == "cancel" {
				if cancel := voiceRequest(h, "user", "POST", path+"/cancel", nil); cancel.Code != 202 {
					t.Fatalf("cancel=%d", cancel.Code)
				}
			}
			deadline := time.Now().Add(time.Second)
			for {
				result := voiceRequest(h, "user", "GET", path, nil)
				if strings.Contains(result.Body.String(), "Sensitive") {
					t.Fatal("provider error leaked")
				}
				if strings.Contains(result.Body.String(), `"status":"failed"`) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("preview stayed active: %s", result.Body.String())
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestPreviewOutlivesHTTPConnectionWriteBudget(t *testing.T) {
	srv, h := voiceWebFixture(t)
	srv.VoiceDir = ""
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	srv.Preview = previewFunction(func(ctx context.Context, _ string, _ int) ([]byte, error) {
		entered <- ctx
		select {
		case <-release:
			return []byte("WAV"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	httpServer := httptest.NewUnstartedServer(h)
	httpServer.Config.WriteTimeout = 100 * time.Millisecond
	httpServer.Start()
	defer httpServer.Close()
	request, err := http.NewRequest("POST", httpServer.URL+"/api/v1/voices/preview", strings.NewReader(`{"voice":"en_US-hfc_male-medium"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: "voxmail_session", Value: "user-token"})
	request.Header.Set("X-CSRF-Token", "user-csrf")
	request.Header.Set("Content-Type", "application/json")
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("response=%d %s", response.StatusCode, data)
	}
	id := previewID(t, data)
	var worker context.Context
	select {
	case worker = <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	select {
	case <-worker.Done():
		t.Fatal("preview inherited HTTP lifetime")
	case <-time.After(3 * httpServer.Config.WriteTimeout):
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		req, _ := http.NewRequest("GET", httpServer.URL+"/api/v1/voices/previews/"+id, nil)
		req.AddCookie(&http.Cookie{Name: "voxmail_session", Value: "user-token"})
		result, err := httpServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(result.Body)
		result.Body.Close()
		if strings.Contains(string(body), `"status":"complete"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not complete beyond HTTP budget: %s", body)
		}
		time.Sleep(time.Millisecond)
	}
}
