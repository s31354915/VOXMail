package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/folderpolicy"
)

func TestFolderPolicyHTTPRequiresOwnerExplicitChoicesAndCurrentVersion(t *testing.T) {
	srv, h := voiceWebFixture(t)
	if _, err := srv.Store.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','user','Mail','a@e','A','h','u','SensitiveSecret','h','u','SensitiveSecret','{"INBOX":"Inbox","[Gmail]/Inbox":"GmailInbox"}','now')`); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/accounts/a/folder-policy"
	if response := voiceRequest(h, "", http.MethodGet, path, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous read: %d", response.Code)
	}
	if response := voiceRequest(h, "admin", http.MethodGet, path, nil); response.Code != http.StatusNotFound {
		t.Fatalf("cross-user admin read: %d %s", response.Code, response.Body.String())
	}
	response := voiceRequest(h, "user", http.MethodGet, path, nil)
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "SensitiveSecret") {
		t.Fatalf("snapshot: %d %s", response.Code, response.Body.String())
	}
	var snapshot struct {
		Version int                 `json:"config_version"`
		Folders map[string]string   `json:"folders"`
		Policy  folderpolicy.Policy `json:"policy"`
		Mapping map[string]string   `json:"folder_map"`
		Roles   map[string]string   `json:"folder_roles"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 1 || len(snapshot.Folders) != 2 || len(snapshot.Policy.Groups) != 0 {
		t.Fatalf("legacy snapshot inferred choices: %+v", snapshot)
	}
	policy := folderpolicy.Policy{Groups: []folderpolicy.Group{{ID: "inbox", Name: "Inbox", Primary: "[Gmail]/Inbox", Folders: []string{"INBOX", "[Gmail]/Inbox"}}}}
	payload := map[string]any{"config_version": 1, "policy": policy}
	request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"config_version":1,"policy":{}}`))
	request.AddCookie(&http.Cookie{Name: "voxmail_session", Value: "user-token"})
	request.Header.Set("Content-Type", "application/json")
	writer := httptest.NewRecorder()
	h.ServeHTTP(writer, request)
	if writer.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF: %d", writer.Code)
	}
	if response := voiceRequest(h, "admin", http.MethodPut, path, payload); response.Code != http.StatusNotFound {
		t.Fatalf("cross-user write: %d", response.Code)
	}
	for _, bad := range []any{map[string]any{"config_version": 1}, map[string]any{"policy": policy}, map[string]any{"config_version": 1, "policy": map[string]any{"excluded": []string{"Unknown"}}}} {
		if response := voiceRequest(h, "user", http.MethodPut, path, bad); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid request accepted: %d %s", response.Code, response.Body.String())
		}
	}
	if response := voiceRequest(h, "user", http.MethodPut, path, payload); response.Code != http.StatusOK {
		t.Fatalf("save: %d %s", response.Code, response.Body.String())
	}
	if response := voiceRequest(h, "user", http.MethodPut, path, payload); response.Code != http.StatusConflict {
		t.Fatalf("stale save: %d %s", response.Code, response.Body.String())
	}
	response = voiceRequest(h, "user", http.MethodGet, path, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("saved snapshot: %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 2 || len(snapshot.Policy.Groups) != 1 || snapshot.Policy.Groups[0].Primary != "[Gmail]/Inbox" {
		t.Fatalf("saved owner choices: %+v", snapshot)
	}
	if snapshot.Mapping["INBOX"] != "Inbox" || snapshot.Mapping["[Gmail]/Inbox"] != "GmailInbox" || snapshot.Roles == nil {
		t.Fatalf("editor metadata missing: %+v", snapshot)
	}
	mapping := map[string]string{"INBOX": "Inbox", "[Gmail]/Inbox": "ChosenInbox"}
	roles := map[string]string{"inbox": "[Gmail]/Inbox"}
	for _, bad := range []any{
		map[string]any{"config_version": 2, "policy": policy, "folder_map": mapping},
		map[string]any{"config_version": 2, "policy": policy, "folder_roles": roles},
		map[string]any{"config_version": 2, "policy": policy, "folder_map": map[string]string{"Unknown": "Unknown"}, "folder_roles": map[string]string{}},
		map[string]any{"config_version": 2, "policy": policy, "folder_map": mapping, "folder_roles": map[string]string{"invalid-role": "INBOX"}},
		map[string]any{"config_version": 2, "policy": map[string]any{"excluded": []string{"Unknown"}}, "folder_map": mapping, "folder_roles": roles},
	} {
		if response := voiceRequest(h, "user", http.MethodPut, path, bad); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid combined request accepted: %d %s", response.Code, response.Body.String())
		}
	}
	response = voiceRequest(h, "user", http.MethodGet, path, nil)
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 2 || snapshot.Mapping["[Gmail]/Inbox"] != "GmailInbox" || len(snapshot.Roles) != 0 {
		t.Fatalf("failed combined requests partially saved: %+v", snapshot)
	}
	combined := map[string]any{"config_version": 2, "policy": policy, "folder_map": mapping, "folder_roles": roles}
	if response := voiceRequest(h, "user", http.MethodPut, path, combined); response.Code != http.StatusOK {
		t.Fatalf("combined save: %d %s", response.Code, response.Body.String())
	}
	response = voiceRequest(h, "user", http.MethodGet, path, nil)
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 3 || snapshot.Mapping["[Gmail]/Inbox"] != "ChosenInbox" || snapshot.Roles["inbox"] != "[Gmail]/Inbox" || len(snapshot.Policy.Groups) != 1 || snapshot.Policy.Groups[0].Primary != "[Gmail]/Inbox" {
		t.Fatalf("combined owner choices missing: %+v", snapshot)
	}
	if response := voiceRequest(h, "user", http.MethodPut, path, combined); response.Code != http.StatusConflict {
		t.Fatalf("stale combined save: %d %s", response.Code, response.Body.String())
	}
}

