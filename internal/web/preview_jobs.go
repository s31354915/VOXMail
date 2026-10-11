package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/voxmail/voxmail/internal/operation"
)

const (
	maxPreviewJobs  = 4
	maxPreviewAudio = 16 << 20
	previewLifetime = 5 * time.Minute
	previewTimeout  = 2 * time.Minute
)

// Preview results are temporary, local artifacts. Their count and byte size
// are bounded, and ownership is rechecked on every status/audio/cancel request.
type previewJob struct {
	ID, UserID, Status string
	UpdatedAt          time.Time
	Audio              []byte
	Failure            *OperationalFailure
	cancel             context.CancelFunc
}

func (s *Server) startPreview(w http.ResponseWriter, userID, voice string, speed int) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.closing || s.voiceCtx.Err() != nil {
		writeError(w, http.StatusServiceUnavailable, "server is shutting down")
		return
	}
	s.previewMu.Lock()
	now := time.Now()
	if s.previewJobs == nil {
		s.previewJobs = make(map[string]*previewJob)
	}
	oldest := ""
	for id, job := range s.previewJobs {
		if job.Status == "running" {
			if job.UserID == userID {
				s.previewMu.Unlock()
				writeError(w, http.StatusConflict, "a voice preview is already running")
				return
			}
			continue
		}
		if now.Sub(job.UpdatedAt) > previewLifetime {
			delete(s.previewJobs, id)
			continue
		}
		if oldest == "" || job.UpdatedAt.Before(s.previewJobs[oldest].UpdatedAt) {
			oldest = id
		}
	}
	if len(s.previewJobs) >= maxPreviewJobs && oldest != "" {
		delete(s.previewJobs, oldest)
	}
	if len(s.previewJobs) >= maxPreviewJobs {
		s.previewMu.Unlock()
		writeError(w, http.StatusTooManyRequests, "voice preview capacity is full; retry after a running preview finishes")
		return
	}
	ctx, cancel := context.WithTimeout(s.voiceCtx, previewTimeout)
	job := &previewJob{ID: newID(), UserID: userID, Status: "running", UpdatedAt: now, cancel: cancel}
	s.previewJobs[job.ID] = job
	s.previewMu.Unlock()
	s.voiceWG.Add(1)
	go func() {
		defer s.voiceWG.Done()
		defer cancel()
		audio, err := s.Preview.PreviewVoice(ctx, voice, speed)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err == nil && (len(audio) == 0 || len(audio) > maxPreviewAudio) {
			err = operation.Wrap("preview", "output", operation.ResourceLimit, fmt.Errorf("voice preview output is invalid"))
		}
		s.previewMu.Lock()
		job.UpdatedAt = time.Now()
		job.cancel = nil
		if err != nil {
			job.Status = "failed"
			job.Failure = failureForError(err)
		} else {
			job.Status = "complete"
			job.Audio = audio
		}
		s.previewMu.Unlock()
	}()
	path := "/api/v1/voices/previews/" + job.ID
	w.Header().Set("Location", path)
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": job.ID, "status": "running", "status_url": path, "audio_url": path + "/audio"})
}

func (s *Server) previewJobStatus(w http.ResponseWriter, r *http.Request) {
	s.readPreviewJob(w, r, false)
}
func (s *Server) previewJobAudio(w http.ResponseWriter, r *http.Request) {
	s.readPreviewJob(w, r, true)
}

func (s *Server) readPreviewJob(w http.ResponseWriter, r *http.Request, audio bool) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	s.previewMu.Lock()
	job := s.previewJobs[r.PathValue("id")]
	if job == nil || job.UserID != u.ID || (job.Status != "running" && time.Since(job.UpdatedAt) > previewLifetime) {
		s.previewMu.Unlock()
		writeError(w, http.StatusNotFound, "voice preview job expired or was not found; start a new preview")
		return
	}
	snapshot := *job
	s.previewMu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	if !audio {
		writeJSON(w, http.StatusOK, map[string]any{"job_id": snapshot.ID, "status": snapshot.Status, "updated_at": snapshot.UpdatedAt, "failure": snapshot.Failure})
		return
	}
	if snapshot.Status != "complete" {
		writeError(w, http.StatusConflict, "voice preview audio is not available")
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", strconv.Itoa(len(snapshot.Audio)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(snapshot.Audio)
}

func (s *Server) cancelPreviewJob(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	s.previewMu.Lock()
	job := s.previewJobs[r.PathValue("id")]
	if job == nil || job.UserID != u.ID {
		s.previewMu.Unlock()
		writeError(w, http.StatusNotFound, "voice preview job not found")
		return
	}
	cancel := job.cancel
	s.previewMu.Unlock()
	if cancel == nil {
		writeError(w, http.StatusConflict, "voice preview is already finished")
		return
	}
	cancel()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancellation requested"})
}
