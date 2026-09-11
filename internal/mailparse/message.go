package mailparse

import (
	"bufio"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"path/filepath"
	"strings"

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
	MessageID, Subject, From, To, Cc, Date, Text string
	Attachments                                  []Attachment
}

const (
	maxAttachments          = 128
	maxAttachmentBytes      = 50 << 20
	maxTotalAttachmentBytes = 100 << 20
)

type parseBudget struct {
	count int
	bytes int64
}

func Parse(r io.Reader) (Message, error) {
	parsed, err := mail.ReadMessage(bufio.NewReader(io.LimitReader(r, 25<<20)))
	if err != nil {
		return Message{}, err
	}
	message := Message{MessageID: parsed.Header.Get("Message-ID"), Subject: decodeHeader(parsed.Header.Get("Subject")), From: parsed.Header.Get("From"), To: parsed.Header.Get("To"), Cc: parsed.Header.Get("Cc"), Date: parsed.Header.Get("Date")}
	budget := &parseBudget{}
	message.Text, message.Attachments = parsePart(textproto.MIMEHeader(parsed.Header), parsed.Body, budget)
	if len(message.Attachments) > maxAttachments {
		message.Attachments = message.Attachments[:maxAttachments]
	}
	message.Text = speech.EmailToSpeech(message.Text)
	return message, nil
}

func parsePart(header textproto.MIMEHeader, body io.Reader, budget *parseBudget) (string, []Attachment) {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		mediaType = "text/plain"
	}
	reader := decodeBody(header, body)
	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		mr := multipart.NewReader(reader, params["boundary"])
		var plain, html, nested string
		var attachments []Attachment
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			text, parts := parsePart(part.Header, part, budget)
			attachments = append(attachments, parts...)
			typ, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
			if strings.HasPrefix(typ, "text/plain") && plain == "" {
				plain = text
			}
			if strings.HasPrefix(typ, "text/html") && html == "" {
				html = text
			}
			// A multipart/alternative is commonly nested inside a
			// multipart/mixed message. Its selected body is returned by the
			// recursive parse, even though its direct type is multipart/*.
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
		return plain, attachments
	case strings.HasPrefix(mediaType, "text/plain"):
		data, _ := io.ReadAll(io.LimitReader(reader, 4<<20))
		return string(data), nil
	case strings.HasPrefix(mediaType, "text/html"):
		data, _ := io.ReadAll(io.LimitReader(reader, 4<<20))
		return string(data), nil
	default:
		if budget != nil && (budget.count >= maxAttachments || budget.bytes >= maxTotalAttachmentBytes) {
			return "", nil
		}
		disposition, dispositionParams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
		name := decodeHeader(dispositionParams["filename"])
		if name == "" {
			_, typeParams, _ := mime.ParseMediaType(header.Get("Content-Type"))
			name = decodeHeader(typeParams["name"])
		}
		if name == "" {
			name = "attachment"
		}
		name = filepath.Base(name)
		if name == "." || name == "" || name == string(filepath.Separator) {
			name = "attachment"
		}
		limit := int64(maxAttachmentBytes)
		if budget != nil {
			remaining := maxTotalAttachmentBytes - budget.bytes
			if remaining < limit {
				limit = remaining
			}
		}
		if limit <= 0 {
			return "", nil
		}
		data, err := io.ReadAll(io.LimitReader(reader, limit+1))
		truncated := err != nil || int64(len(data)) > limit
		if truncated {
			// Do not retain a potentially huge or malformed payload. Keep a
			// metadata entry so the caller can announce that the attachment
			// exists, but make it non-playable because the bytes are incomplete.
			data = nil
		}
		size := int64(len(data))
		if truncated {
			size = limit + 1
		}
		if budget != nil {
			budget.count++
			if truncated {
				budget.bytes += limit
			} else {
				budget.bytes += int64(len(data))
			}
		}
		return "", []Attachment{{
			Name: name, ContentType: mediaType, Size: size, Data: data,
			ContentID: header.Get("Content-ID"), Disposition: disposition, Inline: strings.EqualFold(disposition, "inline"),
			Playable: !truncated && (strings.HasPrefix(mediaType, "audio/") || strings.HasPrefix(mediaType, "video/")),
		}}
	}
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