func TestFolderMappingValidationRetainsCaseDistinctRemoteNames(t *testing.T) {
	if err := validateFolderMap(map[string]string{"Sent": "UpperSent", "sent": "LowerSent"}); err != nil {
		t.Fatalf("distinct remote names refused: %v", err)
	}
	if err := validateFolderMap(map[string]string{"INBOX": "Inbox", "inbox": "OtherInbox"}); err == nil {
		t.Fatal("duplicate reserved Inbox identity accepted")
	}
	if err := validateFolderMap(map[string]string{"One": "Same", "Two": "same"}); err == nil {
		t.Fatal("colliding local aliases accepted")
	}
	for _, mapping := range []map[string]string{
		{"One": "A/B", "Two": "A//B"},
		{"One": "A/B", "Two": "A/./B"},
		{"One": "A/B", "Two": `A\B`},
		{"One": "."},
	} {
		if err := validateFolderMap(mapping); err == nil {
			t.Fatalf("canonical local alias collision accepted: %v", mapping)
		}
	}
}

func TestFolderChoicesCanRepairDiscoveredDestinationConflicts(t *testing.T) {
	srv, h := voiceWebFixture(t)
	_, err := srv.Store.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','user','Mail','a@e','A','h','u','p','h','u','p','{"[Gmail]/Sent Mail":"Sent","[Gmail]/Trash":"Trash"}','now');
 INSERT INTO account_folder_discovery(account_id,remote_path) VALUES('a','Sent'),('a','Trash'),('a','[Gmail]/Sent Mail'),('a','[Gmail]/Trash');`)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/accounts/a/folder-policy"
	response := voiceRequest(h, "user", http.MethodGet, path, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("conflicting editor failed: %d %s", response.Code, response.Body.String())
	}
	var snapshot struct {
		Warnings []string          `json:"warnings"`
		Folders  map[string]string `json:"folders"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Warnings) == 0 || len(snapshot.Folders) != 4 {
		t.Fatalf("missing repair inventory: %s", response.Body.String())
	}
	payload := map[string]any{"config_version": 1, "policy": folderpolicy.Policy{}, "folder_map": map[string]string{"[Gmail]/Sent Mail": "Sent", "[Gmail]/Trash": "Trash"}, "folder_roles": map[string]string{}}
	if response := voiceRequest(h, "user", http.MethodPut, path, payload); response.Code != http.StatusBadRequest {
		t.Fatalf("accepted conflict: %d %s", response.Code, response.Body.String())
	}
	payload["folder_map"] = map[string]string{"[Gmail]/Sent Mail": "Sent", "[Gmail]/Trash": "Trash", "Sent": "Separate/Sent", "Trash": "Separate/Trash"}
	if response := voiceRequest(h, "user", http.MethodPut, path, payload); response.Code != http.StatusOK {
		t.Fatalf("repair failed: %d %s", response.Code, response.Body.String())
	}
	if err := srv.Store.BindSyncFolders(context.Background(), "user", "a", 2); err != nil {
		t.Fatalf("repaired sync still blocked: %v", err)
	}
	response = voiceRequest(h, "user", http.MethodGet, path, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("repaired editor: %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Warnings) != 0 {
		t.Fatalf("repair warnings remain: %v", snapshot.Warnings)
	}
}
