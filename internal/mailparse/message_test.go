package mailparse

import (
	"strings"
	"testing"
)

func TestParseMultipart(t *testing.T) {
	raw := "From: Ada <ada@example.com>\r\nSubject: =?UTF-8?Q?Hello?=\r\nContent-Type: multipart/mixed; boundary=xyz\r\n\r\n--xyz\r\nContent-Type: text/plain\r\n\r\nHello 25%\r\n--xyz\r\nContent-Type: audio/mpeg\r\nContent-Disposition: attachment; filename=note.mp3\r\n\r\nignored\r\n--xyz--\r\n"
	m, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Hello" || !strings.Contains(m.Text, "percent") || len(m.Attachments) != 1 || !m.Attachments[0].Playable {
		t.Fatalf("unexpected message: %+v", m)
	}
}
