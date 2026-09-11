package mailparse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMultipart(t *testing.T) {
	raw := "From: Ada <ada@example.com>\r\nSubject: =?UTF-8?Q?Hello?=\r\nContent-Type: multipart/mixed; boundary=xyz\r\n\r\n--xyz\r\nContent-Type: text/plain\r\n\r\nHello 25%\r\n--xyz\r\nContent-Type: audio/mpeg\r\nContent-Disposition: attachment; filename=note.mp3\r\n\r\nignored\r\n--xyz--\r\n"
	m, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Hello" || !strings.Contains(m.Text, "percent") || len(m.Attachments) != 1 || !m.Attachments[0].Playable || string(m.Attachments[0].Data) != "ignored" {
		t.Fatalf("unexpected message: %+v", m)
	}
}

func TestParseNestedAlternativeKeepsReadableTextAndAttachment(t *testing.T) {
	raw := "From: Ada <ada@example.com>\r\n" +
		"Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\n" +
		"Content-Type: multipart/alternative; boundary=inner\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain\r\n\r\nplain body\r\n" +
		"--inner\r\nContent-Type: text/html\r\n\r\n<b>html body</b>\r\n" +
		"--inner--\r\n" +
		"--outer\r\nContent-Type: audio/ogg\r\nContent-Disposition: attachment; filename=note.ogg\r\n\r\naudio\r\n" +
		"--outer--\r\n"
	m, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Text, "plain body") || len(m.Attachments) != 1 || !m.Attachments[0].Playable {
		t.Fatalf("nested alternative was not preserved: text=%q attachments=%+v", m.Text, m.Attachments)
	}
}

func TestParseCapsAttachmentCountDuringTraversal(t *testing.T) {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=x\r\n\r\n")
	for i := 0; i < maxAttachments+20; i++ {
		b.WriteString("--x\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=f\r\n\r\nx\r\n")
	}
	b.WriteString("--x--\r\n")
	m, err := Parse(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Attachments) != maxAttachments {
		t.Fatalf("attachment count=%d, want %d", len(m.Attachments), maxAttachments)
	}
}

func TestScanPreservesNestedFolderPath(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Projects", "VOXMail", "new")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "message:2,")
	if err := os.WriteFile(path, []byte("From: sender@example.com\r\nSubject: nested\r\n\r\nhello\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	messages, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Folder != "Projects/VOXMail" {
		t.Fatalf("unexpected nested folder result: %+v", messages)
	}
}
