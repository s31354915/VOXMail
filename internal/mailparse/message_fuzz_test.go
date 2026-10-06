package mailparse

import (
	"bytes"
	"testing"
)

func FuzzParseMetadataNeverPanics(f *testing.F) {
	for _, seed := range []string{
		"From: sender@example.com\r\n\r\nhello\r\n",
		"Content-Type: multipart/mixed; boundary=x\r\n\r\n--x--\r\n",
		"Content-Type: text/plain\r\nContent-Disposition: attachment; filename=x.txt\r\n\r\nbody\r\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		_, _ = ParseMetadata(bytes.NewReader(input))
	})
}
