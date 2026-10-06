package mailparse

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"path/filepath"
	"strings"

	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"

	"github.com/voxmail/voxmail/internal/speech"
)

type Attachment struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Playable    bool   `json:"playable"`
	ContentID   string `json:"content_id,omitempty"`
	Disposition string `json:"disposition,omitempty"`
	Inline      bool   `json:"inline,omitempty"`
	Data        []byte `json:"-"`
}

type Message struct {
	MessageID, Subject, From, To, Cc, Date string
	// Text is the deterministic speech-safe presentation retained for existing
	// callers. OriginalText is the readable, non-speech-normalized body and is
	// the value to use when forwarding or otherwise preserving mail content.
	Text, OriginalText string
	Attachments        []Attachment
}

const (
	maxMessageBytes         = 25 << 20
	maxTextBytes            = 4 << 20
	maxAttachments          = 128
	maxAttachmentBytes      = 50 << 20
	maxTotalAttachmentBytes = 100 << 20
	maxHeaderBytes          = 256 << 10
	maxMultipartDepth       = 32
	maxMultipartParts       = 2048
)

var (
	ErrMessageTooLarge = errors.New("message exceeds parser size limit")
	ErrPartTooLarge    = errors.New("MIME part exceeds parser size limit")
	ErrAttachmentLimit = errors.New("MIME attachment budget exceeded")
	ErrHeaderTooLarge  = errors.New("message headers exceed parser size limit")
)

type parseBudget struct {
	count      int
	bytes      int64
	parts      int
	retainData bool
	retainText bool
}

// Parse reads a bounded message before handing it to net/mail. Reading one
// byte beyond the limit is intentional: it distinguishes a complete message
// at the limit from a silently truncated one.
func Parse(r io.Reader) (Message, error) {
	return parseMessage(r, true)
}

// ParseMetadata validates and extracts the body/attachment metadata without
// retaining attachment payloads. Indexing and retention scans should use this
// mode so one large mailbox cannot keep every attachment in memory.
func ParseMetadata(r io.Reader) (Message, error) {
	return parseMessage(r, false)
}

func parseMessage(r io.Reader, retainData bool) (Message, error) {
	limited := &countingReader{reader: io.LimitReader(r, maxMessageBytes+1)}
	headerLimited := &headerLimitReader{reader: limited}
	parsed, err := mail.ReadMessage(bufio.NewReader(headerLimited))
	if headerLimited.err != nil {
		return Message{}, headerLimited.err
	}
	if err != nil {
		return Message{}, err
	}
	message := Message{
		MessageID: parsed.Header.Get("Message-ID"),
		Subject:   decodeHeader(parsed.Header.Get("Subject")),
		From:      parsed.Header.Get("From"),
		To:        parsed.Header.Get("To"),
		Cc:        parsed.Header.Get("Cc"),
		Date:      parsed.Header.Get("Date"),
	}
	budget := &parseBudget{retainData: retainData, retainText: retainData}
	message.OriginalText, message.Attachments, err = parsePart(textproto.MIMEHeader(parsed.Header), parsed.Body, budget, 0)
	if err != nil {
		return Message{}, err
	}
	if limited.n > maxMessageBytes {
		return Message{}, ErrMessageTooLarge
	}
	if len(message.Attachments) > maxAttachments {
		message.Attachments = message.Attachments[:maxAttachments]
	}
	message.Text = speech.EmailToSpeech(message.OriginalText)
	return message, nil
}

