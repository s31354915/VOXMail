package calls

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/voxmail/voxmail/internal/folderpolicy"
	"github.com/voxmail/voxmail/internal/store"
)

// These fixtures also execute the actual console preview module under Node.
// Here they run through persisted mappings/roles and the live phone projection.
func TestFolderPreviewContractMatchesPhone(t *testing.T) {
	var fixtures []struct {
		Name  string
		Input struct {
			Folders   map[string]string
			Mapping   map[string]string
			Roles     map[string]string
			Policy    folderpolicy.Policy
			Mailboxes []store.LogicalMailbox
			Bindings  map[string]string
		}
		Folders []string
		Roles   map[string]string
		Pages   []struct {
			Prefix string
			Page   int
			Keys   []string
			More   bool
		}
	}
	data, err := os.ReadFile("../../tests/fixtures/folder-menu.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			svc, db := newTestService(t)
			ctx := context.Background()
			if err := db.CreateUser(ctx, store.User{ID: "owner", Username: "owner", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			mapping, _ := json.Marshal(fixture.Input.Mapping)
			if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','owner','Mail','a@e','A','h','u','p','h','u','p',?,'now')`, string(mapping)); err != nil {
				t.Fatal(err)
			}
			for remote, local := range fixture.Input.Folders {
				for historical, owner := range fixture.Input.Bindings {
					if owner != remote || historical == local {
						continue
					}
					if err := db.UpsertRemoteFolder(ctx, "a", remote, historical, ""); err != nil {
						t.Fatal(err)
					}
					if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,updated_at) VALUES('a',?,?, '<historical@test>','now')`, historical, "/mail/"+historical); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.UpsertRemoteFolder(ctx, "a", remote, local, ""); err != nil {
					t.Fatal(err)
				}
			}
			for _, mailbox := range fixture.Input.Mailboxes {
				for _, folder := range mailbox.Members {
					if _, err := db.DB.Exec(`INSERT INTO mail_messages(account_id,folder,path,message_id,updated_at) VALUES('a',?,?, '<legacy@test>','now')`, folder, "/mail/"+folder); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := db.SaveFolderConfiguration(ctx, "owner", "a", 1, fixture.Input.Policy, fixture.Input.Mapping, fixture.Input.Roles); err != nil {
				t.Fatal(err)
			}
			accounts, err := db.ListAccounts(ctx, "owner")
			if err != nil || len(accounts) != 1 {
				t.Fatalf("accounts: %v %v", accounts, err)
			}
			sess := &session{CallID: "preview-contract", UserID: "owner", ActiveAccount: "a", State: "account_menu", Accounts: accounts}
			svc.openAccountFolders(sess, accounts[0])
			slices.Sort(sess.Folders)
			expectedFolders := slices.Clone(fixture.Folders)
			slices.Sort(expectedFolders)
			if !reflect.DeepEqual(sess.Folders, expectedFolders) {
				t.Fatalf("folders: %v want %v", sess.Folders, expectedFolders)
			}
			for _, role := range []string{"inbox", "drafts", "sent", "trash"} {
				if sess.FolderRolePaths[role] != fixture.Roles[role] {
					t.Fatalf("role %s: %q want %q", role, sess.FolderRolePaths[role], fixture.Roles[role])
				}
			}
			for _, page := range fixture.Pages {
				children := folderMenuChildren(sess.Folders, sess.FolderRolePaths, page.Prefix, sess.FolderLabels)
				start, limit := page.Page*8, 8
				keys := []string{}
				if page.Prefix == "" {
					if page.Page == 0 {
						start, limit = 0, 4
						for _, role := range []string{"inbox", "drafts", "sent", "trash"} {
							keys = append(keys, sess.FolderRolePaths[role])
						}
					} else {
						start = 4 + (page.Page-1)*8
					}
				}
				if start < len(children) {
					keys = append(keys, children[start:min(start+limit, len(children))]...)
				}
				if !reflect.DeepEqual(keys, page.Keys) {
					t.Fatalf("page %s/%d keys %v want %v", page.Prefix, page.Page, keys, page.Keys)
				}
				if got := folderPageHasMore(len(children), page.Prefix == "", page.Page); got != page.More {
					t.Fatalf("page %s/%d more=%v want %v", page.Prefix, page.Page, got, page.More)
				}
				if page.Prefix != "" && len(page.Keys) > 0 {
					sess.FolderPrefix, sess.FolderPage = page.Prefix, page.Page
					svc.handleFolderMenu(sess, "1")
					if sess.SelectedFolder != page.Keys[0] {
						t.Fatalf("nested selection lost full path: %q want %q", sess.SelectedFolder, page.Keys[0])
					}
					svc.handleFolderAction(sess, "1")
					if page.Prefix == "Legacy" && (len(sess.Messages) != 1 || sess.Messages[0].Folder != "Legacy/Nested") {
						t.Fatalf("nested legacy messages inaccessible: %+v", sess.Messages)
					}
				}
			}
		})
	}
}
