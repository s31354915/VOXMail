package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/bridge"
	remoteimap "github.com/voxmail/voxmail/internal/imap"
	"github.com/voxmail/voxmail/internal/lifecycle"
	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/netguard"
	"github.com/voxmail/voxmail/internal/store"
)

func (s *Service) savePendingContact(sess *session) {
	s.mu.Lock()
	name, email := sess.PendingContactName, sess.PendingContactEmail
	s.mu.Unlock()
	if _, err := s.Store.AddContact(s.baseContext(), store.Contact{UserID: sess.UserID, Name: name, Email: email}); err != nil {
		s.prompt(sess, "That contact could not be saved. It may already exist.")
		return
	}
	s.mu.Lock()
	transitionLocked(sess, "more_options")
	sess.PendingContactName = ""
	sess.PendingContactEmail = ""
	s.mu.Unlock()
	s.prompt(sess, "Contact saved. "+menuPrompt("more_options"))
}

func (s *Service) readMessage(sess *session, m store.MailSummary) {
	file, err := os.Open(m.Path)
	if err != nil {
		s.prompt(sess, "That message is no longer available.")
		return
	}
	parsed, err := mailparse.Parse(file)
	_ = file.Close()
	if err != nil {
		s.prompt(sess, "I could not read that message.")
		return
	}
	if _, localErr := s.queueLocalReadState(sess, m, true); localErr != nil {
		s.prompt(sess, "The local mail cache could not be updated.")
		return
	}
	body := parsed.Text
	if strings.TrimSpace(body) == "" {
		body = "There is no readable email body."
	}
	s.mu.Lock()
	sess.CurrentAttachments = append([]mailparse.Attachment(nil), parsed.Attachments...)
	sess.AttachmentPage = 0
	s.mu.Unlock()
	if len(parsed.Attachments) > 0 {
		descriptions := make([]string, 0, len(parsed.Attachments))
		playable := 0
		for _, attachment := range parsed.Attachments {
			kind := attachment.ContentType
			if kind == "" {
				kind = "unknown file type"
			}
			status := "not playable"
			if attachment.Playable {
				status = "playable"
				playable++
			}
			descriptions = append(descriptions, fmt.Sprintf("%s, %s, %s", attachment.Name, kind, status))
		}
		body += fmt.Sprintf(" There are %d attachments: %s.", len(parsed.Attachments), strings.Join(descriptions, ". "))
		if playable > 0 {
			body += " Press 8 to listen to playable attachments."
		} else {
			body += " There are no playable attachments."
		}
	}
	if len([]rune(body)) > 2800 {
		body = string([]rune(body)[:2800]) + ". Message truncated."
	}
	opening := "Email from"
	if !m.Read {
		opening = "New unread email from"
	}
	s.prompt(sess, fmt.Sprintf("%s %s. Subject %s. %s %s", opening, parsed.From, parsed.Subject, body, menuPrompt("read")))
}

func (s *Service) attachmentMenu(sess *session) {
	s.mu.Lock()
	attachments := append([]mailparse.Attachment(nil), sess.CurrentAttachments...)
	page := sess.AttachmentPage
	s.mu.Unlock()
	playable := make([]mailparse.Attachment, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment.Playable && len(attachment.Data) > 0 {
			playable = append(playable, attachment)
		}
	}
	if len(playable) == 0 {
		s.mu.Lock()
		transitionLocked(sess, "read")
		s.mu.Unlock()
		s.prompt(sess, "There are no playable attachments.")
		return
	}
	start := page * 9
	if start >= len(playable) {
		page = 0
		start = 0
	}
	end := start + 9
	if end > len(playable) {
		end = len(playable)
	}
	parts := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		kind := "audio"
		if strings.HasPrefix(strings.ToLower(playable[i].ContentType), "video/") {
			kind = "video audio"
		}
		parts = append(parts, fmt.Sprintf("Press %d for %s, %s", i-start+1, playable[i].Name, kind))
	}
	extra := ""
	if end < len(playable) {
		extra = " Press 0 for more."
	}
	s.mu.Lock()
	transitionLocked(sess, "attachment_menu")
	sess.AttachmentPage = page
	s.mu.Unlock()
	s.prompt(sess, fmt.Sprintf("There are %d playable attachments. %s.%s Press pound to return.", len(playable), strings.Join(parts, ". "), extra))
}

