// voxmail-secret is intentionally tiny because mbsync invokes it as PassCmd.
// It writes only the requested password to stdout and never logs credentials.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/voxmail/voxmail/internal/config"
	"github.com/voxmail/voxmail/internal/secret"
	"github.com/voxmail/voxmail/internal/store"
)

func main() {
	mode := "imap"
	accountID := ""
	switch {
	case len(os.Args) == 2 && os.Args[1] == "--check-sip":
		mode = "check-sip"
	case len(os.Args) == 3 && os.Args[1] == "--check-account":
		mode = "check-account"
		accountID = os.Args[2]
	case len(os.Args) == 2:
		accountID = os.Args[1]
	default:
		fail("usage: voxmail-secret ACCOUNT_ID | --check-account ACCOUNT_ID | --check-sip")
	}
	cfg, err := config.Load()
	if err != nil {
		fail(err.Error())
	}
	box, err := secret.New(cfg.EncryptionKey)
	if err != nil {
		fail(err.Error())
	}
	db, err := store.OpenReadOnly(cfg.DBPath)
	if err != nil {
		fail(err.Error())
	}
	defer db.Close()
	switch mode {
	case "imap":
		ciphertext, err := db.IMAPPassword(context.Background(), accountID)
		if err != nil {
			fail(err.Error())
		}
		password, err := box.Open(ciphertext)
		if err != nil {
			fail(err.Error())
		}
		_, _ = fmt.Fprintln(os.Stdout, password)
	case "check-account":
		if err := checkAccountSecrets(context.Background(), db, box, accountID); err != nil {
			fail(err.Error())
		}
	case "check-sip":
		if err := checkSIPSecret(context.Background(), db, box); err != nil {
			fail(err.Error())
		}
	}
}

func checkAccountSecrets(ctx context.Context, db *store.Store, box *secret.Box, accountID string) error {
	for _, load := range []struct {
		name string
		read func(context.Context, string) (string, error)
	}{
		{name: "IMAP", read: db.IMAPPassword},
		{name: "SMTP", read: db.SMTPPassword},
	} {
		ciphertext, err := load.read(ctx, accountID)
		if err != nil {
			return fmt.Errorf("read %s password: %w", load.name, err)
		}
		if _, err := box.Open(ciphertext); err != nil {
			return fmt.Errorf("decrypt %s password: %w", load.name, err)
		}
	}
	return nil
}

func checkSIPSecret(ctx context.Context, db *store.Store, box *secret.Box) error {
	settings, err := db.GetSIP(ctx)
	if err != nil {
		return fmt.Errorf("read SIP settings: %w", err)
	}
	if _, err := box.Open(settings.Password); err != nil {
		return fmt.Errorf("decrypt SIP password: %w", err)
	}
	return nil
}

func fail(message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
