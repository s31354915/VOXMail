package calls

import (
	"context"
	"errors"
	"fmt"
	"github.com/voxmail/voxmail/internal/maintenance"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/bridge"
	remoteimap "github.com/voxmail/voxmail/internal/imap"
	"github.com/voxmail/voxmail/internal/lifecycle"
	"github.com/voxmail/voxmail/internal/mailer"
	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/netguard"
	"github.com/voxmail/voxmail/internal/speech"
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

// openSessionMessage opens a session message's file even when its indexed path
// has gone stale. It re-reads the row's current path and the paths of duplicate
// rows for the same message identity (an index pass can leave a stale duplicate
// behind a flag rename), so the message stays playable instead of reporting
// "not available", and returns the location that actually opened.
func (s *Service) openSessionMessage(sess *session, m store.MailSummary) (*os.File, string, error) {
	file, err := openMessageFile(m.Path)
	if err == nil {
		return file, m.Path, nil
	}
	if s.Store == nil {
		return nil, "", err
	}
	own, siblings, lookupErr := s.Store.MailReadCandidates(s.baseContext(), sess.UserID, m.ID)
	if lookupErr != nil {
		return nil, "", err
	}
	for _, candidate := range append([]string{own}, siblings...) {
		if candidate == "" || candidate == m.Path {
			continue
		}
		if file, openErr := openMessageFile(candidate); openErr == nil {
			return file, candidate, nil
		}
	}
	return nil, "", err
}

func (s *Service) readMessage(sess *session, m store.MailSummary) {
	file, resolvedPath, err := s.openSessionMessage(sess, m)
	if err != nil {
		s.prompt(sess, "That message is no longer available.")
		s.prompt(sess, menuPrompt("read"))
		return
	}
	parsed, err := mailparse.Parse(file)
	_ = file.Close()
	if err != nil {
		s.prompt(sess, "I could not read that message. Press 0 for the next message, or pound to go back.")
		return
	}
	// The listed path can go stale between the unread list snapshot and this
	// read (a flag rename or a re-homed index row). Pointing the session at the
	// location that opened keeps the follow-up mark-read, delete and forward
	// actions working on the file that is actually present.
	if resolvedPath != m.Path {
		s.updateSessionMessagePath(sess, m.ID, resolvedPath)
		m.Path = resolvedPath
	}
	// The unread marker is spent only after the message has started playing:
	// a failed or interrupted opening must not mark mail the caller never
	// heard.
	markRead := func() {
		localPath, err := s.queueLocalReadState(sess, m, true)
		if err != nil {
			s.prompt(sess, "I couldn't save that change just now. Try again in a moment.")
			return
		}
		if localPath != m.Path {
			s.updateSessionMessagePath(sess, m.ID, localPath)
			m.Path = localPath
		}
	}
	opening := "Email from"
	if !m.Read {
		opening = "New unread email from"
	}
	header := fmt.Sprintf("%s %s. Subject %s.", opening, spokenHeader(parsed.From, "unknown sender", 240), spokenHeader(parsed.Subject, "no subject", 300))
	body := strings.TrimSpace(parsed.Text)
	if body == "" {
		body = "There is no readable email body."
	} else if cleaned := speech.SanitizeNonLatin(body); cleaned != body {
		// Interim handling for scripts the voice engine cannot speak: warn
		// the caller up front and frame what follows, instead of burying a
		// wordy notice after the sanitized run.
		body = "Parts of this message are written in a language I cannot read aloud. Here is what I can read. " + cleaned
	}
	s.mu.Lock()
	sess.CurrentAttachments = append([]mailparse.Attachment(nil), parsed.Attachments...)
	sess.AttachmentPage = 0
	s.mu.Unlock()
	if len(parsed.Attachments) > 0 {
		playable := 0
		descriptions := make([]string, 0, 3)
		for _, attachment := range parsed.Attachments {
			kind := attachmentDescription(attachment.ContentType)
			status := "open in your email app"
			if attachment.Playable {
				status = "can play here"
				playable++
			}
			// Only the first few attachment names are spoken; a long list is
			// summarized so it does not bury the message body.
			if len(descriptions) < 3 {
				descriptions = append(descriptions, fmt.Sprintf("%s, %s, %s", speech.SanitizeNonLatin(attachment.Name), kind, status))
			}
		}
		summary := strings.Join(descriptions, ". ")
		if more := len(parsed.Attachments) - len(descriptions); more > 0 {
			summary += fmt.Sprintf(". And %d more.", more)
		}
		body += fmt.Sprintf(" There are %d attachments: %s.", len(parsed.Attachments), summary)
		if playable > 0 {
			body += " Press 8 to listen to playable attachments."
		} else {
			body += " There are no playable attachments."
		}
	}
	menu := menuPrompt("read")
	if runeLen(body) <= singlePromptBodyRunes {
		s.prompt(sess, header+" "+body+" "+menu)
		markRead()
		return
	}
	spoken, truncated := truncateForSpeech(body, maxSpokenBodyRunes)
	if truncated {
		spoken += " This is a long message. Open it in your email app to read the rest."
	}
	chunks := splitSpeechChunks(spoken, speechChunkRunes)
	first := header
	if len(chunks) > 0 {
		first += " " + chunks[0]
		chunks = chunks[1:]
	}
	// The header and first chunk play at once; the rest of the body streams
	// chunk by chunk while the next chunk synthesizes, and the menu follows
	// as its own short prompt instead of being buried after a long body.
	s.prompt(sess, first)
	// A keypress during the opening spent its turn: skip the rest of the
	// body and the menu so the pressed key takes effect right away.
	if s.wasInterrupted(sess) {
		return
	}
	markRead()
	if len(chunks) > 0 {
		s.playSpeechChunks(sess, chunks)
	}
	if s.wasInterrupted(sess) {
		return
	}
	s.promptMenu(sess, menu)
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
	tmp, releaseTmp, err := maintenance.CreateTemp(dir, ".attachment-*")
	if err != nil {
		s.prompt(sess, "I could not prepare that attachment.")
		return
	}
	defer releaseTmp()
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
	ctx, cancel := context.WithTimeout(s.eventContext(sess), 10*time.Minute)
	s.mu.Lock()
	if sess.Closed || stateLocked(sess) != "attachment_playback" {
		s.mu.Unlock()
		cancel()
		return
	}
	sess.attachmentSequence++
	sequence := sess.attachmentSequence
	sess.AttachmentCancel = cancel
	s.beginMediaLocked(sess)
	s.mu.Unlock()
	sess.promptMu.Lock()
	err = s.Media.PlayMedia(ctx, sess.TxPath, path)
	sess.promptMu.Unlock()
	playbackContextErr := ctx.Err()
	cancel()
	s.mu.Lock()
	wasPlaying := !sess.Closed && sess.attachmentSequence == sequence && string(stateLocked(sess)) == "attachment_playback"
	if sess.attachmentSequence == sequence {
		sess.AttachmentCancel = nil
	}
	s.endMediaLocked(sess)
	if wasPlaying {
		transitionLocked(sess, "read")
	}
	s.mu.Unlock()
	if wasPlaying {
		if err != nil && playbackContextErr != context.Canceled {
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
	mailboxes, err := s.Store.ListLogicalMailboxes(s.baseContext(), sess.UserID, account.ID)
	if err != nil {
		s.prompt(sess, "Folders are temporarily unavailable.")
		return
	}
	folders := make([]string, 0, len(mailboxes))
	labels := make(map[string]string, len(mailboxes))
	for _, mailbox := range mailboxes {
		folders = append(folders, mailbox.Key)
		labels[mailbox.Key] = mailbox.Name
	}
	if len(folders) == 0 {
		s.prompt(sess, "There are no destination folders.")
		return
	}
	s.mu.Lock()
	sess.MoveFolders = folders
	sess.MoveFolderLabels = labels
	sess.MovePage = 0
	transitionLocked(sess, "move_menu")
	s.mu.Unlock()
	s.promptMoveMenu(sess)
}

func (s *Service) promptMoveMenu(sess *session) {
	s.mu.Lock()
	page := sess.MovePage
	labels := copyFolderLabels(sess.MoveFolderLabels)
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
		parts = append(parts, fmt.Sprintf("Press %d for %s", i+1, folderLabel(store.Account{}, folder, labels)))
	}
	next := ""
	if end < len(folders) {
		next = " Press 0 for more folders."
	}
	s.promptMenu(sess, "Move message. "+strings.Join(parts, ". ")+"."+next+" Press pound to return.")
}

func (s *Service) moveCurrent(sess *session, destination string) {
	s.mu.Lock()
	if sess.Cursor < 0 || sess.Cursor >= len(sess.Messages) {
		s.mu.Unlock()
		return
	}
	message := sess.Messages[sess.Cursor]
	s.mu.Unlock()
	if strings.HasPrefix(destination, "/groups/") {
		_, local, err := s.Store.GroupDestination(s.baseContext(), sess.UserID, message.AccountID, destination)
		if err != nil {
			s.prompt(sess, "That destination is no longer available. Reopen the folder list.")
			return
		}
		destination = local
	}
	cleanDestination := filepath.Clean(filepath.FromSlash(destination))
	if cleanDestination == "." || filepath.IsAbs(cleanDestination) || strings.HasPrefix(cleanDestination, ".."+string(filepath.Separator)) || cleanDestination == ".." {
		s.prompt(sess, "That destination is not allowed.")
		return
	}
	s.prompt(sess, "Working on the mail server. One moment.")
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
		s.prompt(sess, "I couldn't move that message just now. Try again in a moment.")
		return
	}
	target := filepath.Join(targetDir, filepath.Base(message.Path))
	if err := os.Rename(message.Path, target); err != nil {
		s.recordMutation(sess, message, "move", message.Folder, destination, "partial", err)
		_ = s.rollbackRemoteMove(sess, message, destination, message.Folder)
		s.prompt(sess, "I couldn't move that message just now. Try again in a moment.")
		return
	}
	if err := s.Store.MoveMailIndex(s.baseContext(), sess.UserID, message.ID, cleanDestination, target); err != nil {
		_ = os.Rename(target, message.Path)
		s.recordMutation(sess, message, "move", message.Folder, destination, "partial", err)
		_ = s.rollbackRemoteMove(sess, message, destination, message.Folder)
		s.prompt(sess, "I couldn't move that message just now. Try again in a moment.")
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
	localPath, err := s.queueLocalReadState(sess, message, read)
	if err != nil {
		s.prompt(sess, "I couldn't save that change just now. Try again in a moment.")
		return
	}
	if localPath != message.Path {
		s.updateSessionMessagePath(sess, message.ID, localPath)
	}
	if read {
		s.prompt(sess, "The message is marked read.")
	} else {
		s.prompt(sess, "The message is marked unread.")
	}
}

// queueLocalReadState is deliberately Maildir-first. mbsync's bidirectional
// Sync All propagates the S flag on its next run, while the renamed local file
// and SQLite path keep the cache internally consistent immediately. Direct
// IMAP is reserved for operations that Maildir cannot represent safely, such
// as a cross-folder move.
func (s *Service) queueLocalReadState(sess *session, message store.MailSummary, read bool) (string, error) {
	release := s.Store.LockMailReadState()
	defer release()
	path := message.Path
	if _, statErr := os.Stat(path); statErr != nil {
		// mbsync renames message files whenever a flag changes, and since
		// round 6 up to four syncs run at once, so the path captured during
		// listing can go stale before the mutation. Re-derive the file's
		// current name before giving up.
		if current, ok := rederiveMaildirPath(path); ok {
			path = current
		}
	}
	localPath, err := setMaildirRead(path, read)
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
	cleanupCtx, cleanupCancel := lifecycle.CleanupContext(s.baseContext())
	defer cleanupCancel()
	queued := message
	queued.Path = localPath
	queuedErr := s.Store.RecordQueuedRead(cleanupCtx, sess.UserID, queued, read)
	if queuedErr != nil {
		// A sync racing the rename can invalidate the insert's exact
		// folder, path, UID match. Re-derive the file's current location
		// and retry once before rolling back the rename.
		if current, ok := rederiveMaildirPath(localPath); ok && current != localPath {
			queued.Path = current
			if retryErr := s.Store.RecordQueuedRead(cleanupCtx, sess.UserID, queued, read); retryErr == nil {
				localPath = current
				queuedErr = nil
			}
		}
	}
	if queuedErr != nil {
		var rollbackErr error
		if localPath != message.Path {
			rollbackErr = os.Rename(localPath, message.Path)
		}
		if rollbackErr == nil {
			rollbackErr = s.Store.MarkConfirmedReadAtPath(cleanupCtx, queued, message.Read, message.Path)
		}
		return "", errors.Join(queuedErr, rollbackErr)
	}
	_ = s.Store.Audit(cleanupCtx, sess.UserID, "mail_mutation", "mark_read:queued")
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

// rederiveMaildirPath locates the current on-disk name of a Maildir message
// whose path went stale while the call was running. mbsync renames message
// files whenever a flag changes, so a path captured during listing can point
// at a file that no longer exists by mutation time. The search is bounded:
// the original path, its two flag variants, and the message's own directory
// scanned for files sharing the same base name.
func rederiveMaildirPath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
		return path, true
	}
	for _, variant := range []string{mailparse.MaildirReadPath(path, true), mailparse.MaildirReadPath(path, false)} {
		if variant == path {
			continue
		}
		if info, statErr := os.Stat(variant); statErr == nil && info.Mode().IsRegular() {
			return variant, true
		}
	}
	dir, base := filepath.Split(path)
	prefix, _, _ := strings.Cut(base, ":2,")
	if dir == "" || prefix == "" {
		return "", false
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		return "", false
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == base || !entry.Type().IsRegular() {
			continue
		}
		if entryPrefix, _, _ := strings.Cut(name, ":2,"); entryPrefix == prefix {
			return filepath.Join(dir, name), true
		}
	}
	return "", false
}

// cachedFolderRole resolves a folder role for one of the session's accounts
// and memoizes the result on the session. A call that deletes, moves, or saves
// several messages used to re-list every account and re-query folder_roles
// for every single mutation.
func (s *Service) cachedFolderRole(sess *session, accountID, role string) (local, remote string) {
	if sess == nil {
		return "", ""
	}
	s.mu.Lock()
	key := accountID + "|" + role
	if sess.FolderRoleCache == nil {
		sess.FolderRoleCache = make(map[string]store.FolderRole)
	}
	if cached, ok := sess.FolderRoleCache[key]; ok {
		s.mu.Unlock()
		return cached.LocalPath, cached.RemotePath
	}
	s.mu.Unlock()
	if accounts, err := s.Store.ListAccounts(s.baseContext(), sess.UserID); err == nil {
		for _, account := range accounts {
			if account.ID != accountID {
				continue
			}
			if found, roleErr := s.Store.FolderRole(s.baseContext(), account, role); roleErr == nil {
				local, remote = found.LocalPath, found.RemotePath
			}
		}
	}
	s.mu.Lock()
	sess.FolderRoleCache[key] = store.FolderRole{AccountID: accountID, Role: role, LocalPath: local, RemotePath: remote}
	s.mu.Unlock()
	return local, remote
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
	trashName, trashRemote := s.cachedFolderRole(sess, m.AccountID, "trash")
	if trashRemote == "" || trashName == "" {
		// The caller can fix this themselves: folder roles are mapped in the
		// web console, so point there instead of stopping at a dead end.
		s.prompt(sess, "A Trash folder has not been mapped for this account. Map it in the web console to delete mail over the phone.")
		return
	}
	// The remote move hits the mail server and can take a few seconds;
	// without a working prompt the caller hears silence and retries keys.
	s.prompt(sess, "Working on the mail server. One moment.")
	if err := s.moveRemote(sess, m, trashName); err != nil {
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
	// The listed path can be stale: an earlier mark-read or toggle in this
	// call renamed the maildir file. Resolve the current location before
	// renaming into Trash.
	source := pathAbs
	if _, statErr := os.Stat(source); statErr != nil {
		for _, candidate := range []string{mailparse.MaildirReadPath(pathAbs, true), mailparse.MaildirReadPath(pathAbs, false)} {
			if candidate == pathAbs {
				continue
			}
			if info, candErr := os.Stat(candidate); candErr == nil && info.Mode().IsRegular() {
				source = candidate
				break
			}
		}
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
	target := filepath.Join(trash, filepath.Base(source))
	if !strings.Contains(target, ":2,") {
		target += ":2,S"
	}
	err := os.Rename(source, target)
	if err == nil {
		if indexErr := s.Store.DeleteMailIndex(s.baseContext(), sess.UserID, m.ID); indexErr != nil {
			err = indexErr
		}
	}
	if err != nil {
		if renameErr := os.Rename(target, source); renameErr != nil && s.Log != nil {
			s.Log.Warn("could not roll back local trash move", "message_id", m.ID, "error", renameErr)
		}
		_ = s.rollbackRemoteMove(sess, m, trashName, m.Folder)
		s.recordMutation(sess, m, "delete", m.Folder, trashName, "partial", err)
	} else {
		s.recordMutation(sess, m, "delete", m.Folder, trashName, "success", nil)
	}
	s.mu.Lock()
	if err == nil {
		// Drop the deleted message from the session list and point the
		// cursor at its neighbor, so later keys do not act on a message
		// that is gone.
		if sess.Cursor >= 0 && sess.Cursor < len(sess.Messages) && sess.Messages[sess.Cursor].ID == m.ID {
			sess.Messages = append(sess.Messages[:sess.Cursor], sess.Messages[sess.Cursor+1:]...)
			if sess.Cursor >= len(sess.Messages) {
				if len(sess.Messages) > 0 {
					sess.Cursor = len(sess.Messages) - 1
				} else {
					sess.Cursor = 0
				}
			}
		}
	}
	transitionLocked(sess, "list")
	s.mu.Unlock()
	if err == nil {
		s.prompt(sess, "Moved to trash.")
	} else {
		s.prompt(sess, "I could not delete that message.")
	}
}

// openMessageFile opens a message by its listed path. A message whose read
// flag changed earlier in the call was renamed on disk while the session list
// may still hold the original path, so the renamed forms are tried before
// giving up.
func openMessageFile(path string) (*os.File, error) {
	file, err := os.Open(path)
	if err == nil {
		return file, nil
	}
	for _, renamed := range []string{mailparse.MaildirReadPath(path, true), mailparse.MaildirReadPath(path, false)} {
		if renamed == path {
			continue
		}
		if retry, retryErr := os.Open(renamed); retryErr == nil {
			return retry, nil
		}
	}
	return nil, err
}

// updateSessionMessagePath refreshes the on-disk path of a message in the
// session list after the maildir file was renamed (for example when the read
// flag was set), so later keys in the same call do not fail on a stale path.
func (s *Service) updateSessionMessagePath(sess *session, messageID int64, path string) {
	if s == nil || sess == nil || path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range sess.Messages {
		if sess.Messages[i].ID == messageID {
			sess.Messages[i].Path = path
		}
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
	stageDir, releaseStageDir, err := maintenance.MkdirTemp(dir, ".draft-stage-")
	if err != nil {
		s.prompt(sess, "Draft attachment storage is unavailable.")
		return
	}
	defer releaseStageDir()
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
			draftsLocal, draftsRemote := s.cachedFolderRole(sess, accountID, "drafts")
			if role := (store.FolderRole{AccountID: accountID, Role: "drafts", RemotePath: draftsRemote, LocalPath: draftsLocal}); role.RemotePath != "" && (s.Secrets != nil || s.DraftAppender != nil) {
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

// advanceToPlayable advances the read cursor to the next message whose file can
// still be opened, announcing skipped messages instead of failing them one by
// one. It returns the last message even when nothing playable remains so the
// caller reports the usual unavailability prompt.
func (s *Service) advanceToPlayable(sess *session) store.MailSummary {
	skipped := 0
	for attempt := 0; attempt < 9; attempt++ {
		s.mu.Lock()
		sess.Cursor = (sess.Cursor + 1) % len(sess.Messages)
		m := sess.Messages[sess.Cursor]
		s.mu.Unlock()
		if file, _, err := s.openSessionMessage(sess, m); err == nil {
			_ = file.Close()
			// One notice with the count, instead of a single notice that
			// undersells several skips in a row.
			switch {
			case skipped == 1:
				s.prompt(sess, "Skipping a message that is no longer available.")
			case skipped > 1:
				s.prompt(sess, fmt.Sprintf("Skipping %d messages that are no longer available.", skipped))
			}
			return m
		}
		skipped++
	}
	switch {
	case skipped == 1:
		s.prompt(sess, "Skipping a message that is no longer available.")
	case skipped > 1:
		s.prompt(sess, fmt.Sprintf("Skipping %d messages that are no longer available.", skipped))
	}
	s.mu.Lock()
	m := sess.Messages[sess.Cursor]
	s.mu.Unlock()
	return m
}

func attachmentDescription(contentType string) string {
	switch {
	case contentType == "application/pdf":
		return "PDF document"
	case strings.HasPrefix(contentType, "audio/"):
		return "audio file"
	case strings.HasPrefix(contentType, "video/"):
		return "video"
	case strings.HasPrefix(contentType, "image/"):
		return "picture"
	case strings.HasPrefix(contentType, "text/"):
		return "text document"
	case contentType == "message/rfc822":
		return "attached email"
	default:
		return "file"
	}
}
