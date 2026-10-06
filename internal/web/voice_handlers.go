package web

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/lifecycle"
	"github.com/voxmail/voxmail/internal/speech"
	"github.com/voxmail/voxmail/internal/store"
)

func (s *Server) voices(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	result := make([]map[string]any, 0)
	installed := speech.InstalledVoiceNames(s.VoiceDir)
	if u.Role != "admin" {
		for _, name := range installed {
			result = append(result, map[string]any{"voice": name, "installed": true, "downloadable": false})
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	seen := make(map[string]bool, len(installed))
	for _, name := range installed {
		seen[name] = true
	}
	for _, voice := range speech.TrustedVoiceCatalog() {
		result = append(result, map[string]any{"voice": voice.Voice, "installed": seen[voice.Voice], "downloadable": true})
		seen[voice.Voice] = true
	}
	for _, name := range installed {
		if _, trusted := speech.FindTrustedVoice(name); trusted {
			continue
		}
		result = append(result, map[string]any{"voice": name, "installed": true, "downloadable": false})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) installVoice(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if s.VoiceDir == "" {
		writeError(w, http.StatusServiceUnavailable, "voice directory is not configured")
		return
	}
	var body struct {
		Voice string `json:"voice"`
	}
	if !decode(w, r, &body) {
		return
	}
	if _, ok := speech.FindTrustedVoice(body.Voice); !ok {
		writeError(w, http.StatusBadRequest, "voice model is not in the trusted catalog")
		return
	}
	job := &voiceInstallJob{ID: newID(), UserID: u.ID, Voice: body.Voice, Stage: "queued", Status: "queued", Progress: 0, UpdatedAt: time.Now().UTC()}
	select {
	case s.voiceSem <- struct{}{}:
	default:
		writeError(w, http.StatusConflict, "another voice installation is already running")
		return
	}
	if err := s.Store.CreateVoiceInstallJob(r.Context(), store.VoiceInstallJob{
		ID: job.ID, UserID: job.UserID, Voice: job.Voice, Stage: job.Stage,
		Status: job.Status, Progress: job.Progress, UpdatedAt: job.UpdatedAt.Format(time.RFC3339Nano),
	}); err != nil {
		<-s.voiceSem
		serverError(w, err)
		return
	}
	s.voiceMu.Lock()
	s.cleanupVoiceJobsLocked(time.Now().UTC())
	if s.voiceJobs == nil {
		s.voiceJobs = make(map[string]*voiceInstallJob)
	}
	s.voiceJobs[job.ID] = job
	s.voiceMu.Unlock()
	menuSpeed, emailSpeed := 3, 2
	if settings, err := s.Store.SpeechSettings(r.Context(), u.ID); err == nil {
		menuSpeed, emailSpeed = settings.MenuSpeed, settings.EmailSpeed
	}
	s.voiceAdmissionMu.Lock()
	if s.voiceClosing {
		s.voiceAdmissionMu.Unlock()
		s.updateVoiceJob(job.ID, "failed", "failed", 100, context.Canceled)
		<-s.voiceSem
		writeError(w, http.StatusServiceUnavailable, "server is shutting down")
		return
	}
	s.voiceWG.Add(1)
	s.voiceAdmissionMu.Unlock()
	go func() {
		defer s.voiceWG.Done()
		defer func() { <-s.voiceSem }()
		s.runVoiceInstall(job.ID, u.ID, body.Voice, menuSpeed, emailSpeed)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": job.ID, "status": "queued"})
}

func (s *Server) updateVoiceJob(id, stage, status string, progress int, jobErr error) {
	s.voiceMu.Lock()
	job := s.voiceJobs[id]
	if job == nil {
		s.voiceMu.Unlock()
		return
	}
	job.Stage, job.Status, job.Progress, job.UpdatedAt = stage, status, progress, time.Now().UTC()
	if jobErr != nil {
		job.Error = "voice installation failed"
	}
	snapshot := store.VoiceInstallJob{
		ID: job.ID, UserID: job.UserID, Voice: job.Voice, Stage: job.Stage,
		Status: job.Status, Progress: job.Progress, Error: job.Error,
		UpdatedAt: job.UpdatedAt.Format(time.RFC3339Nano),
	}
	s.voiceMu.Unlock()
	if s.Store != nil {
		if err := s.Store.UpdateVoiceInstallJob(context.Background(), snapshot); err != nil && s.Log != nil {
			s.Log.Warn("voice job status persistence failed", "job_id", id, "error", err)
		}
	}
}

func (s *Server) cleanupVoiceJobsLocked(now time.Time) {
	for id, job := range s.voiceJobs {
		if (job.Status == "complete" || job.Status == "failed") && now.Sub(job.UpdatedAt) > time.Hour {
			delete(s.voiceJobs, id)
		}
	}
	if s.Store != nil {
		if err := s.Store.PurgeVoiceInstallJobs(context.Background(), now.Add(-time.Hour)); err != nil && s.Log != nil {
			s.Log.Warn("voice job cleanup failed", "error", err)
		}
	}
}

// BeginShutdown stops admission of new voice installation jobs and cancels
// jobs already running. It is separate from Close so the HTTP server can stop
// accepting requests after cancellation without racing voiceWG.Add.
func (s *Server) BeginShutdown() {
	if s == nil {
		return
	}
	s.voiceAdmissionMu.Lock()
	s.voiceClosing = true
	s.voiceAdmissionMu.Unlock()
	if s.voiceCancel != nil {
		s.voiceCancel()
	}
}

// Close cancels long-running voice operations during application shutdown and
// waits until every admitted job has finished its final database update.
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.BeginShutdown()
	s.voiceWG.Wait()
}

func (s *Server) runVoiceInstall(id, userID, voice string, menuSpeed, emailSpeed int) {
	base := s.voiceCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, 30*time.Minute)
	defer cancel()
	s.updateVoiceJob(id, "installing", "running", 10, nil)
	var err error
	if s.Voice != nil {
		s.updateVoiceJob(id, "activating", "running", 35, nil)
		err = s.Voice.ActivateVoice(ctx, voice, menuSpeed, emailSpeed)
	} else {
		err = speech.InstallVoice(ctx, s.VoiceDir, voice)
	}
	if err != nil {
		s.updateVoiceJob(id, "failed", "failed", 100, err)
		return
	}
	s.updateVoiceJob(id, "complete", "complete", 100, nil)
	cleanupCtx, cleanupCancel := lifecycle.CleanupContext(base)
	_ = s.Store.Audit(cleanupCtx, userID, "voice_installed", voice)
	cleanupCancel()
}

func (s *Server) voiceInstallStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var copy voiceInstallJob
	found := false
	s.voiceMu.Lock()
	job := s.voiceJobs[id]
	if job != nil {
		copy = *job
		found = true
	}
	s.voiceMu.Unlock()
	if !found {
		persistent, err := s.Store.VoiceInstallJob(r.Context(), id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, "voice installation job not found")
				return
			}
			serverError(w, err)
			return
		}
		copy = voiceInstallJob{ID: persistent.ID, UserID: persistent.UserID, Voice: persistent.Voice, Stage: persistent.Stage, Status: persistent.Status, Progress: persistent.Progress, Error: persistent.Error}
		if parsed, err := time.Parse(time.RFC3339Nano, persistent.UpdatedAt); err == nil {
			copy.UpdatedAt = parsed
		}
	}
	if copy.UserID != u.ID && u.Role != "admin" {
		writeError(w, http.StatusNotFound, "voice installation job not found")
		return
	}
	result := map[string]any{"job_id": copy.ID, "voice": copy.Voice, "stage": copy.Stage, "status": copy.Status, "progress": copy.Progress, "updated_at": copy.UpdatedAt}
	if copy.Error != "" {
		result["error"] = copy.Error
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) voiceAvailable(name string) bool {
	if s.VoiceDir == "" {
		_, ok := speech.FindTrustedVoice(name)
		return ok
	}
	return speech.IsInstalledVoice(s.VoiceDir, name)
}

func (s *Server) previewVoice(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, true); !ok {
		return
	}
	if s.Preview == nil {
		writeError(w, http.StatusServiceUnavailable, "voice preview is unavailable")
		return
	}
	var body struct {
		Voice string `json:"voice"`
		Speed int    `json:"speed"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Voice = strings.TrimSpace(body.Voice)
	if !s.voiceAvailable(body.Voice) {
		writeError(w, http.StatusBadRequest, "voice model is not installed")
		return
	}
	if body.Speed < 1 || body.Speed > 5 {
		body.Speed = 3
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	audio, err := s.Preview.PreviewVoice(ctx, body.Voice, body.Speed)
	if err != nil {
		writeError(w, http.StatusBadGateway, "voice preview failed")
		return
	}
	if len(audio) == 0 || len(audio) > 16<<20 {
		writeError(w, http.StatusBadGateway, "voice preview output is invalid")
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(audio)
}
