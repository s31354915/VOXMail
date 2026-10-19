package calls

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	if err != nil {
		s.prompt(sess, "Your mail accounts are temporarily unavailable. Please try again in a moment.")
		return
	}
	if len(accounts) == 0 {
		s.prompt(sess, "No mail accounts are configured. Add an account in the web console, or ask your administrator. "+menuPrompt("main"))
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
		s.prompt(sess, "No messages in "+description+". Press pound to go back.")
	} else {
		s.prompt(sess, listPrompt(mails[0], 0, len(mails)))
	}
}

func (s *Service) handleAccountMenu(sess *session, key string) {
	if key < "1" || key > "9" {
		s.prompt(sess, "That key is not available here. Press pound to go back.")
		return
	}
	s.mu.Lock()
	if key == "9" {
		if len(sess.Accounts) > (sess.AccountPage+1)*8 {
			sess.AccountPage++
			page := sess.AccountPage
			accounts := append([]store.Account(nil), sess.Accounts...)
			s.mu.Unlock()
			s.prompt(sess, "Email. Press 0 for all unread mail. "+accountPrompt(accounts, page))
		} else {
			s.mu.Unlock()
			s.prompt(sess, "There are no more accounts. Press pound to go back.")
		}
		return
	}
	page := sess.AccountPage
	index := page*8 + int(key[0]-'1')
	if index >= len(sess.Accounts) {
		s.mu.Unlock()
		s.prompt(sess, "There is no account for that key. Press pound to go back.")
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
		s.prompt(sess, menuPrompt("compose"))
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
		sess.PendingAttachmentRemove = false
		transitionLocked(sess, "attachment_edit")
	default:
		s.mu.Unlock()
		s.prompt(sess, "That key is not available here. Press pound to go back.")
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
		s.prompt(sess, "That key is not available here. Press pound to go back.")
		return
	}
	state := string(stateLocked(sess))
	d := sess.Draft
	s.mu.Unlock()
	if state == "compose" {
		s.prompt(sess, "Replace the main recipient. "+menuPrompt("compose"))
	} else if state == "recipient_menu" {
		s.prompt(sess, recipientPrompt(d))
	} else {
		s.prompt(sess, "Recipient changes applied. "+menuPrompt("draft_edit"))
	}
}