func (s *Service) playAttachment(sess *session, index int) {
	s.mu.Lock()
	attachments := append([]mailparse.Attachment(nil), sess.CurrentAttachments...)
	page := sess.AttachmentPage
	s.mu.Unlock()
	playable := make([]mailparse.Attachment, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment.Playable && len(attachment.Data) > 0 {
			playable = append(playable, attachment)
		}
	}
	position := page*9 + index
	if position < 0 || position >= len(playable) || len(playable[position].Data) > 50<<20 {
		s.prompt(sess, "That attachment cannot be played.")
		return
	}
	if s.Media == nil || sess.TxPath == "" {
		s.prompt(sess, "Audio playback is unavailable.")
		return
	}
	dir := s.DataRoot
	if dir == "" {
		dir = filepath.Dir(sess.TxPath)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		s.prompt(sess, "I could not prepare that attachment.")
		return
	}
	tmp, err := os.CreateTemp(dir, ".attachment-*")
	if err != nil {
		s.prompt(sess, "I could not prepare that attachment.")
		return
	}
	path := tmp.Name()
	if _, err := tmp.Write(playable[position].Data); err == nil {
		err = tmp.Close()
	} else {
		_ = tmp.Close()
	}
	defer os.Remove(path)
	if err != nil {
		s.prompt(sess, "I could not prepare that attachment.")
		return
	}
	ctx, cancel := context.WithTimeout(s.baseContext(), 10*time.Minute)
	s.mu.Lock()
	sess.AttachmentCancel = cancel
	s.mu.Unlock()
	err = s.Media.PlayMedia(ctx, sess.TxPath, path)
	cancel()
	s.mu.Lock()
	wasPlaying := string(stateLocked(sess)) == "attachment_playback"
	sess.AttachmentCancel = nil
	if wasPlaying {
		transitionLocked(sess, "read")
	}
	s.mu.Unlock()
	if wasPlaying {
		if err != nil && ctx.Err() == nil {
			s.prompt(sess, "Attachment playback failed.")
		} else {
			s.prompt(sess, "Attachment playback finished. Press 8 to listen again, or pound to return.")
		}
	}
}

func (s *Service) openMoveMenu(sess *session, message store.MailSummary) {
	accounts, err := s.Store.ListAccounts(s.baseContext(), sess.UserID)
	if err != nil {
		s.prompt(sess, "Folders are temporarily unavailable.")
		return
	}
	var account store.Account
	for _, candidate := range accounts {
		if candidate.ID == message.AccountID {
			account = candidate
			break
		}
	}
	if account.ID == "" {
		s.prompt(sess, "The message account is unavailable.")
		return
	}
	folders, err := s.Store.ListMailFolders(s.baseContext(), sess.UserID, account.ID)
	if err != nil {
		s.prompt(sess, "Folders are temporarily unavailable.")
		return
	}
	seen := make(map[string]bool, len(folders))
	for _, folder := range folders {
		seen[folder] = true
	}
	var mapping map[string]string
	_ = json.Unmarshal([]byte(account.FolderMap), &mapping)
	for _, local := range mapping {
		if local != "" && !seen[local] {
			folders = append(folders, local)
			seen[local] = true
		}
	}
	sort.Strings(folders)
	if len(folders) == 0 {
		s.prompt(sess, "There are no destination folders.")
		return
	}
	s.mu.Lock()
	sess.MoveFolders = folders
	sess.MovePage = 0
	transitionLocked(sess, "move_menu")
	s.mu.Unlock()
	s.promptMoveMenu(sess)
}

func (s *Service) promptMoveMenu(sess *session) {
	s.mu.Lock()
	page := sess.MovePage
	folders := append([]string(nil), sess.MoveFolders...)
	s.mu.Unlock()
	start := page * 9
	if start >= len(folders) {
		return
	}
	end := start + 9
	if end > len(folders) {
		end = len(folders)
	}
	parts := make([]string, 0, end-start)
	for i, folder := range folders[start:end] {
		parts = append(parts, fmt.Sprintf("Press %d for %s", i+1, folder))
	}
	next := ""
	if end < len(folders) {
		next = " Press 0 for more folders."
	}
	s.prompt(sess, "Move message. "+strings.Join(parts, ". ")+"."+next+" Press pound to return.")
}

