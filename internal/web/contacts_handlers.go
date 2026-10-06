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

func (s *Server) contacts(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if ok {
		out, err := s.Store.ListContacts(r.Context(), u.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}
func (s *Server) createContact(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var c store.Contact
	if !decode(w, r, &c) {
		return
	}
	c.UserID = u.ID
	if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Email) == "" {
		writeError(w, http.StatusBadRequest, "name and email are required")
		return
	}
	id, err := s.Store.AddContact(r.Context(), c)
	if err != nil {
		writeError(w, http.StatusConflict, "contact already exists")
		return
	}
	c.ID = id
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) updateContact(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	c := store.Contact{ID: parseID(r.PathValue("id"))}
	if c.ID < 1 || !decode(w, r, &c) {
		if c.ID < 1 {
			writeError(w, http.StatusBadRequest, "invalid contact id")
		}
		return
	}
	c.UserID = u.ID
	if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Email) == "" {
		writeError(w, http.StatusBadRequest, "name and email are required")
		return
	}
	if err := s.Store.UpdateContact(r.Context(), c); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "contact not found")
			return
		}
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}
func (s *Server) deleteContact(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	id := parseID(r.PathValue("id"))
	if id < 1 {
		writeError(w, http.StatusBadRequest, "invalid contact id")
		return
	}
	if err := s.Store.DeleteContact(r.Context(), u.ID, id); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) whitelist(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	out, err := s.Store.ListWhitelist(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) addWhitelist(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var e store.WhitelistEntry
	if !decode(w, r, &e) {
		return
	}
	e.UserID = u.ID
	e.Phone = phone.Normalize(e.Phone)
	if e.Phone == "" {
		writeError(w, http.StatusBadRequest, "phone is required")
		return
	}
	if err := s.Store.AddWhitelist(r.Context(), e); err != nil {
		writeError(w, http.StatusConflict, "phone already belongs to a user")
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "caller_whitelist_added", e.Phone)
	writeJSON(w, http.StatusCreated, e)
}
func (s *Server) deleteWhitelist(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	id := parseID(r.PathValue("id"))
	if id < 1 {
		writeError(w, http.StatusBadRequest, "invalid whitelist id")
		return
	}
	if err := s.Store.DeleteWhitelist(r.Context(), u.ID, id); err != nil {
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "caller_whitelist_deleted", fmt.Sprint(id))
	w.WriteHeader(http.StatusNoContent)
}
