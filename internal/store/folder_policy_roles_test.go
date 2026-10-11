package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/voxmail/voxmail/internal/folderpolicy"
)

func TestGroupedRoleWritesUseOwnerPrimaryAndRejectExcludedRole(t *testing.T) {
	for _, role := range []string{"drafts", "trash", "sent"} {
		t.Run(role, func(t *testing.T) {
			ctx := context.Background()
			db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u','u','p','p','user','now')`); err != nil {
				t.Fatal(err)
			}
			first, chosen := "Provider/"+role, "Legacy/"+role
			mapping, _ := json.Marshal(map[string]string{first: "First", chosen: "Chosen"})
			if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','u','Mail','a@e','A','h','u','p','h','u','p',?,'now')`, string(mapping)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB.Exec(`INSERT INTO folder_roles(account_id,role,remote_path,local_path) VALUES('a',?,?,'First')`, role, first); err != nil {
				t.Fatal(err)
			}
			account := Account{ID: "a", UserID: "u", FolderMap: string(mapping)}
			policy := folderpolicy.Policy{Groups: []folderpolicy.Group{{ID: "role-pair", Name: "User mailbox", Primary: chosen, Folders: []string{first, chosen}}}}
			if err := db.SaveFolderPolicy(ctx, "u", "a", 1, policy); err != nil {
				t.Fatal(err)
			}
			destination, err := db.FolderRole(ctx, account, role)
			if err != nil || destination.RemotePath != chosen || destination.LocalPath != "Chosen" || destination.Role != role {
				t.Fatalf("old role replaced owner primary: %+v %v", destination, err)
			}
			if err := db.UpsertRemoteFolder(ctx, "a", chosen, "Chosen", role); err != nil {
				t.Fatal(err)
			}
			newMapping, _ := json.Marshal(map[string]string{first: "First", chosen: "PendingAlias"})
			if _, err := db.DB.Exec(`UPDATE accounts SET folder_map=? WHERE id='a'`, string(newMapping)); err != nil {
				t.Fatal(err)
			}
			destination, err = db.FolderRole(ctx, account, role)
			if err != nil || destination.RemotePath != chosen || destination.LocalPath != "Chosen" {
				t.Fatalf("pending remap displaced synchronized identity: %+v %v", destination, err)
			}
			remote, err := db.RemoteFolderForLocal(ctx, "u", "a", "Chosen")
			if err != nil || remote != chosen {
				t.Fatalf("source identity lost during remap: %q %v", remote, err)
			}
			if _, err := db.RemoteFolderForLocal(ctx, "u", "a", "/groups/role-pair"); err == nil {
				t.Fatal("logical key accepted as physical IMAP identity")
			}
			if err := db.UpsertRemoteFolder(ctx, "a", "DifferentRemote", "Chosen", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := db.RemoteFolderForLocal(ctx, "u", "a", "Chosen"); !errors.Is(err, ErrInvalidAccountConfig) {
				t.Fatalf("ambiguous physical source silently guessed: %v", err)
			}
			if err := db.SaveFolderPolicy(ctx, "u", "a", 2, folderpolicy.Policy{Excluded: []string{first}}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.FolderRole(ctx, account, role); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("excluded role used as write destination: %v", err)
			}
			var savedRemote string
			if err := db.DB.QueryRow(`SELECT remote_path FROM folder_roles WHERE account_id='a' AND role=?`, role).Scan(&savedRemote); err != nil || savedRemote != first {
				t.Fatalf("group/exclusion silently rewrote saved role: %q %v", savedRemote, err)
			}
		})
	}
}