func (s *Service) moveCurrent(sess *session, destination string) {
	s.mu.Lock()
	if sess.Cursor < 0 || sess.Cursor >= len(sess.Messages) {
		s.mu.Unlock()
		return
	}
	message := sess.Messages[sess.Cursor]
	s.mu.Unlock()
	cleanDestination := filepath.Clean(filepath.FromSlash(destination))
	if cleanDestination == "." || filepath.IsAbs(cleanDestination) || strings.HasPrefix(cleanDestination, ".."+string(filepath.Separator)) || cleanDestination == ".." {
		s.prompt(sess, "That destination is not allowed.")
		return
	}
	if err := s.moveRemote(sess, message, destination); err != nil {
		status := "failed"
		prompt := "I could not move that message on the mail server."
		if errors.Is(err, remoteimap.ErrMutationUncertain) {
			status = "uncertain"
			prompt = "The mail server did not confirm the move. The next sync will reconcile it."
		}
		s.recordMutation(sess, message, "move", message.Folder, destination, status, err)
		s.prompt(sess, prompt)
		return
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(message.Path)))
	targetDir := filepath.Join(root, cleanDestination, "cur")
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		s.recordMutation(sess, message, "move", message.Folder, destination, "partial", err)
		_ = s.rollbackRemoteMove(sess, message, destination, message.Folder)
		s.prompt(sess, "The local mail cache could not be updated.")
		return
	}
	target := filepath.Join(targetDir, filepath.Base(message.Path))
	if err := os.Rename(message.Path, target); err != nil {
		s.recordMutation(sess, message, "move", message.Folder, destination, "partial", err)
		_ = s.rollbackRemoteMove(sess, message, destination, message.Folder)
		s.prompt(sess, "The local mail cache could not be updated.")
		return
	}
	if err := s.Store.MoveMailIndex(s.baseContext(), sess.UserID, message.ID, cleanDestination, target); err != nil {
		_ = os.Rename(target, message.Path)
		s.recordMutation(sess, message, "move", message.Folder, destination, "partial", err)
		_ = s.rollbackRemoteMove(sess, message, destination, message.Folder)
		s.prompt(sess, "The message moved, but the local index could not be updated.")
		return
	}
	s.recordMutation(sess, message, "move", message.Folder, destination, "success", nil)
	s.mu.Lock()
	sess.Messages[sess.Cursor].Folder = cleanDestination
	sess.Messages[sess.Cursor].Path = target
	transitionLocked(sess, "read")
	s.mu.Unlock()
	s.prompt(sess, "Message moved.")
}

func (s *Service) toggleRead(sess *session, message store.MailSummary) {
	read := !message.Read
	if _, err := s.queueLocalReadState(sess, message, read); err != nil {
		s.prompt(sess, "The local read state could not be updated.")
		return
	}
	if read {
		s.prompt(sess, "Message marked read locally and queued for synchronization.")
	} else {
		s.prompt(sess, "Message marked unread locally and queued for synchronization.")
	}
}

// queueLocalReadState is deliberately Maildir-first. mbsync's bidirectional
// Sync All propagates the S flag on its next run, while the renamed local file
// and SQLite path keep the cache internally consistent immediately. Direct
// IMAP is reserved for operations that Maildir cannot represent safely, such
// as a cross-folder move.
func (s *Service) queueLocalReadState(sess *session, message store.MailSummary, read bool) (string, error) {
	localPath, err := setMaildirRead(message.Path, read)
	if err != nil {
		s.recordMutation(sess, message, "mark_read", message.Folder, message.Folder, "failed", err)
		return "", err
	}
	if err := s.Store.MarkMailReadAtPath(s.baseContext(), sess.UserID, message.ID, read, localPath); err != nil {
		if localPath != message.Path {
			_ = os.Rename(localPath, message.Path)
		}
		s.recordMutation(sess, message, "mark_read", message.Folder, message.Folder, "failed", err)
		return "", err
	}
	s.recordMutation(sess, message, "mark_read", message.Folder, message.Folder, "queued", nil)
	s.mu.Lock()
	for i := range sess.Messages {
		if sess.Messages[i].ID == message.ID {
			sess.Messages[i].Read = read
			sess.Messages[i].Path = localPath
		}
	}
	s.mu.Unlock()
	return localPath, nil
}

