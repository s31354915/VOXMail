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
	if !s.requireOnline(w, "voice installation") {
		return
	}
	if s.VoiceDir == "" {
		writeError(w, http.StatusServiceUnavailable, "voice directory is not configured")
		return
	}
	var body speech.AdministratorVoiceRequest
	if !decode(w, r, &body) {
		return
	}
	voice, err := speech.ResolveAdministratorVoice(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job := &voiceInstallJob{ID: newID(), UserID: u.ID, Voice: voice.Voice, Stage: "queued", Status: "queued", Progress: 0, UpdatedAt: time.Now().UTC()}
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
	if s.voiceJobCancel == nil {
		s.voiceJobCancel = make(map[string]context.CancelFunc)
	}
	s.voiceJobs[job.ID] = job
	s.voiceMu.Unlock()
	menuSpeed, emailSpeed := 3, 2
	if settings, err := s.Store.SpeechSettings(r.Context(), u.ID); err == nil {
		menuSpeed, emailSpeed = settings.MenuSpeed, settings.EmailSpeed
	}
	s.admissionMu.Lock()
	if s.closing {
		s.admissionMu.Unlock()
		s.updateVoiceJob(job.ID, "failed", "failed", 100, context.Canceled)
		<-s.voiceSem
		writeError(w, http.StatusServiceUnavailable, "server is shutting down")
		return
	}
	s.voiceWG.Add(1)
	s.admissionMu.Unlock()
	base := s.voiceCtx
	if base == nil {
		base = context.Background()
	}
	jobCtx, jobCancel := context.WithTimeout(base, 30*time.Minute)
	s.voiceMu.Lock()
	s.voiceJobCancel[job.ID] = jobCancel
	s.voiceMu.Unlock()
	go func() {
		defer s.voiceWG.Done()
		defer func() { <-s.voiceSem }()
		defer jobCancel()
		defer func() {
			s.voiceMu.Lock()
			delete(s.voiceJobCancel, job.ID)
			s.voiceMu.Unlock()
		}()
		s.runVoiceInstall(jobCtx, job.ID, u.ID, voice, menuSpeed, emailSpeed)
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
		var installError *speech.VoiceInstallError
		if errors.As(jobErr, &installError) {
			job.Error = installError.Reason
		}
		if errors.Is(jobErr, context.Canceled) {
			job.Error = "voice installation canceled"
		}
		if errors.Is(jobErr, context.DeadlineExceeded) {
			job.Error = "voice installation timed out"
		}
		if s.Log != nil {
			s.Log.Warn("voice installation failed", "voice", job.Voice, "stage", stage, "reason", job.Error)
		}
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

// BeginShutdown stops HTTP/job admission and cancels running voice jobs. The
// HTTP server stops accepting connections; Close then joins admitted handlers
// and asynchronous jobs before the database closes.
func (s *Server) BeginShutdown() {
	if s == nil {
		return
	}
	s.admissionMu.Lock()
	s.closing = true
	s.admissionMu.Unlock()
	if s.voiceCancel != nil {
		s.voiceCancel()
	}
}

// Close closes admission and cancels voice jobs, then joins HTTP handlers and
// asynchronous jobs, including handlers retained after an HTTP shutdown timeout.
// The caller must cancel the HTTP base context before calling Close.
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.BeginShutdown()
	s.requestWG.Wait()
	s.voiceWG.Wait()
	if syncer, ok := s.Sync.(AccountSyncStarter); ok {
		syncer.WaitManualSyncs()
	}
}

func (s *Server) runVoiceInstall(ctx context.Context, id, userID string, voice speech.VoiceSpec, menuSpeed, emailSpeed int) {
	s.updateVoiceJob(id, "installing", "running", 10, nil)
	installer := s.VoiceInstaller
	if installer == nil {
		installer = speech.AdministratorVoiceInstaller{}
	}
	err := installer.Install(ctx, s.VoiceDir, voice)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && s.Voice != nil {
		s.updateVoiceJob(id, "activating", "running", 35, nil)
		err = s.Voice.ActivateVoice(ctx, voice.Voice, menuSpeed, emailSpeed)
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
	}
	if err != nil {
		s.updateVoiceJob(id, "failed", "failed", 100, err)
		return
	}
	s.updateVoiceJob(id, "complete", "complete", 100, nil)
	cleanupCtx, cleanupCancel := lifecycle.CleanupContext(context.WithoutCancel(ctx))
	_ = s.Store.Audit(cleanupCtx, userID, "voice_installed", voice.Voice)
	cleanupCancel()
}

func (s *Server) activeVoiceInstallJob(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	job, err := s.Store.ActiveVoiceInstallJob(r.Context())
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "idle"})
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, voiceInstallJobResult(job))
}

