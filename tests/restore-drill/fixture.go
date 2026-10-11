package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

const fixtureMail = "From: sender@example.test\r\nSubject: Restore fixture\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=restore-fixture\r\n\r\n--restore-fixture\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nRestored message.\r\n--restore-fixture\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=payload.txt\r\nContent-Transfer-Encoding: base64\r\n\r\nc2FtcGxlLXBheWxvYWQ=\r\n--restore-fixture--\r\n"

func main() {
	if len(os.Args) != 2 {
		fail("usage: fixture.go BACKUP.tar.gz")
	}
	key := os.Getenv("VOXMAIL_ENCRYPTION_KEY")
	if len(key) < 32 {
		fail("VOXMAIL_ENCRYPTION_KEY must contain at least 32 bytes")
	}
	root, err := os.MkdirTemp("", "voxmail-restore-fixture-")
	if err != nil {
		fail(err.Error())
	}
	defer os.RemoveAll(root)
	dataRoot := filepath.Join(root, "data")
	for _, dir := range []string{
		"sqlite", "mail/account-1/Inbox/cur", "voices", "prompts", "recordings",
	} {
		if err := os.MkdirAll(filepath.Join(dataRoot, dir), 0755); err != nil {
			fail(err.Error())
		}
	}
	if err := writeFixture(dataRoot, key); err != nil {
		fail(err.Error())
	}
	if err := writeArchive(dataRoot, os.Args[1]); err != nil {
		fail(err.Error())
	}
}

func writeFixture(dataRoot, key string) error {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(dataRoot, "sqlite", "voxmail.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	box, err := secret.New(key)
	if err != nil {
		return err
	}
	imapPassword, err := box.Seal("restored-imap-secret")
	if err != nil {
		return err
	}
	smtpPassword, err := box.Seal("restored-smtp-secret")
	if err != nil {
		return err
	}
	if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,enabled,created_at) VALUES('user-1','restore-admin','fixture','fixture','admin',1,'2026-01-01T00:00:00Z')`); err != nil {
		return err
	}
	if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,imap_security,smtp_host,smtp_user,smtp_password,folder_map,call_alert_enabled,alert_folders,created_at) VALUES('account-1','user-1','Restored mailbox','restore@example.test','Restore','imap.invalid','restore',?,'implicit_tls','smtp.invalid','restore',?,'{"INBOX":"Inbox"}',1,'["INBOX"]','2026-01-01T00:00:00Z')`, imapPassword, smtpPassword); err != nil {
		return err
	}
	phone := "+15550001111"
	if err := db.SaveUserSettings(ctx, "user-1", store.UserSettings{AlertsEnabled: true, AlertPhone: &phone}); err != nil {
		return err
	}
	if _, err := db.DB.Exec(`INSERT INTO alert_numbers(user_id,number,active,created_at) VALUES('user-1',?,1,'2026-01-01T00:00:00Z')`, phone); err != nil {
		return err
	}
	if _, err := db.DB.Exec(`INSERT INTO caller_whitelist(user_id,phone,created_at) VALUES('user-1',?,'2026-01-01T00:00:00Z')`, phone); err != nil {
		return err
	}
	if err := db.UpsertSIP(ctx, box, store.SIPSettings{
		Domain: "sip.invalid", Username: "restore-admin", Password: "restored-sip-secret", Enabled: true,
	}); err != nil {
		return err
	}
	if _, err := db.DB.Exec(`UPDATE system_settings SET setup_completed=1 WHERE id=1`); err != nil {
		return err
	}
	mailPath := "/data/mail/account-1/Inbox/cur/restore-fixture.eml"
	if err := os.WriteFile(filepath.Join(dataRoot, "mail/account-1/Inbox/cur/restore-fixture.eml"), []byte(fixtureMail), 0644); err != nil {
		return err
	}
	parsed, err := mailparse.ParseMetadata(strings.NewReader(fixtureMail))
	if err != nil {
		return fmt.Errorf("parse fixture mail: %w", err)
	}
	if len(parsed.Attachments) != 1 {
		return fmt.Errorf("fixture has %d attachments, want one", len(parsed.Attachments))
	}
	messageID, err := db.UpsertMailIndex(ctx, store.MailIndexInput{
		AccountID: "account-1", Folder: "Inbox", Path: mailPath,
		MessageID: "<restore-fixture@example.test>", Sender: "sender@example.test",
		Subject: "Restore fixture", Attachments: len(parsed.Attachments), UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		SourceSize: int64(len(fixtureMail)),
	})
	if err != nil {
		return err
	}
	attachment := parsed.Attachments[0]
	return db.ReplaceAttachmentMetadata(ctx, messageID, []store.AttachmentMetadata{{
		PartPath: "part-1", Filename: attachment.Name, ContentType: attachment.ContentType,
		Size: attachment.Size, Disposition: attachment.Disposition,
	}})
}

func writeArchive(dataRoot, target string) error {
	file, err := os.Create(target)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	err = filepath.Walk(dataRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dataRoot {
			return nil
		}
		relative, err := filepath.Rel(dataRoot, path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relative)
		header.Uid, header.Gid = 10001, 10001
		header.Uname, header.Gname = "voxmail", "voxmail"
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(archive, input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if closeErr := archive.Close(); err == nil {
		err = closeErr
	}
	if closeErr := compressed.Close(); err == nil {
		err = closeErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func fail(message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