func (s *Service) deleteCurrent(sess *session) {
	s.mu.Lock()
	if len(sess.Messages) == 0 {
		s.mu.Unlock()
		return
	}
	m := sess.Messages[sess.Cursor]
	s.mu.Unlock()
	accountRoot := ""
	if s.DataRoot != "" {
		accountRoot = filepath.Join(s.DataRoot, "mail", m.AccountID)
	} else {
		accountRoot = filepath.Dir(filepath.Dir(filepath.Dir(m.Path)))
	}
	rootAbs, rootErr := filepath.Abs(accountRoot)
	pathAbs, pathErr := filepath.Abs(m.Path)
	if rootErr != nil || pathErr != nil || (pathAbs != rootAbs && !strings.HasPrefix(pathAbs, rootAbs+string(filepath.Separator))) {
		s.prompt(sess, "I could not delete that message.")
		return
	}
	trashName := ""
	trashRemote := ""
	if accounts, err := s.Store.ListAccounts(s.baseContext(), sess.UserID); err == nil {
		for _, account := range accounts {
			if account.ID != m.AccountID {
				continue
			}
			if role, roleErr := s.Store.FolderRole(s.baseContext(), account, "trash"); roleErr == nil {
				trashName, trashRemote = role.LocalPath, role.RemotePath
			}
		}
	}
	if trashRemote == "" || trashName == "" {
		s.prompt(sess, "A Trash folder has not been mapped for this account.")
		return
	}
	if err := s.moveRemote(sess, m, trashRemote); err != nil {
		status := "failed"
		if errors.Is(err, remoteimap.ErrMutationUncertain) {
			status = "uncertain"
		}
		s.recordMutation(sess, m, "delete", m.Folder, trashName, status, err)
		if s.Log != nil {
			s.Log.Warn("could not move remote message to trash", "message_id", m.ID, "error", err)
		}
		if status == "uncertain" {
			s.prompt(sess, "The mail server did not confirm the Trash move. The next sync will reconcile it.")
		} else {
			s.prompt(sess, "I could not move that message to the remote Trash folder.")
		}
		return
	}
	trash := filepath.Join(rootAbs, trashName, "cur")
	if !strings.HasPrefix(filepath.Clean(trash), rootAbs+string(filepath.Separator)) {
		s.recordMutation(sess, m, "delete", m.Folder, trashName, "partial", errors.New("trash path escaped account root"))
		_ = s.rollbackRemoteMove(sess, m, trashName, m.Folder)
		s.prompt(sess, "I could not delete that message.")
		return
	}
	if err := os.MkdirAll(trash, 0700); err != nil {
		s.recordMutation(sess, m, "delete", m.Folder, trashName, "partial", err)
		_ = s.rollbackRemoteMove(sess, m, trashName, m.Folder)
		s.prompt(sess, "I could not delete that message.")
		return
	}
	target := filepath.Join(trash, filepath.Base(pathAbs))
	if !strings.Contains(target, ":2,") {
		target += ":2,S"
	}
	err := os.Rename(pathAbs, target)
	if err == nil {
		if indexErr := s.Store.DeleteMailIndex(s.baseContext(), sess.UserID, m.ID); indexErr != nil {
			err = indexErr
		}
	}
	if err != nil {
		if renameErr := os.Rename(target, pathAbs); renameErr != nil && s.Log != nil {
			s.Log.Warn("could not roll back local trash move", "message_id", m.ID, "error", renameErr)
		}
		_ = s.rollbackRemoteMove(sess, m, trashName, m.Folder)
		s.recordMutation(sess, m, "delete", m.Folder, trashName, "partial", err)
	} else {
		s.recordMutation(sess, m, "delete", m.Folder, trashName, "success", nil)
	}
	s.mu.Lock()
	transitionLocked(sess, "list")
	s.mu.Unlock()
	if err == nil {
		s.prompt(sess, "Moved to trash.")
	} else {
		s.prompt(sess, "I could not delete that message.")
	}
}

