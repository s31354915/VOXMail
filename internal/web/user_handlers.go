package web

import (
	"net/http"
	"strings"

	"github.com/voxmail/voxmail/internal/auth"
	"github.com/voxmail/voxmail/internal/store"
)

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, publicUser(u))
	}
	writeJSON(w, http.StatusOK, out)
}

type userRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	PIN      string `json:"pin"`
	Role     string `json:"role"`
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var req userRequest
	if !decode(w, r, &req) {
		return
	}
	if err := validateCredentials(req.Username, req.Password, req.PIN); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	pw, err := auth.Hash(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "password cannot be hashed")
		return
	}
	pin, err := auth.Hash(req.PIN)
	if err != nil {
		writeError(w, http.StatusBadRequest, "PIN cannot be hashed")
		return
	}
	role := "user"
	if req.Role == "admin" {
		role = "admin"
	}
	u := store.User{ID: newID(), Username: strings.TrimSpace(req.Username), PasswordHash: pw, PINHash: pin, Role: role, Enabled: true}
	if err := s.Store.CreateUser(r.Context(), u); err != nil {
		writeError(w, http.StatusConflict, "username already exists")
		return
	}
	writeJSON(w, http.StatusCreated, publicUser(u))
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.PathValue("id") == admin.ID {
		writeError(w, http.StatusBadRequest, "admin cannot delete itself")
		return
	}
	userID := r.PathValue("id")
	accounts, err := s.Store.ListAccounts(r.Context(), userID)
	if err != nil {
		serverError(w, err)
		return
	}
	if coordinator, ok := s.Sync.(AccountDeletionCoordinator); s.Sync != nil && !ok {
		writeError(w, http.StatusServiceUnavailable, "mail synchronization cannot be stopped safely")
		return
	} else if coordinator != nil {
		for _, account := range accounts {
			if err := coordinator.StopAccount(r.Context(), account.ID); err != nil {
				serverError(w, err)
				return
			}
		}
	}
	s.invalidateDeletedUserCalls(userID)
	cleanupPending := false
	for _, account := range accounts {
		pending, err := s.deleteAccountData(r.Context(), userID, account.ID, false)
		if err != nil {
			serverError(w, err)
			return
		}
		cleanupPending = cleanupPending || pending
	}
	if err := s.Store.DeleteUser(r.Context(), userID); err != nil {
		serverError(w, err)
		return
	}
	if cleanupPending {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "deleted", "cleanup_pending": true})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
