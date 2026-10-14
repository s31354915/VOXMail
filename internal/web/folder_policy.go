package web

import (
	"errors"
	"net/http"

	"github.com/voxmail/voxmail/internal/folderpolicy"
	"github.com/voxmail/voxmail/internal/store"
)

func (s *Server) accountFolderPolicy(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	id := r.PathValue("id")
	policy, version, err := s.Store.FolderPolicySnapshot(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrAccountNotFound) {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	inventory, err := s.Store.FolderInventory(r.Context(), u.ID, id)
	if err != nil {
		serverError(w, err)
		return
	}
	warnings := []string{}
	mailboxes, err := s.Store.ListLogicalMailboxes(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrInvalidAccountConfig) {
		warnings = append(warnings, "Some folders share a local destination. Choose a distinct destination for each folder before saving. The phone preview is unavailable until these conflicts are resolved.")
		mailboxes = nil
	} else if err != nil {
		serverError(w, err)
		return
	}
	accounts, err := s.Store.ListAccounts(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	var mapping map[string]string
	for _, account := range accounts {
		if account.ID == id {
			if account.ConfigVersion != version {
				writeError(w, http.StatusConflict, "account settings changed; reload folder choices")
				return
			}
			mapping = accountJSON(account).FolderMap
		}
	}
	if mapping == nil {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	roles, err := s.Store.ListFolderRoles(r.Context(), u.ID, id)
	if err != nil {
		serverError(w, err)
		return
	}
	roleMap := make(map[string]string, len(roles))
	for _, role := range roles {
		roleMap[role.Role] = role.RemotePath
	}
	bindings, err := s.Store.FolderBindingsForPreview(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrInvalidAccountConfig) {
		if len(warnings) == 0 {
			warnings = append(warnings, "Retained folder identities conflict. Choose distinct local destinations; the phone preview is unavailable until the conflict is resolved.")
		}
		bindings = nil
	} else if err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"config_version": version, "policy": policy, "folders": inventory, "mailboxes": mailboxes, "folder_map": mapping, "folder_roles": roleMap, "folder_bindings": bindings, "warnings": warnings})
}

func (s *Server) saveAccountFolderPolicy(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	var request struct {
		Version int                  `json:"config_version"`
		Policy  *folderpolicy.Policy `json:"policy"`
		Mapping map[string]string    `json:"folder_map"`
		Roles   map[string]string    `json:"folder_roles"`
	}
	if !decode(w, r, &request) {
		return
	}
	if request.Version < 1 || request.Policy == nil {
		writeError(w, http.StatusBadRequest, "config_version and explicit folder policy are required")
		return
	}
	id := r.PathValue("id")
	if (request.Mapping == nil) != (request.Roles == nil) {
		writeError(w, http.StatusBadRequest, "folder_map and folder_roles must be provided together")
		return
	}
	err := s.Store.SaveFolderConfiguration(r.Context(), u.ID, id, request.Version, *request.Policy, request.Mapping, request.Roles)
	switch {
	case errors.Is(err, store.ErrAccountNotFound):
		writeError(w, http.StatusNotFound, "account not found")
		return
	case errors.Is(err, store.ErrAccountConfigConflict):
		writeError(w, http.StatusConflict, "account settings changed; reload before saving folder choices")
		return
	case errors.Is(err, store.ErrInvalidAccountConfig), errors.Is(err, store.ErrInvalidFolderRole):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		serverError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), u.ID, "account_folder_policy_saved", id)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "config_version": request.Version + 1})
}