func (s *Service) sendDraft(sess *session) {
	s.mu.Lock()
	if sess.SendInFlight {
		s.mu.Unlock()
		s.prompt(sess, "Sending is already in progress. Please wait for its result.")
		return
	}
	sess.SendInFlight = true
	d := sess.Draft
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		sess.SendInFlight = false
		s.mu.Unlock()
	}()
	parseRecipients := func(values []string) ([]string, error) {
		out := make([]string, 0, len(values))
		for _, value := range values {
			address, err := mail.ParseAddress(value)
			if err != nil {
				return nil, err
			}
			out = append(out, address.Address)
		}
		return out, nil
	}
	toValues := append([]string{d.To}, d.AdditionalTo...)
	to, err := parseRecipients(toValues)
	if err != nil || len(to) == 0 {
		s.prompt(sess, "That recipient is not valid.")
		return
	}
	cc, err := parseRecipients(d.Cc)
	if err != nil {
		s.prompt(sess, "A Cc recipient is not valid.")
		return
	}
	bcc, err := parseRecipients(d.Bcc)
	if err != nil {
		s.prompt(sess, "A Bcc recipient is not valid.")
		return
	}
	accounts, err := s.Store.ListAccounts(s.baseContext(), sess.UserID)
	if err != nil || len(accounts) == 0 || s.Secrets == nil {
		s.prompt(sess, "No sending account is configured.")
		return
	}
	a := accounts[0]
	s.mu.Lock()
	activeAccount := sess.ActiveAccount
	s.mu.Unlock()
	if activeAccount != "" {
		for _, candidate := range accounts {
			if candidate.ID == activeAccount {
				a = candidate
				break
			}
		}
	}
	password, err := s.Secrets.Open(a.SMTPPassword)
	if err != nil {
		s.prompt(sess, "The sending account is unavailable.")
		return
	}
	messageID, err := mailer.NewMessageID(a.Email)
	if err != nil {
		s.prompt(sess, "The message could not be constructed.")
		return
	}
	raw, buildErr := mailer.BuildMessageWithAttachmentsID(a.Email, a.SenderName, to, cc, bcc, d.Subject, d.Body, d.Attachments, messageID)
	if buildErr != nil {
		s.prompt(sess, "The message could not be constructed.")
		return
	}
	if err := s.Store.CreateOutboundSubmission(s.baseContext(), store.OutboundSubmission{
		MessageID: messageID,
		UserID:    sess.UserID,
		AccountID: a.ID,
		DraftID:   d.ID,
		Status:    "pending",
	}); err != nil {
		if s.Log != nil {
			s.Log.Warn("could not journal SMTP submission", "error", err)
		}
		s.prompt(sess, "The send attempt could not be recorded. Please try again.")
		return
	}
	envelope := append(append(append([]string{}, to...), cc...), bcc...)
	mailConfig := mailer.Config{Host: a.SMTPHost, Port: a.SMTPPort, Security: a.SMTPSecurity, Username: a.SMTPUser, Password: password, From: a.Email}
	submit := s.SubmitMail
	if submit == nil {
		submit = mailer.SendWithOutcome
	}
	sendResult := submit(mailConfig, envelope, raw)
	status := string(sendResult.Status)
	detail := ""
	if sendResult.Status == mailer.SendAccepted && sendResult.CleanupError != nil {
		detail = "message accepted; connection cleanup failed"
	} else if sendResult.Status == mailer.SendUncertain {
		detail = "SMTP outcome is uncertain; reconcile the Message-ID before retrying"
	} else if sendResult.Status == mailer.SendRejected {
		detail = "SMTP submission rejected"
	}
	cleanupCtx, cleanupCancel := lifecycle.CleanupContext(s.baseContext())
	journalErr := s.Store.UpdateOutboundSubmission(cleanupCtx, messageID, status, detail)
	cleanupCancel()
	if journalErr != nil && s.Log != nil {
		s.Log.Warn("could not update SMTP submission journal", "message_id", messageID, "error", journalErr)
	}
	if sendResult.Status != mailer.SendAccepted {
		if sendResult.Status == mailer.SendUncertain {
			s.prompt(sess, "The mail server's result is uncertain. Do not retry yet; check the account before sending again.")
		} else {
			s.prompt(sess, "Sending failed. Check the account settings.")
		}
		return
	}
	if sendResult.CleanupError != nil && s.Log != nil {
		s.Log.Warn("SMTP accepted message but QUIT cleanup failed", "error", sendResult.CleanupError)
	}
	if d.ID != "" {
		if err := s.Store.DeleteDraft(s.baseContext(), sess.UserID, d.ID); err == nil {
			s.removeDraftAttachments(sess, d)
		}
	}
	s.mu.Lock()
	transitionLocked(sess, "main")
	s.mu.Unlock()
	s.prompt(sess, "Message sent.")
}