func (s *Service) handleAttachmentEdit(sess *session, key string) {
	switch key {
	case "1":
		s.mu.Lock()
		sess.PendingAttachmentRemove = false
		s.mu.Unlock()
		s.startAudioAttachment(sess)
	case "2", "3":
		s.mu.Lock()
		hasAttachment := len(sess.Draft.Attachments) > 0
		pendingRemove := sess.PendingAttachmentRemove
		if !hasAttachment {
			sess.PendingAttachmentRemove = false
		}
		count := len(sess.Draft.Attachments)
		s.mu.Unlock()
		if !hasAttachment {
			s.prompt(sess, "There is no attachment to change. Press 1 to add one, or pound to return.")
			return
		}
		if key == "3" {
			s.mu.Lock()
			sess.PendingAttachmentRemove = false
			s.mu.Unlock()
			s.startAudioAttachment(sess)
			return
		}
		// Removing an attachment used to be instant and irreversible. The
		// first press of 2 now asks for confirmation; the second removes.
		if !pendingRemove {
			s.mu.Lock()
			sess.PendingAttachmentRemove = true
			s.mu.Unlock()
			s.prompt(sess, fmt.Sprintf("Press 2 again to remove the last attachment, or pound to keep it. You have %d.", count))
			return
		}
		s.mu.Lock()
		sess.Draft.Attachments = sess.Draft.Attachments[:len(sess.Draft.Attachments)-1]
		sess.PendingAttachmentRemove = false
		count = len(sess.Draft.Attachments)
		s.mu.Unlock()
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
		more := (sess.DraftPage+1)*8 < len(sess.Drafts)
		if more {
			sess.DraftPage++
		}
		s.mu.Unlock()
		if more {
			s.promptDraftList(sess)
		} else {
			s.prompt(sess, "There are no more saved drafts. Press pound to go back.")
		}
		return
	}
	if key < "1" || key > "8" {
		s.prompt(sess, "That key is not available here. Press pound to go back.")
		return
	}
	s.mu.Lock()
	index := sess.DraftPage*8 + int(key[0]-'1')
	if index < 0 || index >= len(sess.Drafts) {
		s.mu.Unlock()
		s.prompt(sess, "There is no saved draft for that key. Press pound to go back.")
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
	// A missing, oversized, or unreadable attachment must not abort the whole
	// resume: skip it, say so, and let the caller keep working the draft. A
	// per-file cap and an aggregate cap keep a large draft from exhausting
	// memory on the call path.
	const maxResumeAttachmentBytes = 25 << 20
	const maxResumeAttachmentsTotal = 50 << 20
	attachments := make([]mailer.Attachment, 0, len(record.Attachments))
	skippedAttachments := 0
	var loadedAttachmentBytes int64
	for _, saved := range record.Attachments {
		path, ok := s.safeDraftAttachmentPath(sess, saved.Path)
		if !ok {
			skippedAttachments++
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxResumeAttachmentBytes || loadedAttachmentBytes+info.Size() > maxResumeAttachmentsTotal {
			skippedAttachments++
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			skippedAttachments++
			continue
		}
		loadedAttachmentBytes += int64(len(data))
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
	resumed := "Draft resumed. "
	if skippedAttachments > 0 {
		resumed = fmt.Sprintf("Draft resumed, but %d of its attachments could not be loaded and were left out. ", skippedAttachments)
	}
	s.prompt(sess, resumed+menuPrompt(state))
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
		current, next := "off", "on"
		if newValue {
			current, next = "on", "off"
		}
		s.prompt(sess, fmt.Sprintf("Call alerts were %s for this account. They are now %s. %s", current, next, menuPrompt("account_settings")))
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
		more := (sess.AlertFolderPage+1)*8 < len(sess.AlertFolders)
		if more {
			sess.AlertFolderPage++
		}
		s.mu.Unlock()
		if more {
			s.promptAccountAlertFolders(sess)
		} else {
			s.prompt(sess, "There are no more folders. Press pound when finished.")
		}
		return
	}
	if key < "1" || key > "8" {
		s.prompt(sess, "That key is not available here. Press pound when finished.")
		return
	}
	s.mu.Lock()
	index := sess.AlertFolderPage*8 + int(key[0]-'1')
	if index < 0 || index >= len(sess.AlertFolders) {
		s.mu.Unlock()
		s.prompt(sess, "There is no folder for that key. Press pound when finished.")
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
		done := make(chan struct{})
		defer close(done)
		go func() {
			// Long refreshes used to leave the caller in silence; reassure
			// them periodically until the refresh finishes.
			ticker := time.NewTicker(20 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					s.prompt(sess, "Still refreshing. Please wait.")
				}
			}
		}()
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
	ctx := s.baseContext()
	mailboxes, err := s.Store.ListLogicalMailboxes(ctx, sess.UserID, account.ID)
	if err != nil {
		s.prompt(sess, "Folders are temporarily unavailable.")
		return
	}
	allFolders := make([]string, 0, len(mailboxes))
	labels, physicalKeys := map[string]string{}, map[string]string{}
	for _, mailbox := range mailboxes {
		allFolders = append(allFolders, mailbox.Key)
		labels[mailbox.Key] = mailbox.Name
		for _, member := range mailbox.Members {
			physicalKeys[member] = mailbox.Key
		}
	}
	rolePaths := map[string]string{}
	inbox, err := s.Store.InboxFolder(ctx, account)
	if err != nil {
		s.prompt(sess, "Folders are temporarily unavailable.")
		return
	}
	rolePaths["inbox"] = physicalKeys[normalizeFolderPath(inbox)]
	roles, err := s.Store.ListFolderRoles(ctx, sess.UserID, account.ID)
	if err != nil {
		s.prompt(sess, "Folder settings are temporarily unavailable.")
		return
	}
	assignedRoles := map[string]bool{}
	for _, role := range roles {
		name := strings.ToLower(strings.TrimSpace(role.Role))
		assignedRoles[name] = true
		resolved, roleErr := s.Store.FolderRole(ctx, account, name)
		if errors.Is(roleErr, sql.ErrNoRows) {
			rolePaths[name] = ""
			continue
		}
		if roleErr != nil {
			s.prompt(sess, "Folder settings are temporarily unavailable.")
			return
		}
		rolePaths[name] = physicalKeys[normalizeFolderPath(resolved.LocalPath)]
	}
	for _, role := range []string{"drafts", "sent", "trash"} {
		if rolePaths[role] != "" || assignedRoles[role] {
			continue
		}
		resolved, roleErr := s.Store.FolderRole(ctx, account, role)
		if roleErr == nil {
			rolePaths[role] = physicalKeys[normalizeFolderPath(resolved.LocalPath)]
			continue
		}
		if errors.Is(roleErr, store.ErrInvalidFolderRole) {
			// Multiple case-distinct conventional remotes need an owner choice.
			continue
		}
		if !errors.Is(roleErr, sql.ErrNoRows) {
			s.prompt(sess, "Folder settings are temporarily unavailable.")
			return
		}
		candidate, matches := "", 0
		for folder, key := range physicalKeys {
			if strings.EqualFold(folder, role) {
				candidate = key
				matches++
			}
		}
		if matches == 1 {
			rolePaths[role] = candidate
		}
	}
	s.mu.Lock()
	sess.Folders = allFolders
	sess.FolderRolePaths = rolePaths
	sess.FolderLabels = labels
	sess.FolderPage = 0
	sess.FolderPrefix = ""
	sess.SelectedFolder = ""
	transitionLocked(sess, "folders")
	s.mu.Unlock()
	s.prompt(sess, folderPromptFor(account, allFolders, rolePaths, "", 0, labels))
}

func (s *Service) handleFolderMenu(sess *session, key string) {
	s.mu.Lock()
	labels := copyFolderLabels(sess.FolderLabels)
	folders := append([]string(nil), sess.Folders...)
	rolePaths := make(map[string]string, len(sess.FolderRolePaths))
	for role, folder := range sess.FolderRolePaths {
		rolePaths[role] = folder
	}
	prefix, page, accountID := sess.FolderPrefix, sess.FolderPage, sess.ActiveAccount
	var account store.Account
	for _, candidate := range sess.Accounts {
		if candidate.ID == accountID {
			account = candidate
			break
		}
	}
	s.mu.Unlock()
	children := folderMenuChildren(folders, rolePaths, prefix, labels)
	if key == "9" {
		if folderPageHasMore(len(children), prefix == "", page) {
			s.mu.Lock()
			sess.FolderPage++
			page = sess.FolderPage
			s.mu.Unlock()
			s.prompt(sess, folderPromptFor(account, folders, rolePaths, prefix, page, labels))
		} else {
			s.prompt(sess, "There are no more folders. Press pound to go back.")
		}
		return
	}
	if prefix == "" && page == 0 && key >= "1" && key <= "4" {
		role, label := standardFolderRole(key)
		folder := rolePaths[role]
		if folder == "" {
			s.prompt(sess, label+" is not configured for this account. "+folderPromptFor(account, folders, rolePaths, prefix, page, labels))
			return
		}
		s.mu.Lock()
		sess.SelectedFolder = folder
		s.mu.Unlock()
		s.listenFolder(sess, accountID, folder)
		return
	}
	if key < "1" || key > "8" {
		s.prompt(sess, "That key is not available here. Press pound to go back.")
		return
	}
	index := page*8 + int(key[0]-'1')
	if prefix == "" {
		if page == 0 {
			if key < "5" {
				return
			}
			index = int(key[0] - '5')
		} else {
			index = 4 + (page-1)*8 + int(key[0]-'1')
		}
	}
	if index < 0 || index >= len(children) {
		s.prompt(sess, "There is no folder for that key. Press pound to go back.")
		return
	}
	s.mu.Lock()
	sess.SelectedFolder = children[index]
	transitionLocked(sess, "folder_action")
	s.mu.Unlock()
	s.prompt(sess, fmt.Sprintf("Folder %s. Press 1 to listen to messages here, or 2 to open nested folders. Press pound to return.", folderLabel(account, children[index], labels)))
}

func (s *Service) handleFolderAction(sess *session, key string) {
	s.mu.Lock()
	labels := copyFolderLabels(sess.FolderLabels)
	folder, accountID := sess.SelectedFolder, sess.ActiveAccount
	folders := append([]string(nil), sess.Folders...)
	rolePaths := make(map[string]string, len(sess.FolderRolePaths))
	for role, folder := range sess.FolderRolePaths {
		rolePaths[role] = folder
	}
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
		s.mu.Unlock()
		if len(folderChildren(folders, folder)) == 0 {
			s.prompt(sess, "There are no nested folders here. Press 1 to listen to this folder, or pound to return.")
			return
		}
		s.prompt(sess, folderPromptFor(account, folders, rolePaths, folder, 0, labels))
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
		s.prompt(sess, "There are no messages in that folder. Press pound to choose another folder.")
	} else {
		s.prompt(sess, listPrompt(mails[0], 0, len(mails)))
	}
}

func folderChildren(folders []string, prefix string) []string {
	prefix = normalizeFolderPath(prefix)
	seen := make(map[string]bool)
	var out []string
	for _, raw := range folders {
		folder := normalizeFolderPath(raw)
		if strings.HasPrefix(folder, "/groups/") {
			if prefix == "" && !seen[folder] {
				seen[folder] = true
				out = append(out, folder)
			}
			continue
		}
		if folder == "" || folder == prefix || (prefix != "" && !strings.HasPrefix(folder, prefix+"/")) {
			continue
		}
		rest := strings.TrimPrefix(folder, prefix)
		rest = strings.TrimPrefix(rest, "/")
		// A direct child still needs its complete physical path. Returning
		// only the basename would query "2026" instead of "Projects/2026".
		child := folder
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
	if err != nil {
		s.prompt(sess, "Your contacts are temporarily unavailable. Please try again in a moment.")
		return
	}
	if len(contacts) == 0 {
		s.prompt(sess, "You have no contacts yet. To save a sender, open their message, press 9, then 1. You can also add contacts in the web console. Press pound to go back.")
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
		s.prompt(sess, "That key is not available here. Press pound to go back.")
		return
	}
	s.mu.Lock()
	if key == "9" {
		if len(sess.Contacts) > (sess.ContactPage+1)*8 {
			sess.ContactPage++
			page := sess.ContactPage
			contacts := append([]store.Contact(nil), sess.Contacts...)
			s.mu.Unlock()
			s.prompt(sess, contactPrompt(contacts, page))
		} else {
			s.mu.Unlock()
			s.prompt(sess, "There are no more contacts. Press pound to go back.")
		}
		return
	}
	page := sess.ContactPage
	index := page*8 + int(key[0]-'1')
	if index >= len(sess.Contacts) {
		s.mu.Unlock()
		s.prompt(sess, "There is no contact for that key. Press pound to go back.")
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
		s.prompt(sess, menuPrompt("recipient_input"))
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
		s.prompt(sess, menuPrompt(field))
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
		s.prompt(sess, "Change your voice and speech speed in the web console. "+s.settingsPrompt(sess))
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

func standardFolderRole(key string) (string, string) {
	switch key {
	case "1":
		return "inbox", "Inbox"
	case "2":
		return "drafts", "Drafts"
	case "3":
		return "sent", "Sent"
	case "4":
		return "trash", "Trash"
	default:
		return "", ""
	}
}

func normalizeFolderPath(folder string) string {
	if strings.HasPrefix(folder, "/groups/") {
		return folder
	}
	return strings.Trim(strings.ReplaceAll(strings.TrimSpace(folder), "\\", "/"), "/")
}

func folderMenuChildren(folders []string, rolePaths map[string]string, prefix string, labels ...map[string]string) []string {
	children := folderChildren(folders, prefix)
	if len(labels) > 0 {
		sort.Slice(children, func(i, j int) bool {
			return folderLabel(store.Account{}, children[i], labels...) < folderLabel(store.Account{}, children[j], labels...)
		})
	}
	if prefix != "" {
		return children
	}
	standard := make(map[string]bool, len(rolePaths))
	for _, role := range []string{"inbox", "drafts", "sent", "trash"} {
		folder := rolePaths[role]
		if folder = normalizeFolderPath(folder); folder != "" {
			standard[folder] = true
		}
	}
	others := make([]string, 0, len(children))
	for _, folder := range children {
		if !standard[normalizeFolderPath(folder)] {
			others = append(others, folder)
		}
	}
	return others
}

func folderPageHasMore(count int, root bool, page int) bool {
	start := page * 8
	if root {
		if page == 0 {
			return 4 < count
		} else {
			start = 4 + (page-1)*8
		}
	}
	return start+8 < count
}

func folderPromptFor(account store.Account, folders []string, rolePaths map[string]string, prefix string, page int, labels ...map[string]string) string {
	children := folderMenuChildren(folders, rolePaths, prefix, labels...)
	parts := make([]string, 0, 12)
	where := "root"
	if prefix != "" {
		start := page * 8
		end := min(start+8, len(children))
		for i := start; i < end; i++ {
			parts = append(parts, fmt.Sprintf("Press %d for %s", i-start+1, folderLabel(account, children[i], labels...)))
		}
		where = folderLabel(account, prefix, labels...)
		if end < len(children) {
			parts = append(parts, "Press 9 for more")
		}
		if len(parts) == 0 {
			parts = append(parts, "There are no nested folders")
		}
		return "Folders in " + where + " for " + account.CanonicalName + ". " + strings.Join(parts, ". ") + ". Press pound to go back."
	}

	start, keyStart := 0, 5
	if page > 0 {
		start, keyStart = 4+(page-1)*8, 1
		parts = append(parts, "More folders")
	} else {
		parts = append(parts,
			"Press 1 for Inbox",
			"Press 2 for Drafts",
			"Press 3 for Sent",
			"Press 4 for Trash",
		)
	}
	end := min(start+8, len(children))
	if page == 0 {
		end = min(4, len(children))
	}
	for i := start; i < end; i++ {
		parts = append(parts, fmt.Sprintf("Press %d for %s", keyStart+i-start, folderLabel(account, children[i], labels...)))
	}
	if end < len(children) {
		parts = append(parts, "Press 9 for more folders")
	} else if len(children) == 0 {
		parts = append(parts, "There are no other folders")
	}
	return "Folders for " + account.CanonicalName + ". " + strings.Join(parts, ". ") + ". Press pound to go back."
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
	return fmt.Sprintf("Recipients: %d main, %d copy, and %d hidden copy. %s", toCount, len(d.Cc), len(d.Bcc), menuPrompt("recipient_menu"))
}

func copyFolderLabels(labels map[string]string) map[string]string {
	copy := make(map[string]string, len(labels))
	for key, label := range labels {
		copy[key] = label
	}
	return copy
}

func folderLabel(account store.Account, folder string, labels ...map[string]string) string {
	if len(labels) > 0 && labels[0][folder] != "" {
		return labels[0][folder]
	}
	return mappedFolder(account, folder)
}

func mappedFolder(account store.Account, folder string) string {
	// FolderMap maps remote names to local names. A folder that is a remote
	// key resolves to its local name; anything else is already a local name
	// and passes through unchanged. The former reverse-mapping branch
	// compared the folder against mapped values and returned the folder
	// itself, which was dead code.
	var mapping map[string]string
	if json.Unmarshal([]byte(account.FolderMap), &mapping) == nil {
		if local := mapping[folder]; local != "" {
			return local
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
