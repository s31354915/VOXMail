package mailparse

import (
	"errors"
	"fmt"
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
	_, err := Parse(strings.NewReader(b.String()))
	if !errors.Is(err, ErrAttachmentLimit) {
		t.Fatalf("error=%v, want ErrAttachmentLimit", err)
	}
}

func TestParseClassifiesTextAttachmentAndPreservesOriginalBody(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain\r\n\r\nPrice is $5.\r\n" +
		"--x\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=notes.txt\r\n\r\nAttached $7.\r\n" +
		"--x--\r\n"
	m, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(m.OriginalText) != "Price is $5." {
		t.Fatalf("original text=%q", m.OriginalText)
	}
	if !strings.Contains(m.Text, "dollars") {
		t.Fatalf("speech text=%q", m.Text)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Name != "notes.txt" || strings.TrimSpace(string(m.Attachments[0].Data)) != "Attached $7." {
		t.Fatalf("attachments=%+v", m.Attachments)
	}
}

func TestParseRejectsTruncatedOuterMessage(t *testing.T) {
	input := strings.NewReader("Content-Type: application/octet-stream\r\n\r\n" + strings.Repeat("x", maxMessageBytes))
	_, err := Parse(input)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("error=%v, want ErrMessageTooLarge", err)
	}
}

func TestParseRejectsMalformedEncodedAttachment(t *testing.T) {
	raw := "Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nnot base64!"
	if _, err := Parse(strings.NewReader(raw)); err == nil {
		t.Fatal("malformed base64 attachment was accepted")
	}
}

func TestParseMetadataDoesNotRetainAttachmentBytes(t *testing.T) {
	raw := "Content-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=x.bin\r\n\r\nbytes"
	m, err := ParseMetadata(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Size != 5 || len(m.Attachments[0].Data) != 0 {
		t.Fatalf("metadata attachment=%+v", m.Attachments)
	}
}

func TestParseDecodesLegacyCharsetForReadableText(t *testing.T) {
	raw := "Content-Type: text/plain; charset=iso-8859-1\r\n\r\n" + string([]byte{'c', 'a', 'f', 0xe9})
	m, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.OriginalText != "café" {
		t.Fatalf("decoded text=%q, want café", m.OriginalText)
	}
}

func TestParseRejectsOversizedHeaders(t *testing.T) {
	raw := "X-Large: " + strings.Repeat("x", maxHeaderBytes) + "\r\n\r\nbody"
	_, err := Parse(strings.NewReader(raw))
	if !errors.Is(err, ErrHeaderTooLarge) {
		t.Fatalf("error=%v, want ErrHeaderTooLarge", err)
	}
}

func TestParseRejectsExcessiveMIMENesting(t *testing.T) {
	body := "Content-Type: text/plain\r\n\r\nbody\r\n"
	for level := maxMultipartDepth + 1; level >= 0; level-- {
		body = fmt.Sprintf("Content-Type: multipart/mixed; boundary=b%d\r\n\r\n--b%d\r\n%s--b%d--\r\n", level, level, body, level)
	}
	_, err := Parse(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "MIME nesting exceeds") {
		t.Fatalf("error=%v, want nesting-limit error", err)
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

// A multipart body that stops mid part keeps the readable parts in strict
// mode too: the part reader surfaces a swallowed unexpected EOF as a
// premature boundary EOF ("multipart: NextPart: EOF") from the next
// NextRawPart call, and a message that parsed up to the cut must stay
// indexable instead of failing the whole parse.
func TestParseKeepsPartsOnPrematureBoundaryEOF(t *testing.T) {
	for _, tc := range []struct{ name, first, body string }{
		{"truncated base64 attachment", "first part text", "--BOUND\r\nContent-Type: text/plain\r\n\r\nfirst part text\r\n--BOUND\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8gd29ybG"},
		{"second part body cut", "one", "--BOUND\r\nContent-Type: text/plain\r\n\r\none\r\n--BOUND\r\nContent-Type: application/octet-stream\r\n\r\npartial"},
	} {
		raw := "From: a@example.com\r\nSubject: Hi\r\nContent-Type: multipart/mixed; boundary=BOUND\r\n\r\n" + tc.body
		m, err := Parse(strings.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: strict parse dropped a partially readable message: %v", tc.name, err)
		}
		if !strings.Contains(m.OriginalText, tc.first) {
			t.Fatalf("%s: first part lost, text=%q", tc.name, m.OriginalText)
		}
	}
	// A text part cut mid body still fails strict (its read error is not a
	// boundary EOF) and recovers through the lenient pass.
	truncatedText := "From: a@example.com\r\nSubject: Hi\r\nContent-Type: multipart/mixed; boundary=BOUND\r\n\r\n--BOUND\r\nContent-Type: text/plain\r\n\r\nfirst part text\r\n--BOUND\r\nContent-Type: text/plain\r\n\r\npartial"
	if _, err := Parse(strings.NewReader(truncatedText)); err == nil {
		t.Fatal("strict parse accepted a text part cut mid body")
	}
	m, err := ParseLenient(strings.NewReader(truncatedText))
	if err != nil {
		t.Fatalf("lenient recovery failed: %v", err)
	}
	if !m.Recovered || !strings.Contains(m.OriginalText, "first part text") {
		t.Fatalf("lenient lost the readable part: recovered=%v text=%q", m.Recovered, m.OriginalText)
	}
}

func TestParseLenientKeepsTruncatedAttachment(t *testing.T) {
	// A truncated base64 attachment (partially written message file, no
	// closing MIME boundary) previously made the strict parse fail with
	// "decode MIME attachment: unexpected EOF" and made the lenient
	// recovery drop the part entirely. The recovery must keep the decoded
	// prefix so the mail still indexes with its attachment.
	raw := "From: Ada <ada@example.com>\r\n" +
		"Content-Type: multipart/mixed; boundary=xyz\r\n\r\n" +
		"--xyz\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"body text\r\n" +
		"--xyz\r\n" +
		"Content-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=doc.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"aGVsbG8gd29ybG"
	m, err := ParseLenient(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("lenient recovery should read a truncated attachment: %v", err)
	}
	if !strings.Contains(m.Text, "body text") {
		t.Fatalf("body text lost: %q", m.Text)
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("expected the partial attachment to survive, got %+v", m.Attachments)
	}
	if got := string(m.Attachments[0].Data); got != "hello wor" {
		t.Fatalf("unexpected partial attachment data %q", got)
	}
}

func TestParseLenientKeepsCorruptedAttachment(t *testing.T) {
	// When the closing boundary IS present but the base64 payload is
	// corrupted (or cut mid-quantum before it), the strict parser fails on
	// "illegal base64 data" and the lenient recovery pass must still read
	// the message, keeping the decoded prefix instead of dropping the mail.
	raw := "From: Ada <ada@example.com>\r\n" +
		"Content-Type: multipart/mixed; boundary=xyz\r\n\r\n" +
		"--xyz\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"body text\r\n" +
		"--xyz\r\n" +
		"Content-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=doc.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"aGVsbG8gd29ybG--xyz--\r\n"
	if _, err := Parse(strings.NewReader(raw)); err == nil {
		t.Fatal("strict parse should still report the corrupted attachment")
	}
	m, err := ParseLenient(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("lenient parse should recover a corrupted attachment: %v", err)
	}
	if !strings.Contains(m.Text, "body text") {
		t.Fatalf("body text lost: %q", m.Text)
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("expected the partial attachment to survive, got %+v", m.Attachments)
	}
}