func (s *Service) saveDraft(sess *session) {
	s.mu.Lock()
	d := sess.Draft
	activeAccount := sess.ActiveAccount
	s.mu.Unlock()
	accounts, err := s.Store.ListAccounts(s.baseContext(), sess.UserID)
	if err != nil || len(accounts) == 0 {
		s.prompt(sess, "No mail account is available for this draft.")
		return
	}
	accountID := activeAccount
	if accountID == "" {
		accountID = accounts[0].ID
	}
	draftID := d.ID
	if draftID == "" {
		draftID = fmt.Sprintf("draft-%d", time.Now().UnixNano())
	}
	var oldAttachmentPaths []string
	if d.ID != "" {
		if previous, loadErr := s.Store.LoadDraft(s.baseContext(), sess.UserID, d.ID); loadErr == nil {
			for _, attachment := range previous.Attachments {
				oldAttachmentPaths = append(oldAttachmentPaths, attachment.Path)
			}
		}
	}
	dir := s.DataRoot
	if dir == "" {
		dir = filepath.Dir(sess.TxPath)
	}
	dir = filepath.Join(dir, "drafts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		s.prompt(sess, "Draft storage is unavailable.")
		return
	}
	stageDir, err := os.MkdirTemp(dir, ".draft-stage-")
	if err != nil {
		s.prompt(sess, "Draft attachment storage is unavailable.")
		return
	}
	defer os.RemoveAll(stageDir)
	for i, attachment := range d.Attachments {
		if len(attachment.Data) == 0 {
			continue
		}
		path := filepath.Join(stageDir, fmt.Sprintf("%d.bin", i))
		if err := os.WriteFile(path, attachment.Data, 0600); err != nil {
			s.prompt(sess, "Draft attachment storage failed.")
			return
		}
	}
	generationID, err := bridge.NewRequestID()
	if err != nil {
		s.prompt(sess, "Draft attachment storage failed.")
		return
	}
	generationDir := filepath.Join(dir, draftStorageKey(draftID), fmt.Sprintf("%d-%s", time.Now().UnixNano(), generationID))
	if err := os.MkdirAll(filepath.Dir(generationDir), 0700); err != nil {
		s.prompt(sess, "Draft attachment storage is unavailable.")
		return
	}
	if err := os.Rename(stageDir, generationDir); err != nil {
		s.prompt(sess, "Draft attachment storage failed.")
		return
	}
	attachments := make([]store.DraftAttachment, 0, len(d.Attachments))
	for i, attachment := range d.Attachments {
		if len(attachment.Data) == 0 {
			continue
		}
		name := fmt.Sprintf("%d.bin", i)
		attachments = append(attachments, store.DraftAttachment{Filename: attachment.Filename, ContentType: attachment.ContentType, Path: filepath.Join(generationDir, name), Size: int64(len(attachment.Data))})
	}
	recipients := append([]string{d.To}, d.AdditionalTo...)
	record := store.DraftRecord{ID: draftID, UserID: sess.UserID, AccountID: accountID, Subject: d.Subject, Body: d.Body, To: recipients, Cc: d.Cc, Bcc: d.Bcc, Attachments: attachments, OriginalMessageID: d.OriginalMessageID}
	if d.ForwardOriginal {
		record.ForwardMode = "forward"
	}
	if err := s.Store.SaveDraft(s.baseContext(), record); err != nil {
		_ = os.RemoveAll(generationDir)
		s.prompt(sess, "The draft could not be saved.")
		return
	}
	for _, oldPath := range oldAttachmentPaths {
		if underDraftRoot(oldPath, dir) {
			_ = os.Remove(oldPath)
		}
	}
	if cleanupErr := cleanupDraftGenerations(dir, draftID, generationDir); cleanupErr != nil && s.Log != nil {
		s.Log.Warn("could not clean obsolete draft attachment generations", "draft_id", draftID, "error", cleanupErr)
	}
	for _, attachment := range attachments {
		if _, statErr := os.Stat(attachment.Path); statErr != nil && !os.IsNotExist(statErr) {
			if s.Log != nil {
				s.Log.Warn("draft attachment stat failed after commit", "path", attachment.Path, "error", statErr)
			}
		}
	}
	// IMAP APPEND creates a new message; it cannot update the message created
	// by an earlier save. The durable local ID is therefore the at-most-once
	// remote-append marker. Local draft state remains canonical after the first
	// save, including after a restart, so repeated saves cannot create duplicate
	// remote Drafts messages.
	remoteSaved := false
	remoteAppendSkipped := d.ID != ""
	if !remoteAppendSkipped {
		if account, ok := findAccount(accounts, accountID); ok {
			if role, roleErr := s.Store.FolderRole(s.baseContext(), account, "drafts"); roleErr == nil && (s.Secrets != nil || s.DraftAppender != nil) {
				to, cc, bcc := draftAddresses(d)
				raw := mailer.BuildMessageWithAttachments(account.Email, account.SenderName, to, cc, bcc, d.Subject, d.Body, d.Attachments)
				if s.DraftAppender != nil {
					remoteSaved = s.DraftAppender(s.baseContext(), account, role.RemotePath, raw) == nil
				} else if password, openErr := s.Secrets.Open(account.IMAPPassword); openErr == nil {
					if connection, connErr := s.openAccountIMAP(account, password); connErr == nil {
						remoteSaved = connection.Append(role.RemotePath, raw) == nil
						_ = connection.Close()
					}
				}
			}
		}
	}
	s.mu.Lock()
	sess.Draft.ID = draftID
	transitionLocked(sess, "main")
	s.mu.Unlock()
	if remoteSaved {
		s.prompt(sess, "Draft saved to the remote Drafts folder.")
	} else if remoteAppendSkipped {
		s.prompt(sess, "Draft saved locally. Its remote Drafts copy is an initial snapshot; later edits remain local.")
	} else {
		s.prompt(sess, "Draft saved locally. Map a Drafts folder and check the account connection to upload it remotely.")
	}
}

