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
