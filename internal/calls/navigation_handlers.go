package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/store"
)

func (s *Service) openAccounts(sess *session) {
	accounts, err := s.Store.ListAccounts(s.baseContext(), sess.UserID)
	if err != nil || len(accounts) == 0 {
		s.prompt(sess, "No mail accounts are configured.")
		return
	}
	s.mu.Lock()
	sess.Accounts = accounts
	sess.AccountPage = 0
	transitionLocked(sess, "accounts")
	sess.ActiveAccount = ""
	s.mu.Unlock()
	s.prompt(sess, "Email. Press 0 for all unread mail. "+accountPrompt(accounts, 0))
}

func (s *Service) openMessages(sess *session, unreadOnly bool, description string) {
	mails, err := s.Store.ListMail(s.baseContext(), sess.UserID, unreadOnly)
	if err != nil {
		s.prompt(sess, "Mail is temporarily unavailable.")
		return
	}
	s.mu.Lock()
	sess.Messages, sess.Cursor = mails, 0
	transitionLocked(sess, "list")
	s.mu.Unlock()
	if len(mails) == 0 {
		s.prompt(sess, "There are no "+description+".")
	} else {
		s.prompt(sess, listPrompt(mails[0], 0, len(mails)))
	}
}

func (s *Service) handleAccountMenu(sess *session, key string) {
	if key < "1" || key > "9" {
		return
	}
	s.mu.Lock()
	if key == "9" && len(sess.Accounts) > (sess.AccountPage+1)*8 {
		sess.AccountPage++
		page := sess.AccountPage
		accounts := append([]store.Account(nil), sess.Accounts...)
		s.mu.Unlock()
		s.prompt(sess, "Email. Press 0 for all unread mail. "+accountPrompt(accounts, page))
		return
	}
	page := sess.AccountPage
	index := int(key[0] - '1')
	index += page * 8
	if key == "9" || index >= len(sess.Accounts) {
		s.mu.Unlock()
		return
	}
	account := sess.Accounts[index]
	sess.ActiveAccount = account.ID
	transitionLocked(sess, "account_menu")
	s.mu.Unlock()
	s.prompt(sess, s.accountMenuPrompt(sess, account))
}

func (s *Service) handleSelectedAccountMenu(sess *session, key string) {
	s.mu.Lock()
	accountID := sess.ActiveAccount
	var account store.Account
	for _, candidate := range sess.Accounts {
		if candidate.ID == accountID {
			account = candidate
			break
		}
	}
	s.mu.Unlock()
	if account.ID == "" {
		s.prompt(sess, "That account is unavailable.")
		return
	}
	switch key {
	case "1":
		s.openAccountFolders(sess, account)
	case "2":
		s.mu.Lock()
		transitionLocked(sess, "draft_menu")
		s.mu.Unlock()
		s.prompt(sess, "Press 1 for a new email, 2 to resume a saved draft, or pound to go back.")
	case "3":
		s.refreshAccount(sess, account.ID)
	case "4":
		if !s.alertsAvailable(sess) {
			s.prompt(sess, "Alert settings are disabled by the administrator. Press pound to return.")
			return
		}
		s.mu.Lock()
		transitionLocked(sess, "account_settings")
		s.mu.Unlock()
		s.prompt(sess, menuPrompt("account_settings"))
	}
}

func (s *Service) handleDraftMenu(sess *session, key string) {
	switch key {
	case "1":
		s.startCompose(sess, "")
		s.prompt(sess, "Enter the recipient using multi tap, then press pound.")
	case "2":
		s.openDrafts(sess)
	}
}

func (s *Service) handleDraftEdit(sess *session, key string) {
	s.mu.Lock()
	switch key {
	case "1":
		transitionLocked(sess, "recipient_edit")
	case "2":
		sess.EditingDraft = true
		sess.Draft.Subject = ""
		transitionLocked(sess, "subject_method")
	case "3":
		sess.EditingDraft = true
		sess.Draft.Body = ""
		transitionLocked(sess, "body_method")
	case "4":
		transitionLocked(sess, "attachment_edit")
	default:
		s.mu.Unlock()
		return
	}
	state := string(stateLocked(sess))
	s.mu.Unlock()
	switch state {
	case "recipient_edit":
		s.prompt(sess, menuPrompt("recipient_edit"))
	case "subject_method", "body_method", "attachment_edit":
		s.prompt(sess, menuPrompt(state))
	}
}