func (s *Service) removeDraftAttachments(sess *session, d draft) {
	if d.ID == "" || sess == nil {
		return
	}
	root := s.DataRoot
	if root == "" {
		root = filepath.Dir(sess.TxPath)
	}
	draftsRoot := filepath.Join(root, "drafts")
	if underDraftRoot(filepath.Join(draftsRoot, draftStorageKey(d.ID)), draftsRoot) {
		_ = os.RemoveAll(filepath.Join(draftsRoot, draftStorageKey(d.ID)))
	}
}

func draftStorageKey(id string) string {
	return store.DraftStorageKey(id)
}

func underDraftRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	return path != root && strings.HasPrefix(path, root+string(filepath.Separator))
}

func cleanupDraftGenerations(draftsRoot, draftID, keep string) error {
	storageRoot := filepath.Join(draftsRoot, draftStorageKey(draftID))
	entries, err := os.ReadDir(storageRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	keep = filepath.Clean(keep)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(storageRoot, entry.Name())
		if filepath.Clean(candidate) == keep || !underDraftRoot(candidate, storageRoot) {
			continue
		}
		if err := os.RemoveAll(candidate); err != nil {
			return err
		}
	}
	return nil
}

func findAccount(accounts []store.Account, id string) (store.Account, bool) {
	for _, account := range accounts {
		if account.ID == id {
			return account, true
		}
	}
	return store.Account{}, false
}

func draftAddresses(d draft) (to, cc, bcc []string) {
	if strings.TrimSpace(d.To) != "" {
		to = append(to, d.To)
	}
	to = append(to, d.AdditionalTo...)
	cc = append(cc, d.Cc...)
	bcc = append(bcc, d.Bcc...)
	return to, cc, bcc
}

func (s *Service) openAccountIMAP(account store.Account, password string) (*remoteimap.Client, error) {
	port := account.IMAPPort
	if port == 0 {
		port = 993
	}
	address, err := s.resolveIMAPEndpoint(s.baseContext(), account.IMAPHost, port)
	if err != nil {
		return nil, fmt.Errorf("IMAP endpoint rejected by outbound policy: %w", err)
	}
	return remoteimap.Open(s.baseContext(), remoteimap.Config{Host: account.IMAPHost, Address: address, Port: port, Security: account.IMAPSecurity, Username: account.IMAPUser, Password: password})
}

func (s *Service) resolveIMAPEndpoint(ctx context.Context, host string, port int) (string, error) {
	if s != nil && s.IMAPEndpointResolver != nil {
		return s.IMAPEndpointResolver(ctx, host, port)
	}
	return netguard.CheckAndPin(ctx, host, port, false)
}
