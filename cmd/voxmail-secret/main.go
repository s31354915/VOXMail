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
	if len(os.Args) != 2 {
		fail("usage: voxmail-secret ACCOUNT_ID")
	}
	cfg, err := config.Load()
	if err != nil {
		fail(err.Error())
	}
	box, err := secret.New(cfg.EncryptionKey)
	if err != nil {
		fail(err.Error())
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		fail(err.Error())
	}
	defer db.Close()
	ciphertext, err := db.IMAPPassword(context.Background(), os.Args[1])
	if err != nil {
		fail(err.Error())
	}
	password, err := box.Open(ciphertext)
	if err != nil {
		fail(err.Error())
	}
	_, _ = fmt.Fprintln(os.Stdout, password)
}

func fail(message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
