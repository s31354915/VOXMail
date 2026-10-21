package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/voxmail/voxmail/internal/secret"
)

func TestSIPTransportNormalization(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "sip.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	box, err := secret.New("test-key-with-more-than-32-characters-123456")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ in, want string }{
		{"tls", "tls"},
		{" TLS ", "tls"},
		{"tcp", "tcp"},
		{"", "udp"},
		{"sctp", "udp"},
	} {
		if err := db.UpsertSIP(ctx, box, SIPSettings{Domain: "sip.example.com", Username: "bob", Password: "x", Transport: tc.in}); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetSIP(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.Transport != tc.want {
			t.Fatalf("transport %q stored/read as %q, want %q", tc.in, got.Transport, tc.want)
		}
	}
	if _, err := db.DB.ExecContext(ctx, `UPDATE sip_settings SET transport='TLS ' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetSIP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Transport != "tls" {
		t.Fatalf("legacy row value 'TLS ' read back as %q, want tls", got.Transport)
	}
}