func (s *Server) cancelVoiceInstall(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	s.voiceMu.Lock()
	job := s.voiceJobs[id]
	var status string
	if job != nil {
		status = job.Status
	}
	cancel := s.voiceJobCancel[id]
	s.voiceMu.Unlock()
	if status == "" {
		stored, err := s.Store.VoiceInstallJob(r.Context(), id)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "voice installation job not found")
			return
		}
		if err != nil {
			serverError(w, err)
			return
		}
		status = stored.Status
	}
	if status != "queued" && status != "running" {
		writeError(w, http.StatusConflict, "voice installation is already finished")
		return
	}
	if cancel == nil {
		writeError(w, http.StatusConflict, "voice installation worker is no longer running")
		return
	}
	cancel()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancellation requested"})
}

func voiceInstallJobResult(job store.VoiceInstallJob) map[string]any {
	result := map[string]any{"job_id": job.ID, "voice": job.Voice, "stage": job.Stage, "status": job.Status, "progress": job.Progress, "updated_at": job.UpdatedAt}
	if job.Error != "" {
		result["error"] = job.Error
	}
	return result
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
	u, ok := s.require(w, r, true)
	if !ok {
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
	s.startPreview(w, u.ID, body.Voice, body.Speed)
}

// voiceWarmStatus reports which resident models are currently loaded and
// which installed voices the administrator pinned as always-warm.
func (s *Server) voiceWarmStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if s.SpeechPool == nil {
		writeError(w, http.StatusServiceUnavailable, "the resident speech engine is disabled")
		return
	}
	pins, err := s.Store.KeepWarmVoices(r.Context())
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			serverError(w, err)
			return
		}
		pins = make(map[string]bool)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resident":  s.SpeechPool.ResidentVoices(),
		"keep_warm": pins,
	})
}

// setVoiceKeepWarm pins or unpins an installed voice so its model stays
// loaded across calls. Only installed voices qualify; the request never
// triggers a download.
func (s *Server) setVoiceKeepWarm(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if s.SpeechPool == nil {
		writeError(w, http.StatusServiceUnavailable, "the resident speech engine is disabled")
		return
	}
	voice := strings.TrimSuffix(strings.TrimSpace(r.PathValue("voice")), ".onnx")
	if voice == "" {
		writeError(w, http.StatusBadRequest, "voice is required")
		return
	}
	if s.VoiceDir == "" {
		writeError(w, http.StatusServiceUnavailable, "voice directory is not configured")
		return
	}
	installed := false
	for _, name := range speech.InstalledVoiceNames(s.VoiceDir) {
		if name == voice {
			installed = true
			break
		}
	}
	if !installed {
		writeError(w, http.StatusNotFound, "voice is not installed")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	enabled := *body.Enabled
	if err := s.Store.SetVoiceKeepWarm(r.Context(), voice, enabled); err != nil {
		serverError(w, err)
		return
	}
	applied := false
	if enabled {
		// Menu speed keeps the pin consistent with the default activation;
		// the resident engine applies per-call speech rates itself.
		applied = s.SpeechPool.KeepWarm(voice, 3) != nil
	} else {
		applied = s.SpeechPool.ReleaseKeepWarm(voice)
	}
	writeJSON(w, http.StatusOK, map[string]any{"voice": voice, "keep_warm": enabled, "applied": applied})
}
