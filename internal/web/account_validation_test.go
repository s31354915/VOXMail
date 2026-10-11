package web

import (
	"errors"
	"testing"

	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

func TestFillAccountValidationPasswordsUsesSavedCredentialsOnlyForBlankFields(t *testing.T) {
	box, err := secret.New("account-validation-disposable-key-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	imapPassword, err := box.Seal("saved-imap-password")
	if err != nil {
		t.Fatal(err)
	}
	smtpPassword, err := box.Seal("saved-smtp-password")
	if err != nil {
		t.Fatal(err)
	}
	saved := store.Account{ID: "owned-mailbox", IMAPPassword: imapPassword, SMTPPassword: smtpPassword}

	got, err := fillAccountValidationPasswords(accountRequest{ID: saved.ID, IMAPHost: "edited-imap.example.test", SMTPHost: "edited-smtp.example.test"}, saved, box)
	if err != nil {
		t.Fatal(err)
	}
	if got.IMAPHost != "edited-imap.example.test" || got.SMTPHost != "edited-smtp.example.test" {
		t.Fatalf("edited hosts were replaced: IMAP=%q SMTP=%q", got.IMAPHost, got.SMTPHost)
	}
	if got.IMAPPassword != "saved-imap-password" || got.SMTPPassword != "saved-smtp-password" {
		t.Fatalf("saved credentials were not restored for the test: IMAP=%q SMTP=%q", got.IMAPPassword, got.SMTPPassword)
	}

	got, err = fillAccountValidationPasswords(accountRequest{ID: saved.ID, IMAPPassword: "edited-imap-password"}, saved, box)
	if err != nil {
		t.Fatal(err)
	}
	if got.IMAPPassword != "edited-imap-password" || got.SMTPPassword != "saved-smtp-password" {
		t.Fatalf("explicit edits were not preserved: IMAP=%q SMTP=%q", got.IMAPPassword, got.SMTPPassword)
	}
}

func TestFillAccountValidationPasswordsRequiresOwnedSavedAccountAndSecretBox(t *testing.T) {
	saved := store.Account{ID: "owned-mailbox", IMAPPassword: "sealed-imap", SMTPPassword: "sealed-smtp"}
	if _, err := fillAccountValidationPasswords(accountRequest{ID: "other-mailbox"}, saved, nil); !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("wrong account ID error=%v, want not found", err)
	}
	if _, err := fillAccountValidationPasswords(accountRequest{ID: saved.ID}, saved, nil); !errors.Is(err, errAccountValidationSecretsUnavailable) {
		t.Fatalf("missing secret box error=%v", err)
	}
	box, err := secret.New("account-validation-disposable-key-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fillAccountValidationPasswords(accountRequest{ID: saved.ID}, saved, box); !errors.Is(err, errStoredAccountCredentialsUnavailable) {
		t.Fatalf("invalid stored credential error=%v", err)
	}
}