func (s *Service) handleRecipientEdit(sess *session, key string) {
	s.mu.Lock()
	switch key {
	case "1":
		sess.Draft.To = ""
		sess.Draft.AdditionalTo = nil
		sess.EditingDraft = true
		transitionLocked(sess, "compose")
		sess.Editor = newIVREditor("compose")
	case "2":
		sess.Draft.Cc = nil
		transitionLocked(sess, "draft_edit")
	case "3":
		sess.Draft.Bcc = nil
		transitionLocked(sess, "draft_edit")
	case "4":
		sess.EditingDraft = true
		transitionLocked(sess, "recipient_menu")
	default:
		s.mu.Unlock()
		return
	}
	state := string(stateLocked(sess))
	d := sess.Draft
	s.mu.Unlock()
	if state == "compose" {
		s.prompt(sess, "Enter the replacement primary recipient using multi tap, then press pound.")
	} else if state == "recipient_menu" {
		s.prompt(sess, recipientPrompt(d))
	} else {
		s.prompt(sess, "Recipient changes applied. "+menuPrompt("draft_edit"))
	}
}

func (s *Service) handleAttachmentEdit(sess *session, key string) {
	switch key {
	case "1":
		s.startAudioAttachment(sess)
	case "2", "3":
		s.mu.Lock()
		hasAttachment := len(sess.Draft.Attachments) > 0
		if hasAttachment {
			sess.Draft.Attachments = sess.Draft.Attachments[:len(sess.Draft.Attachments)-1]
		}
		count := len(sess.Draft.Attachments)
		s.mu.Unlock()
		if !hasAttachment {
			s.prompt(sess, "There is no attachment to change. Press 1 to add one, or pound to return.")
			return
		}
		if key == "3" {
			s.startAudioAttachment(sess)
			return
		}
		s.prompt(sess, fmt.Sprintf("The last attachment was removed. %d attachments remain. Press 1 to add one, or pound to return.", count))
	}
}

func (s *Service) openDrafts(sess *session) {
	s.mu.Lock()
	accountID := sess.ActiveAccount
	s.mu.Unlock()
	drafts, err := s.Store.ListDrafts(s.baseContext(), sess.UserID, accountID)
	if err != nil {
		s.prompt(sess, "Saved drafts are temporarily unavailable.")
		return
	}
	if len(drafts) == 0 {
		s.prompt(sess, "There are no saved drafts. Press 1 for a new email, or pound to go back.")
		return
	}
	s.mu.Lock()
	sess.Drafts = drafts
	sess.DraftPage = 0
	transitionLocked(sess, "draft_list")
	s.mu.Unlock()
	s.promptDraftList(sess)
}