func parsePart(header textproto.MIMEHeader, body io.Reader, budget *parseBudget, depth int) (string, []Attachment, error) {
	if depth > maxMultipartDepth {
		return "", nil, fmt.Errorf("MIME nesting exceeds %d levels", maxMultipartDepth)
	}
	if budget != nil {
		budget.parts++
		if budget.parts > maxMultipartParts {
			return "", nil, fmt.Errorf("MIME part count exceeds %d", maxMultipartParts)
		}
	}
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		if strings.TrimSpace(header.Get("Content-Type")) != "" {
			return "", nil, fmt.Errorf("parse Content-Type: %w", err)
		}
		mediaType = "text/plain"
	}
	mediaType = strings.ToLower(mediaType)
	disposition, dispositionParams, dispositionErr := mime.ParseMediaType(header.Get("Content-Disposition"))
	if dispositionErr != nil && strings.TrimSpace(header.Get("Content-Disposition")) != "" {
		return "", nil, fmt.Errorf("parse Content-Disposition: %w", dispositionErr)
	}
	filename := decodeHeader(dispositionParams["filename"])
	if filename == "" {
		filename = decodeHeader(params["name"])
	}

	reader := decodeBody(header, body)
	// A text part with a filename or attachment disposition is still an
	// attachment. Classification must happen before body selection, otherwise
	// text attachments disappear from metadata.
	isAttachment := filename != "" || strings.EqualFold(disposition, "attachment")
	if isAttachment || (!strings.HasPrefix(mediaType, "text/") && !strings.HasPrefix(mediaType, "multipart/")) {
		return readAttachment(header, reader, mediaType, disposition, filename, budget)
	}

	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		boundary := params["boundary"]
		if boundary == "" {
			return "", nil, fmt.Errorf("multipart Content-Type has no boundary")
		}
		mr := multipart.NewReader(reader, boundary)
		var plain, html, nested string
		var attachments []Attachment
		for {
			part, nextErr := mr.NextPart()
			if nextErr == io.EOF {
				break
			}
			if nextErr != nil {
				return "", nil, fmt.Errorf("read multipart part: %w", nextErr)
			}
			text, parts, partErr := parsePart(part.Header, part, budget, depth+1)
			if partErr != nil {
				return "", nil, partErr
			}
			attachments = append(attachments, parts...)
			typ, _, typeErr := mime.ParseMediaType(part.Header.Get("Content-Type"))
			if typeErr != nil {
				typ = "text/plain"
			}
			typ = strings.ToLower(typ)
			if strings.HasPrefix(typ, "text/plain") && plain == "" {
				plain = text
			}
			if strings.HasPrefix(typ, "text/html") && html == "" {
				html = text
			}
			if strings.HasPrefix(typ, "multipart/") && nested == "" && text != "" {
				nested = text
			}
		}
		if plain == "" {
			plain = nested
		}
		if plain == "" {
			plain = html
		}
		return plain, attachments, nil
	case mediaType == "text/plain", mediaType == "text/html":
		reader = decodeTextBody(header, reader)
		if budget != nil && !budget.retainText {
			decoded, readErr := io.Copy(io.Discard, io.LimitReader(reader, maxTextBytes+1))
			if readErr != nil {
				return "", nil, readErr
			}
			if decoded > maxTextBytes {
				return "", nil, fmt.Errorf("%w: text part exceeds %d bytes", ErrPartTooLarge, maxTextBytes)
			}
			return "", nil, nil
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, maxTextBytes+1))
		if readErr != nil {
			return "", nil, readErr
		}
		if len(data) > maxTextBytes {
			return "", nil, fmt.Errorf("%w: text part exceeds %d bytes", ErrPartTooLarge, maxTextBytes)
		}
		return string(data), nil, nil
	default:
		return readAttachment(header, reader, mediaType, disposition, filename, budget)
	}
}

