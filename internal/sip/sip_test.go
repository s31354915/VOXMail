package sip

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/store"
)

func TestBuildAccount(t *testing.T) {
	line := buildAccount(store.SIPSettings{
		Domain: "sip.example.com", Username: "+15551212",
		Password: "s3cr@t;p:ass", Port: 5060, Transport: "udp", RegInterval: 300,
	})
	want := "<sip:%2B15551212@sip.example.com:5060;transport=udp>;auth_user=%2B15551212;auth_pass=s3cr%40t%3Bp%3Aass;regint=300"
	if line != want {
		t.Fatalf("account line:\n got %q\nwant %q", line, want)
	}
}

func TestBuildAccountRequiresIdentity(t *testing.T) {
	if got := buildAccount(store.SIPSettings{Domain: "x", RegInterval: 300}); got != "" {
		t.Fatalf("expected empty account without username, got %q", got)
	}
}

func TestWriteConfig(t *testing.T) {
	dir := t.TempDir()
	b := &Baresip{ConfigDir: dir, MaxCalls: 10}
	line := "<sip:user@host:5061;transport=tcp>;auth_user=user;auth_pass=pass;regint=600"
	b.account = line
	if err := b.writeConfig(store.SIPSettings{Domain: "host", Username: "user", Port: 5061, Transport: "tcp", RegInterval: 600}, true); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(config)
	for _, want := range []string{
		"call_max_calls 10",
		"sip_listen 0.0.0.0:5061",
		"call_accept yes",
		"poll_method poll",
		"audio_source voxmail,",
		"audio_player voxmail,",
		"audio_alert voxmail,",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in config:\n%s", want, text)
		}
	}
	accounts, err := os.ReadFile(filepath.Join(dir, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	if string(accounts) != line+"\n" {
		t.Fatalf("unexpected accounts file %q", string(accounts))
	}
	if info, err := os.Stat(filepath.Join(dir, "accounts")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("accounts permissions: %v %v", info, err)
	}
}

func TestWriteConfigRemovesAccountsWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	b := &Baresip{ConfigDir: dir, MaxCalls: 10}
	if err := b.writeConfig(store.SIPSettings{Port: 5060}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "accounts")); !os.IsNotExist(err) {
		t.Fatalf("expected accounts file to be removed, got err %v", err)
	}
}