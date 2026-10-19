package mailparse

import (
	"strings"
	"testing"
)

func mustLenient(t *testing.T, raw string) Message {
	t.Helper()
	message, err := ParseLenient(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("lenient parse failed: %v", err)
	}
	if !message.Recovered {
		t.Fatal("expected Recovered to be set")
	}
	return message
}

// A base64 body with embedded garbage must still yield its decoded prefix.
func TestLenientRecoversCorruptBase64(t *testing.T) {
	message := mustLenient(t, "From: a@example.com\r\nSubject: Hi\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nSGVsbG8g###@@@d29ybGQ=")
	if !strings.Contains(message.OriginalText, "Hello") {
		t.Fatalf("recovered text %q missing decoded prefix", message.OriginalText)
	}
}

// A multipart message whose final boundary is missing keeps the parts that
// were readable instead of failing the whole message.
func TestLenientRecoversTruncatedMultipart(t *testing.T) {
	raw := "From: a@example.com\r\nSubject: Hi\r\nContent-Type: multipart/mixed; boundary=BOUND\r\n\r\n--BOUND\r\nContent-Type: text/plain\r\n\r\nfirst part text\r\n--BOUND\r\nContent-Type: text/plain\r\n\r\nsecond part never terminated"
	message := mustLenient(t, raw)
	if !strings.Contains(message.OriginalText, "first part text") {
		t.Fatalf("recovered text %q missing first part", message.OriginalText)
	}
}

// Bare LF line endings parse like standard MIME text.
func TestLenientAcceptsBareLFHeaders(t *testing.T) {
	message := mustLenient(t, "From: a@example.com\nSubject: Hi there\nContent-Type: text/plain\n\nplain body")
	if message.Subject != "Hi there" {
		t.Fatalf("subject %q", message.Subject)
	}
	if !strings.Contains(message.OriginalText, "plain body") {
		t.Fatalf("body %q", message.OriginalText)
	}
}

// A missing blank line between headers and body still yields both.
func TestLenientSplitsMergedHeaderBody(t *testing.T) {
	message := mustLenient(t, "From: a@example.com\nSubject: Hi\nthis line has no colon and starts the body\nmore body")
	if message.Subject != "Hi" {
		t.Fatalf("subject %q", message.Subject)
	}
	if !strings.Contains(message.OriginalText, "more body") {
		t.Fatalf("body %q", message.OriginalText)
	}
}

// A file truncated mid-header still recovers the headers parsed so far.
func TestLenientRecoversTruncatedHeaders(t *testing.T) {
	message := mustLenient(t, "From: a@example.com\nSubject: Half a header with")
	if message.Subject != "Half a header with" {
		t.Fatalf("subject %q", message.Subject)
	}
}

// Size limits still bind in lenient mode.
func TestLenientStillRejectsOversize(t *testing.T) {
	big := "From: a@example.com\nSubject: Hi\n\n" + strings.Repeat("x", maxMessageBytes+64)
	if _, err := ParseLenient(strings.NewReader(big)); err != ErrMessageTooLarge {
		t.Fatalf("expected ErrMessageTooLarge, got %v", err)
	}
}

// A quoted-printable body that dies on a trailing escape keeps the bytes
// decoded so far instead of failing the whole part.
func TestLenientSalvagesTruncatedQuotedPrintable(t *testing.T) {
	message := mustLenient(t, "From: a@example.com\r\nSubject: Hi\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nHello world==")
	if !strings.Contains(message.OriginalText, "Hello world") {
		t.Fatalf("recovered text %q missing salvaged prefix", message.OriginalText)
	}
}

// An attachment whose quoted-printable body is truncated still contributes
// its decoded prefix as an attachment.
func TestLenientSalvagesAttachmentDecodeError(t *testing.T) {
	raw := "From: a@example.com\r\nSubject: Hi\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=data.bin\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nQUJDRA=\r"
	message := mustLenient(t, raw)
	if len(message.Attachments) != 1 {
		t.Fatalf("attachments=%d, want the salvaged attachment", len(message.Attachments))
	}
	attachment := message.Attachments[0]
	if attachment.Name != "data.bin" {
		t.Fatalf("attachment name=%q", attachment.Name)
	}
	if attachment.Size == 0 {
		t.Fatal("salvaged attachment lost its decoded prefix")
	}
	if string(attachment.Data) != "QUJDRA" {
		t.Fatalf("salvaged attachment data=%q", string(attachment.Data))
	}
}

// The same truncation inside a multipart body: the attachment survives the
// truncated closing boundary and its truncated trailing escape.
func TestLenientSalvagesTruncatedMultipartAttachment(t *testing.T) {
	raw := "From: a@example.com\r\nSubject: Hi\r\nContent-Type: multipart/mixed; boundary=BOUND\r\n\r\n--BOUND\r\nContent-Type: text/plain\r\n\r\nbody text\r\n--BOUND\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=a.bin\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nQUJDRA=\r\n--BOU"
	message := mustLenient(t, raw)
	if !strings.Contains(message.OriginalText, "body text") {
		t.Fatalf("recovered text %q missing first part", message.OriginalText)
	}
	if len(message.Attachments) != 1 {
		t.Fatalf("attachments=%d, want the salvaged attachment", len(message.Attachments))
	}
	if !strings.Contains(string(message.Attachments[0].Data), "QUJDRA") {
		t.Fatalf("salvaged attachment data=%q", string(message.Attachments[0].Data))
	}
}

// A malformed trailing escape inside the multipart attachment is salvaged by
// the lenient body readers instead of failing the part.
func TestLenientSalvagesMultipartAttachmentTrailingEscape(t *testing.T) {
	raw := "From: a@example.com\r\nSubject: Hi\r\nContent-Type: multipart/mixed; boundary=BOUND\r\n\r\n--BOUND\r\nContent-Type: text/plain\r\n\r\nbody text\r\n--BOUND\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=a.bin\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nQUJDRA==\r\n--BOU"
	message := mustLenient(t, raw)
	if len(message.Attachments) != 1 {
		t.Fatalf("attachments=%d, want the salvaged attachment", len(message.Attachments))
	}
	if !strings.Contains(string(message.Attachments[0].Data), "QUJDRA") {
		t.Fatalf("salvaged attachment data=%q", string(message.Attachments[0].Data))
	}
}

// A body-level limit failure must not drop the message: the fallback keeps
// the headers and a bounded slice of the raw body as readable text.
func TestLenientFallbackKeepsBodyTextOnOversizePart(t *testing.T) {
	raw := "From: a@example.com\nSubject: big\n\n" + strings.Repeat("y", maxTextBytes+1024)
	message, err := ParseLenient(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("oversize part must not fail the lenient parse: %v", err)
	}
	if !message.Recovered {
		t.Fatal("expected Recovered to be set")
	}
	if len(message.OriginalText) != maxTextBytes {
		t.Fatalf("salvaged body length=%d, want bounded %d", len(message.OriginalText), maxTextBytes)
	}
}

// In metadata mode the same fallback keeps the headers without retaining
// body text.
func TestLenientMetadataFallbackKeepsHeadersOnOversizePart(t *testing.T) {
	raw := "From: a@example.com\nSubject: big metadata\n\n" + strings.Repeat("y", maxTextBytes+1024)
	message, err := ParseLenientMetadata(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("oversize part must not fail the lenient metadata parse: %v", err)
	}
	if message.Subject != "big metadata" {
		t.Fatalf("subject %q", message.Subject)
	}
	if message.OriginalText != "" {
		t.Fatalf("metadata mode retained %d bytes of body text", len(message.OriginalText))
	}
}
