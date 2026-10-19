package mailparse

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/textproto"
	"strings"

	"github.com/voxmail/voxmail/internal/speech"
)

// ParseLenient recovers as much as possible from a damaged message: corrupt
// base64, truncated multipart bodies, bare LF line endings, and header blocks
// that stop mid-line. The strict parser stays the primary path; callers
// should only reach for this after a message failed the strict parse, so a
// recovered result reflects the best usable projection of a file a strict
// read would have skipped entirely.
func ParseLenient(r io.Reader) (Message, error) {
	return parseLenient(r, true)
}

// ParseLenientMetadata is the bounded-memory variant of ParseLenient: it
// never retains attachment payloads.
func ParseLenientMetadata(r io.Reader) (Message, error) {
	return parseLenient(r, false)
}

func parseLenient(r io.Reader, retainData bool) (Message, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxMessageBytes+1))
	if err != nil {
		return Message{}, err
	}
	if len(raw) > maxMessageBytes {
		return Message{}, ErrMessageTooLarge
	}
	raw = normalizeLineEndings(raw)
	fields, body := lenientHeaderFields(raw)
	parsed := textproto.MIMEHeader(fields)
	message := Message{
		MessageID: parsed.Get("Message-Id"),
		Subject:   decodeHeader(parsed.Get("Subject")),
		From:      parsed.Get("From"),
		To:        parsed.Get("To"),
		Cc:        parsed.Get("Cc"),
		Date:      parsed.Get("Date"),
		Recovered: true,
	}
	budget := &parseBudget{retainData: retainData, retainText: retainData}
	message.OriginalText, message.Attachments, err = parsePart(parsed, bytes.NewReader(body), budget, 0, true)
	if err != nil {
		return message, err
	}
	if len(message.Attachments) > maxAttachments {
		message.Attachments = message.Attachments[:maxAttachments]
	}
	message.Text = speech.EmailToSpeech(message.OriginalText)
	return message, nil
}

// normalizeLineEndings rewrites lone LF and lone CR bytes to CRLF so
// truncated or Unix-line-ended mail parses like standard MIME text.
func normalizeLineEndings(raw []byte) []byte {
	needs := false
	for i, b := range raw {
		if b == '\n' && (i == 0 || raw[i-1] != '\r') {
			needs = true
			break
		}
		if b == '\r' && (i+1 >= len(raw) || raw[i+1] != '\n') {
			needs = true
			break
		}
	}
	if !needs {
		return raw
	}
	out := make([]byte, 0, len(raw)+16)
	for i, b := range raw {
		switch b {
		case '\r':
			out = append(out, '\r')
			if i+1 >= len(raw) || raw[i+1] != '\n' {
				out = append(out, '\n')
			}
		case '\n':
			if len(out) == 0 || out[len(out)-1] != '\r' {
				out = append(out, '\r')
			}
			out = append(out, '\n')
		default:
			out = append(out, b)
		}
	}
	return out
}

// lenientHeaderFields splits a raw message into header fields and body
// without requiring a well-formed blank-line separator. A line that carries
// no colon and is not a folded continuation ends the header block, so a
// missing blank line between headers and body still yields both. Malformed
// header lines are skipped rather than fatal.
func lenientHeaderFields(raw []byte) (textproto.MIMEHeader, []byte) {
	fields := textproto.MIMEHeader{}
	lines := bytes.Split(raw, []byte("\n"))
	lastKey := ""
	for i, rawLine := range lines {
		line := bytes.TrimSuffix(rawLine, []byte("\r"))
		if len(line) == 0 {
			return fields, joinRawLines(lines[i+1:])
		}
		if line[0] == ' ' || line[0] == '\t' {
			if lastKey != "" {
				values := fields[textproto.CanonicalMIMEHeaderKey(lastKey)]
				if len(values) > 0 {
					values[len(values)-1] += " " + strings.TrimSpace(string(line))
				}
			}
			continue
		}
		idx := bytes.IndexByte(line, ':')
		if idx <= 0 {
			return fields, joinRawLines(lines[i:])
		}
		key := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(string(line[:idx])))
		if key == "" {
			return fields, joinRawLines(lines[i:])
		}
		fields.Add(key, strings.TrimSpace(string(line[idx+1:])))
		lastKey = key
	}
	return fields, nil
}

func joinRawLines(lines [][]byte) []byte {
	if len(lines) == 0 {
		return nil
	}
	return bytes.Join(lines, []byte("\n"))
}

const b64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// lenientBase64Reader decodes base64 content that a strict decoder rejects:
// embedded whitespace and arbitrary garbage are dropped, URL-safe alphabets
// are normalized, and a corrupt or unpadded tail yields the bytes decoded so
// far instead of failing the whole stream. Decoding never errors; a part
// that cannot be decoded at all simply contributes nothing.
type lenientBase64Reader struct {
	src   io.Reader
	carry []byte
	out   []byte
	eof   bool
	err   error
}

func newLenientBase64Reader(src io.Reader) *lenientBase64Reader {
	return &lenientBase64Reader{src: src}
}

func (l *lenientBase64Reader) Read(p []byte) (int, error) {
	for len(l.out) == 0 {
		if l.err != nil {
			return 0, l.err
		}
		if l.eof {
			return 0, io.EOF
		}
		if err := l.fill(); err != nil {
			l.err = err
			return 0, err
		}
	}
	n := copy(p, l.out)
	l.out = l.out[n:]
	return n, nil
}

func (l *lenientBase64Reader) fill() error {
	chunk := make([]byte, 8192)
	n, readErr := l.src.Read(chunk)
	for _, b := range chunk[:n] {
		switch {
		case b == '=':
			// Padding ends the stream: anything after it is garbage.
			l.flushFinal()
			l.eof = true
			return nil
		case b == '-':
			l.carry = append(l.carry, '+')
		case b == '_':
			l.carry = append(l.carry, '/')
		default:
			if strings.IndexByte(b64Alphabet, b) >= 0 {
				l.carry = append(l.carry, b)
			}
			// Whitespace and arbitrary invalid bytes are dropped.
		}
	}
	for len(l.carry) >= 4 {
		l.decodeQuad()
	}
	if readErr != nil {
		if readErr == io.EOF {
			l.flushFinal()
			l.eof = true
			return nil
		}
		return readErr
	}
	return nil
}

// flushFinal decodes any complete quads the carry still holds, then the
// trailing 2 or 3 characters with synthetic padding. A lone trailing
// character cannot carry any bytes and is dropped.
func (l *lenientBase64Reader) flushFinal() {
	for len(l.carry) >= 4 {
		l.decodeQuad()
	}
	if len(l.carry) >= 2 {
		quad := make([]byte, 4)
		copy(quad, l.carry)
		switch len(l.carry) {
		case 2:
			quad[2], quad[3] = '=', '='
		case 3:
			quad[3] = '='
		}
		if decoded, err := base64.StdEncoding.DecodeString(string(quad)); err == nil {
			l.out = append(l.out, decoded...)
		}
	}
	l.carry = nil
}

func (l *lenientBase64Reader) decodeQuad() {
	quad := l.carry[:4]
	if decoded, err := base64.StdEncoding.DecodeString(string(quad)); err == nil {
		l.out = append(l.out, decoded...)
	}
	// A quad that still fails to decode is dropped rather than fatal.
	l.carry = l.carry[4:]
}