func (s *Service) promptDraftList(sess *session) {
	s.mu.Lock()
	page := sess.DraftPage
	drafts := append([]store.DraftRecord(nil), sess.Drafts...)
	s.mu.Unlock()
	start := page * 8
	if start >= len(drafts) {
		s.prompt(sess, "There are no more saved drafts. Press pound to go back.")
		return
	}
	end := start + 8
	if end > len(drafts) {
		end = len(drafts)
	}
	parts := make([]string, 0, end-start)
	for i, item := range drafts[start:end] {
		description := strings.TrimSpace(item.Subject)
		if description == "" {
			description = "no subject"
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s", i+1, description))
	}
	next := ""
	if end < len(drafts) {
		next = " Press 9 for more."
	}
	s.prompt(sess, "Saved drafts. "+strings.Join(parts, ". ")+"."+next+" Press pound to go back.")
}

func (s *Service) handleDraftList(sess *session, key string) {
	if key == "9" {
		s.mu.Lock()
		if (sess.DraftPage+1)*8 < len(sess.Drafts) {
			sess.DraftPage++
		}
		s.mu.Unlock()
		s.promptDraftList(sess)
		return
	}
	if key < "1" || key > "8" {
		return
	}
	s.mu.Lock()
	index := sess.DraftPage*8 + int(key[0]-'1')
	if index < 0 || index >= len(sess.Drafts) {
		s.mu.Unlock()
		return
	}
	draftRecord := sess.Drafts[index]
	s.mu.Unlock()
	s.resumeDraft(sess, draftRecord)
}

func (s *Service) resumeDraft(sess *session, record store.DraftRecord) {
	if record.UserID != sess.UserID {
		s.prompt(sess, "That draft is unavailable.")
		return
	}
	attachments := make([]mailer.Attachment, 0, len(record.Attachments))
	for _, saved := range record.Attachments {
		path, ok := s.safeDraftAttachmentPath(sess, saved.Path)
		if !ok {
			s.prompt(sess, "That draft contains an unsafe attachment path.")
			return
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 25<<20 {
			s.prompt(sess, "A draft attachment is unavailable.")
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			s.prompt(sess, "A draft attachment could not be loaded.")
			return
		}
		attachments = append(attachments, mailer.Attachment{Filename: saved.Filename, ContentType: saved.ContentType, Data: data})
	}
	d := draft{ID: record.ID, Subject: record.Subject, Body: record.Body, Cc: append([]string(nil), record.Cc...), Bcc: append([]string(nil), record.Bcc...), Attachments: attachments, ForwardOriginal: record.ForwardMode == "forward", OriginalMessageID: record.OriginalMessageID}
	if len(record.To) > 0 {
		d.To = record.To[0]
		d.AdditionalTo = append([]string(nil), record.To[1:]...)
	}
	s.mu.Lock()
	sess.Draft = d
	sess.ActiveAccount = record.AccountID
	sess.RecipientKind = ""
	sess.Editor = nil
	switch {
	case strings.TrimSpace(d.To) == "":
		transitionLocked(sess, "compose")
		sess.Editor = newIVREditor("compose")
	case strings.TrimSpace(d.Subject) == "":
		transitionLocked(sess, "subject_method")
	case strings.TrimSpace(d.Body) == "":
		transitionLocked(sess, "body_method")
	default:
		transitionLocked(sess, "review")
	}
	state := string(stateLocked(sess))
	s.mu.Unlock()
	s.prompt(sess, "Draft resumed. "+menuPrompt(state))
}

func (s *Service) safeDraftAttachmentPath(sess *session, path string) (string, bool) {
	root := s.DataRoot
	if root == "" {
		root = filepath.Dir(sess.TxPath)
	}
	draftsRoot, err := filepath.Abs(filepath.Join(root, "drafts"))
	if err != nil {
		return "", false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(draftsRoot, pathAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return pathAbs, true
}

func (s *Service) handleAccountSettings(sess *session, key string) {
	s.mu.Lock()
	accountID := sess.ActiveAccount
	var account store.Account
	for _, candidate := range sess.Accounts {
		if candidate.ID == accountID {
			account = candidate
			break
		}
	}
	s.mu.Unlock()
	if account.ID == "" {
		s.prompt(sess, "That account is unavailable.")
		return
	}
	switch key {
	case "1":
		newValue := !account.CallAlertEnabled
		if err := s.Store.SetAccountCallAlertEnabled(s.baseContext(), sess.UserID, account.ID, newValue); err != nil {
			s.prompt(sess, "The account alert setting could not be changed.")
			return
		}
		s.mu.Lock()
		for i := range sess.Accounts {
			if sess.Accounts[i].ID == account.ID {
				sess.Accounts[i].CallAlertEnabled = newValue
			}
		}
		s.mu.Unlock()
		if newValue {
			s.prompt(sess, "Call alerts are enabled for this account. "+menuPrompt("account_settings"))
		} else {
			s.prompt(sess, "Call alerts are disabled for this account. "+menuPrompt("account_settings"))
		}
	case "2":
		s.openAccountAlertFolders(sess, account)
	}
}

func (s *Service) openAccountAlertFolders(sess *session, account store.Account) {
	folders, err := s.Store.ListMailFolders(s.baseContext(), sess.UserID, account.ID)
	if err != nil {
		s.prompt(sess, "Alert folders are temporarily unavailable.")
		return
	}
	seen := make(map[string]bool, len(folders))
	for _, folder := range folders {
		seen[folder] = true
	}
	var mapping map[string]string
	_ = json.Unmarshal([]byte(account.FolderMap), &mapping)
	for remote, local := range mapping {
		if local != "" && !seen[local] {
			folders = append(folders, local)
			seen[local] = true
		}
		if remote != "" && !seen[remote] {
			folders = append(folders, remote)
			seen[remote] = true
		}
	}
	sort.Strings(folders)
	selected := make(map[string]bool)
	var configured []string
	if json.Unmarshal([]byte(account.AlertFolders), &configured) == nil {
		for _, value := range configured {
			for _, folder := range folders {
				if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(folder)) {
					selected[folder] = true
				}
			}
		}
	}
	s.mu.Lock()
	sess.AlertFolders = folders
	sess.AlertFolderSelection = selected
	sess.AlertFolderPage = 0
	transitionLocked(sess, "account_alert_folders")
	s.mu.Unlock()
	s.promptAccountAlertFolders(sess)
}

func (s *Service) promptAccountAlertFolders(sess *session) {
	s.mu.Lock()
	page := sess.AlertFolderPage
	folders := append([]string(nil), sess.AlertFolders...)
	selected := make(map[string]bool, len(sess.AlertFolderSelection))
	for folder, value := range sess.AlertFolderSelection {
		selected[folder] = value
	}
	s.mu.Unlock()
	start := page * 8
	if start >= len(folders) {
		s.prompt(sess, "There are no synchronized folders. Press pound to return.")
		return
	}
	end := start + 8
	if end > len(folders) {
		end = len(folders)
	}
	parts := make([]string, 0, end-start)
	for i, folder := range folders[start:end] {
		state := "off"
		if selected[folder] {
			state = "on"
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s, currently %s", i+1, folder, state))
	}
	next := ""
	if end < len(folders) {
		next = " Press 9 for more."
	}
	s.prompt(sess, "Choose alert folders. "+strings.Join(parts, ". ")+"."+next+" Press pound when finished.")
}

func (s *Service) handleAccountAlertFolders(sess *session, key string) {
	if key == "9" {
		s.mu.Lock()
		if (sess.AlertFolderPage+1)*8 < len(sess.AlertFolders) {
			sess.AlertFolderPage++
		}
		s.mu.Unlock()
		s.promptAccountAlertFolders(sess)
		return
	}
	if key < "1" || key > "8" {
		return
	}
	s.mu.Lock()
	index := sess.AlertFolderPage*8 + int(key[0]-'1')
	if index < 0 || index >= len(sess.AlertFolders) {
		s.mu.Unlock()
		return
	}
	folder := sess.AlertFolders[index]
	sess.AlertFolderSelection[folder] = !sess.AlertFolderSelection[folder]
	var account store.Account
	for _, candidate := range sess.Accounts {
		if candidate.ID == sess.ActiveAccount {
			account = candidate
			break
		}
	}
	selected := make([]string, 0, len(sess.AlertFolderSelection))
	for candidate, enabled := range sess.AlertFolderSelection {
		if enabled {
			selected = append(selected, mappedFolder(account, candidate))
		}
	}
	selected = uniqueFolders(selected)
	accountID, userID := sess.ActiveAccount, sess.UserID
	s.mu.Unlock()
	sort.Strings(selected)
	data, err := json.Marshal(selected)
	if err == nil {
		err = s.Store.SetAccountAlertFolders(s.baseContext(), userID, accountID, selected)
	}
	if err != nil {
		s.prompt(sess, "The alert folders could not be changed.")
		return
	}
	s.mu.Lock()
	for i := range sess.Accounts {
		if sess.Accounts[i].ID == accountID {
			sess.Accounts[i].AlertFolders = string(data)
		}
	}
	s.mu.Unlock()
	s.promptAccountAlertFolders(sess)
}

func uniqueFolders(folders []string) []string {
	seen := make(map[string]struct{}, len(folders))
	out := make([]string, 0, len(folders))
	for _, folder := range folders {
		folder = strings.TrimSpace(folder)
		if folder == "" {
			continue
		}
		key := strings.ToLower(folder)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, folder)
	}
	return out
}

func (s *Service) refreshAccount(sess *session, accountID string) {
	s.mu.Lock()
	refresher := s.Refresher
	s.mu.Unlock()
	if refresher == nil {
		s.prompt(sess, "Manual refresh is unavailable. Press pound to return.")
		return
	}
	s.prompt(sess, "Refreshing this account now. Please wait.")
	s.startTask(func() {
		ctx, cancel := context.WithTimeout(s.baseContext(), 2*time.Minute)
		defer cancel()
		err := refresher.RefreshAccount(ctx, sess.UserID, accountID)
		if err != nil {
			s.prompt(sess, "The account refresh failed. Please try again later.")
			return
		}
		s.mu.Lock()
		var refreshed store.Account
		for _, candidate := range sess.Accounts {
			if candidate.ID == accountID {
				refreshed = candidate
				break
			}
		}
		currentState := string(stateLocked(sess))
		s.mu.Unlock()
		if currentState == "account_menu" && refreshed.ID != "" {
			s.prompt(sess, "The account refresh finished. "+s.accountMenuPrompt(sess, refreshed))
		} else {
			s.prompt(sess, "The account refresh finished.")
		}
	})
}

func (s *Service) openAccountFolders(sess *session, account store.Account) {
	folders, err := s.Store.ListMailFolders(s.baseContext(), sess.UserID, account.ID)
	if err != nil {
		s.prompt(sess, "Folders are temporarily unavailable.")
		return
	}
	seen := make(map[string]bool, len(folders))
	for _, folder := range folders {
		seen[folder] = true
	}
	var folderMap map[string]string
	if json.Unmarshal([]byte(account.FolderMap), &folderMap) == nil {
		for remote, local := range folderMap {
			if local != "" && !seen[local] {
				folders = append(folders, local)
				seen[local] = true
			}
			if remote != "" && !seen[remote] {
				folders = append(folders, remote)
				seen[remote] = true
			}
		}
	}
	sort.Strings(folders)
	s.mu.Lock()
	sess.Folders = folders
	sess.FolderPage = 0
	sess.FolderPrefix = ""
	sess.SelectedFolder = ""
	transitionLocked(sess, "folders")
	s.mu.Unlock()
	if len(folders) == 0 {
		s.prompt(sess, "This account has no synchronized folders.")
	} else {
		s.prompt(sess, folderPromptFor(account, folders, "", 0))
	}
}

func (s *Service) handleFolderMenu(sess *session, key string) {
	s.mu.Lock()
	folders := append([]string(nil), sess.Folders...)
	prefix, page, accountID := sess.FolderPrefix, sess.FolderPage, sess.ActiveAccount
	var account store.Account
	for _, candidate := range sess.Accounts {
		if candidate.ID == accountID {
			account = candidate
			break
		}
	}
	s.mu.Unlock()
	if key == "0" && prefix == "" {
		inbox, err := s.Store.InboxFolder(s.baseContext(), account)
		if err != nil {
			s.prompt(sess, "Inbox is temporarily unavailable.")
			return
		}
		s.listenFolder(sess, accountID, inbox)
		return
	}
	children := folderChildren(folders, prefix)
	if key == "9" {
		if (page+1)*8 < len(children) {
			s.mu.Lock()
			sess.FolderPage++
			page = sess.FolderPage
			s.mu.Unlock()
			s.prompt(sess, folderPromptFor(account, folders, prefix, page))
		}
		return
	}
	if key < "1" || key > "8" {
		return
	}
	index := page*8 + int(key[0]-'1')
	if index < 0 || index >= len(children) {
		return
	}
	s.mu.Lock()
	sess.SelectedFolder = children[index]
	transitionLocked(sess, "folder_action")
	s.mu.Unlock()
	s.prompt(sess, fmt.Sprintf("Folder %s. Press 1 to listen to messages here, or 2 to open nested folders. Press pound to return.", mappedFolder(account, children[index])))
}

func (s *Service) handleFolderAction(sess *session, key string) {
	s.mu.Lock()
	folder, accountID := sess.SelectedFolder, sess.ActiveAccount
	var account store.Account
	for _, candidate := range sess.Accounts {
		if candidate.ID == accountID {
			account = candidate
			break
		}
	}
	s.mu.Unlock()
	if folder == "" {
		return
	}
	switch key {
	case "1":
		s.listenFolder(sess, accountID, folder)
	case "2":
		s.mu.Lock()
		sess.FolderPrefix = folder
		sess.FolderPage = 0
		transitionLocked(sess, "folders")
		folders := append([]string(nil), sess.Folders...)
		s.mu.Unlock()
		if len(folderChildren(folders, folder)) == 0 {
			s.prompt(sess, "There are no nested folders here. Press 1 to listen to this folder, or pound to return.")
			return
		}
		s.prompt(sess, folderPromptFor(account, folders, folder, 0))
	}
}

func (s *Service) listenFolder(sess *session, accountID, folder string) {
	mails, err := s.Store.ListMailForAccount(s.baseContext(), sess.UserID, accountID, folder, false)
	if err != nil {
		s.prompt(sess, "Mail is temporarily unavailable.")
		return
	}
	s.mu.Lock()
	sess.Messages, sess.Cursor = mails, 0
	transitionLocked(sess, "list")
	s.mu.Unlock()
	if len(mails) == 0 {
		s.prompt(sess, "There are no messages in that folder.")
	} else {
		s.prompt(sess, listPrompt(mails[0], 0, len(mails)))
	}
}

func folderChildren(folders []string, prefix string) []string {
	prefix = strings.Trim(strings.ReplaceAll(prefix, "\\", "/"), "/")
	seen := make(map[string]bool)
	var out []string
	for _, raw := range folders {
		folder := strings.Trim(strings.ReplaceAll(raw, "\\", "/"), "/")
		if folder == "" || folder == prefix || (prefix != "" && !strings.HasPrefix(folder, prefix+"/")) {
			continue
		}
		rest := strings.TrimPrefix(folder, prefix)
		rest = strings.TrimPrefix(rest, "/")
		child := rest
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			child = strings.TrimSuffix(folder[:len(folder)-len(rest)+slash], "/")
		}
		if !seen[child] {
			seen[child] = true
			out = append(out, child)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Service) openContacts(sess *session) {
	contacts, err := s.Store.ListContacts(s.baseContext(), sess.UserID)
	if err != nil || len(contacts) == 0 {
		s.prompt(sess, "You have no contacts.")
		return
	}
	s.mu.Lock()
	sess.Contacts = contacts
	sess.ContactPage = 0
	transitionLocked(sess, "contacts")
	s.mu.Unlock()
	s.prompt(sess, contactPrompt(contacts, 0))
}

func (s *Service) handleContactMenu(sess *session, key string) {
	if key < "1" || key > "9" {
		return
	}
	s.mu.Lock()
	if key == "9" && len(sess.Contacts) > (sess.ContactPage+1)*8 {
		sess.ContactPage++
		page := sess.ContactPage
		contacts := append([]store.Contact(nil), sess.Contacts...)
		s.mu.Unlock()
		s.prompt(sess, contactPrompt(contacts, page))
		return
	}
	page := sess.ContactPage
	index := int(key[0] - '1')
	index += page * 8
	if key == "9" {
		s.mu.Unlock()
		return
	}
	if index >= len(sess.Contacts) {
		s.mu.Unlock()
		return
	}
	contact := sess.Contacts[index]
	s.mu.Unlock()
	s.startComposeTo(sess, contact.Email)
	s.prompt(sess, fmt.Sprintf("Composing to %s. %s", contact.Name, menuPrompt("subject_method")))
}

func (s *Service) handleRecipientMenu(sess *session, key string) {
	switch key {
	case "1", "2", "3":
		s.mu.Lock()
		transitionLocked(sess, "recipient_input")
		sess.RecipientKind = map[string]string{"1": "to", "2": "cc", "3": "bcc"}[key]
		sess.Editor = newIVREditor("recipient_input")
		s.mu.Unlock()
		s.prompt(sess, "Enter the email address using multi tap, then press pound.")
	case "4":
		s.mu.Lock()
		transitionLocked(sess, "subject_method")
		sess.RecipientKind = ""
		sess.Editor = nil
		s.mu.Unlock()
		s.prompt(sess, menuPrompt("subject_method"))
	}
}

func (s *Service) handleFieldMethod(sess *session, key, field string) {
	if key == "1" {
		s.mu.Lock()
		transitionLocked(sess, field)
		sess.Editor = newIVREditor(field)
		if field == "subject" {
			sess.Editor.Text = sess.Draft.Subject
		} else {
			sess.Editor.Text = sess.Draft.Body
		}
		s.mu.Unlock()
		s.prompt(sess, "Type with multi tap, then press pound.")
		return
	}
	if key == "2" {
		if field == "body" {
			s.startVoiceRecording(sess)
		} else {
			s.startSubjectVoice(sess)
		}
	}
}

func (s *Service) handleSettingsMenu(sess *session, key string) {
	switch key {
	case "1":
		s.prompt(sess, "Voice model and speech speed are managed in the web console.")
	case "2":
		if !s.alertsAvailable(sess) {
			s.prompt(sess, "Call alerts are disabled by the administrator.")
			return
		}
		enabled, err := s.Store.AlertsEnabled(s.baseContext(), sess.UserID)
		if err != nil {
			s.prompt(sess, "Alert settings are unavailable.")
			return
		}
		enabled = !enabled
		if err := s.Store.SetAlertsEnabled(s.baseContext(), sess.UserID, enabled); err != nil {
			s.prompt(sess, "Alert settings could not be changed.")
			return
		}
		if enabled {
			s.prompt(sess, "Call alerts are now enabled.")
		} else {
			s.prompt(sess, "Call alerts are now disabled.")
		}
	case "3":
		s.openContacts(sess)
	}
}

func accountPrompt(accounts []store.Account, page int) string {
	parts := make([]string, 0, len(accounts))
	start := page * 8
	for i := start; i < len(accounts); i++ {
		if i == start+8 {
			break
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s", i-start+1, accounts[i].CanonicalName))
	}
	next := ""
	if start+8 < len(accounts) {
		next = " Press 9 for more."
	}
	return "Accounts. " + strings.Join(parts, ". ") + "." + next + " Press pound to go back."
}

func (s *Service) accountMenuPrompt(sess *session, account store.Account) string {
	if !s.alertsAvailable(sess) {
		return fmt.Sprintf("%s. Press 1 to listen to mail, 2 to send an email, or 3 to refresh. Press pound to go back.", account.CanonicalName)
	}
	return fmt.Sprintf("%s. Press 1 to listen to mail, 2 to send an email, 3 to refresh, or 4 for account settings. Press pound to go back.", account.CanonicalName)
}

func folderPrompt(account store.Account, folders []string, page int) string {
	return folderPromptFor(account, folders, "", page)
}

func folderPromptFor(account store.Account, folders []string, prefix string, page int) string {
	children := folderChildren(folders, prefix)
	parts := make([]string, 0, len(folders))
	start := page * 8
	for i := start; i < len(children); i++ {
		if i == start+8 {
			break
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s", i-start+1, mappedFolder(account, children[i])))
	}
	next := ""
	if start+8 < len(children) {
		next = " Press 9 for more."
	}
	where := "root"
	if prefix != "" {
		where = mappedFolder(account, prefix)
	}
	rootOption := ""
	if prefix == "" {
		rootOption = " Press 0 for Inbox at the root."
	}
	return "Folders in " + where + " for " + account.CanonicalName + ". " + strings.Join(parts, ". ") + "." + next + rootOption + " Press pound to go back."
}

func contactPrompt(contacts []store.Contact, page int) string {
	parts := make([]string, 0, len(contacts))
	start := page * 8
	for i := start; i < len(contacts); i++ {
		if i == start+8 {
			break
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s", i-start+1, contacts[i].Name))
	}
	next := ""
	if start+8 < len(contacts) {
		next = " Press 9 for more."
	}
	return "Contacts. " + strings.Join(parts, ". ") + "." + next + " Press pound to go back."
}

func recipientPrompt(d draft) string {
	toCount := 0
	if strings.TrimSpace(d.To) != "" {
		toCount = 1 + len(d.AdditionalTo)
	}
	return fmt.Sprintf("You have %d To recipient(s), %d Cc, and %d Bcc. Press 1 to add To, 2 to add Cc, 3 to add Bcc, or 4 to continue.", toCount, len(d.Cc), len(d.Bcc))
}

func mappedFolder(account store.Account, folder string) string {
	var mapping map[string]string
	if json.Unmarshal([]byte(account.FolderMap), &mapping) == nil {
		if local := mapping[folder]; local != "" {
			return local
		}
		for remote, local := range mapping {
			if local == folder && remote != "" {
				return local
			}
		}
	}
	return folder
}

func remoteFolder(account store.Account, local string) string {
	var mapping map[string]string
	if json.Unmarshal([]byte(account.FolderMap), &mapping) == nil {
		for remote, mapped := range mapping {
			if remote == local || mapped == local {
				return remote
			}
		}
	}
	return local
}