func readAttachment(header textproto.MIMEHeader, reader io.Reader, mediaType, disposition, filename string, budget *parseBudget) (string, []Attachment, error) {
	if budget != nil {
		if budget.count >= maxAttachments {
			return "", nil, fmt.Errorf("%w: more than %d attachments", ErrAttachmentLimit, maxAttachments)
		}
		if budget.bytes >= maxTotalAttachmentBytes {
			return "", nil, fmt.Errorf("%w: more than %d decoded bytes", ErrAttachmentLimit, maxTotalAttachmentBytes)
		}
	}
	if filename == "" {
		_, params, _ := mime.ParseMediaType(header.Get("Content-Type"))
		filename = decodeHeader(params["name"])
	}
	if filename == "" {
		filename = "attachment"
	}
	filename = filepath.Base(filename)
	if filename == "." || filename == "" || filename == string(filepath.Separator) {
		filename = "attachment"
	}
	limit := int64(maxAttachmentBytes)
	if budget != nil {
		remaining := maxTotalAttachmentBytes - budget.bytes
		if remaining < limit {
			limit = remaining
		}
	}
	if limit <= 0 {
		return "", nil, nil
	}
	var data []byte
	var size int64
	var err error
	if budget != nil && !budget.retainData {
		size, err = io.Copy(io.Discard, io.LimitReader(reader, limit+1))
	} else {
		data, err = io.ReadAll(io.LimitReader(reader, limit+1))
		size = int64(len(data))
	}
	if err != nil {
		return "", nil, fmt.Errorf("decode MIME attachment: %w", err)
	}
	truncated := size > limit
	if truncated {
		return "", nil, fmt.Errorf("%w: attachment %s exceeds %d bytes", ErrPartTooLarge, filename, limit)
	}
	if budget != nil {
		budget.count++
		budget.bytes += size
	}
	if budget == nil || budget.retainData {
		return "", []Attachment{{
			Name: filename, ContentType: mediaType, Size: size, Data: data,
			ContentID: header.Get("Content-ID"), Disposition: disposition,
			Inline:   strings.EqualFold(disposition, "inline"),
			Playable: strings.HasPrefix(mediaType, "audio/") || strings.HasPrefix(mediaType, "video/"),
		}}, nil
	}
	return "", []Attachment{{
		Name: filename, ContentType: mediaType, Size: size,
		ContentID: header.Get("Content-ID"), Disposition: disposition,
		Inline:   strings.EqualFold(disposition, "inline"),
		Playable: strings.HasPrefix(mediaType, "audio/") || strings.HasPrefix(mediaType, "video/"),
	}}, nil
}

func decodeBody(header textproto.MIMEHeader, body io.Reader) io.Reader {
	switch strings.ToLower(header.Get("Content-Transfer-Encoding")) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		return quotedprintable.NewReader(body)
	default:
		return body
	}
}

func decodeTextBody(header textproto.MIMEHeader, body io.Reader) io.Reader {
	_, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		return body
	}
	charset := strings.TrimSpace(params["charset"])
	if charset == "" || strings.EqualFold(charset, "utf-8") || strings.EqualFold(charset, "utf8") || strings.EqualFold(charset, "us-ascii") || strings.EqualFold(charset, "ascii") {
		return body
	}
	enc, err := htmlindex.Get(charset)
	if err != nil {
		// Unknown labels are kept byte-for-byte. Rejecting otherwise readable
		// mail is worse than allowing the speech layer to handle replacement
		// characters for an unrecognised charset.
		return body
	}
	return transform.NewReader(body, enc.NewDecoder())
}

type countingReader struct {
	reader io.Reader
	n      int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.n += int64(n)
	return n, err
}

type headerLimitReader struct {
	reader io.Reader
	err    error
	done   bool
	bytes  int
	tail   []byte
}

func (r *headerLimitReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.reader.Read(p)
	if n == 0 || r.done {
		return n, err
	}
	for _, b := range p[:n] {
		r.bytes++
		r.tail = append(r.tail, b)
		if len(r.tail) > 4 {
			r.tail = r.tail[len(r.tail)-4:]
		}
		if bytes.HasSuffix(r.tail, []byte("\n\n")) || bytes.HasSuffix(r.tail, []byte("\r\n\r\n")) || bytes.HasSuffix(r.tail, []byte("\r\r")) {
			r.done = true
			break
		}
		if r.bytes > maxHeaderBytes {
			r.err = ErrHeaderTooLarge
			return 0, r.err
		}
	}
	return n, err
}

func decodeHeader(value string) string {
	if value == "" {
		return ""
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}
