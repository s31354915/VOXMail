package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/voxmail/voxmail/internal/phone"
	"github.com/voxmail/voxmail/internal/store"
)

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	settings, err := s.Store.UserSettings(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	available, err := s.Store.AlertsAvailable(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	result := map[string]any{"tts_voice": settings.TTSVoice, "menu_speed": settings.MenuSpeed, "email_speed": settings.EmailSpeed, "alerts_available": available}
	if available || u.Role == "admin" {
		result["alerts_enabled"] = settings.AlertsEnabled
		result["alert_phone"] = settings.AlertPhone
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) adminAlerts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	available, err := s.Store.AlertsAvailable(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"available": available})
}

func (s *Server) saveAdminAlerts(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		Available bool `json:"available"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.Store.SetAlertsAvailable(r.Context(), body.Available); err != nil {
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "global_alert_availability_changed", fmt.Sprintf("available=%t", body.Available))
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var body struct {
		TTSVoice      string  `json:"tts_voice"`
		MenuSpeed     int     `json:"menu_speed"`
		EmailSpeed    int     `json:"email_speed"`
		AlertsEnabled bool    `json:"alerts_enabled"`
		AlertPhone    *string `json:"alert_phone"`
	}
	if !decode(w, r, &body) {
		return
	}
	alertsAvailable, err := s.Store.AlertsAvailable(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if !alertsAvailable && u.Role != "admin" {
		body.AlertsEnabled = false
		body.AlertPhone = nil
	}
	preserveAlertSettings := !alertsAvailable
	if preserveAlertSettings {
		settings, err := s.Store.UserSettings(r.Context(), u.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		body.AlertsEnabled = settings.AlertsEnabled
		body.AlertPhone = settings.AlertPhone
	}
	if body.TTSVoice == "" {
		body.TTSVoice = "en_US-hfc_male-medium"
	}
	if body.MenuSpeed < 1 || body.MenuSpeed > 5 {
		body.MenuSpeed = 3
	}
	if body.EmailSpeed < 1 || body.EmailSpeed > 5 {
		body.EmailSpeed = 2
	}
	body.TTSVoice = strings.TrimSpace(body.TTSVoice)
	if body.TTSVoice == "" || strings.ContainsAny(body.TTSVoice, "/\\\x00\r\n") || strings.Contains(body.TTSVoice, "..") {
		writeError(w, http.StatusBadRequest, "invalid voice model")
		return
	}
	if !s.voiceAvailable(body.TTSVoice) {
		writeError(w, http.StatusBadRequest, "voice model is not installed; an administrator must install it first")
		return
	}
	// Preference writes stay short. Model installation and prompt activation are
	// administrator jobs, so a normal user save cannot trigger a long download
	// or hold an HTTP request open for minutes.
	if body.AlertPhone != nil {
		phone := phone.Normalize(*body.AlertPhone)
		if phone == "" {
			body.AlertPhone = nil
		} else {
			body.AlertPhone = &phone
		}
	}
	err = s.Store.SaveUserSettings(r.Context(), u.ID, store.UserSettings{TTSVoice: body.TTSVoice, MenuSpeed: body.MenuSpeed, EmailSpeed: body.EmailSpeed, AlertsEnabled: body.AlertsEnabled, AlertPhone: body.AlertPhone})
	if err != nil {
		serverError(w, err)
		return
	}
	if preserveAlertSettings {
		// Global alert disablement must not erase a user's saved destinations.
	} else if body.AlertPhone == nil {
		if err := s.Store.ClearAlertNumbers(r.Context(), u.ID); err != nil {
			serverError(w, err)
			return
		}
	} else if err := s.Store.SaveAlertNumber(r.Context(), u.ID, *body.AlertPhone, true); err != nil {
		serverError(w, err)
		return
	}
	result := map[string]any{"tts_voice": body.TTSVoice, "menu_speed": body.MenuSpeed, "email_speed": body.EmailSpeed}
	if alertsAvailable || u.Role == "admin" {
		result["alerts_enabled"] = body.AlertsEnabled
		result["alert_phone"] = body.AlertPhone
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) alertNumbers(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAlerts(w, r, false)
	if !ok {
		return
	}
	numbers, err := s.Store.ListAlertNumbers(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	// Migrate the legacy single-number setting into the durable list on first
	// read, preserving existing deployments without exposing two sources of
	// truth to the browser.
	if len(numbers) == 0 {
		if legacy, legacyErr := s.Store.ActiveAlertNumber(r.Context(), u.ID); legacyErr == nil && legacy != "" {
			if saveErr := s.Store.SaveAlertNumber(r.Context(), u.ID, legacy, true); saveErr == nil {
				numbers, _ = s.Store.ListAlertNumbers(r.Context(), u.ID)
			}
		}
	}
	writeJSON(w, http.StatusOK, numbers)
}

func (s *Server) saveAlertNumber(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAlerts(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Number string `json:"number"`
		Active bool   `json:"active"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Number = phone.Normalize(body.Number)
	if body.Number == "" {
		writeError(w, http.StatusBadRequest, "a valid alert number is required")
		return
	}
	if err := s.Store.SaveAlertNumber(r.Context(), u.ID, body.Number, body.Active); err != nil {
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "alert_number_saved", body.Number)
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

func (s *Server) activateAlertNumber(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAlerts(w, r, true)
	if !ok {
		return
	}
	id := parseID(r.PathValue("id"))
	if id < 1 {
		writeError(w, http.StatusBadRequest, "invalid alert number id")
		return
	}
	if err := s.Store.SetActiveAlertNumber(r.Context(), u.ID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "alert number not found")
		} else {
			serverError(w, err)
		}
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "alert_number_activated", fmt.Sprint(id))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAlertNumber(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAlerts(w, r, true)
	if !ok {
		return
	}
	id := parseID(r.PathValue("id"))
	if id < 1 {
		writeError(w, http.StatusBadRequest, "invalid alert number id")
		return
	}
	if err := s.Store.DeleteAlertNumber(r.Context(), u.ID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "alert number not found")
		} else {
			serverError(w, err)
		}
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "alert_number_deleted", fmt.Sprint(id))
	w.WriteHeader(http.StatusNoContent)
}
