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
}
type Message struct {
	MessageID, Subject, From, To, Date, Text string
	Attachments                              []Attachment
}

func Parse(r io.Reader) (Message, error) {
	parsed, err := mail.ReadMessage(bufio.NewReader(io.LimitReader(r, 25<<20)))
	if err != nil {
		return Message{}, err
	}
	message := Message{MessageID: parsed.Header.Get("Message-ID"), Subject: decodeHeader(parsed.Header.Get("Subject")), From: parsed.Header.Get("From"), To: parsed.Header.Get("To"), Date: parsed.Header.Get("Date")}
	message.Text, message.Attachments = parsePart(textproto.MIMEHeader(parsed.Header), parsed.Body)
	message.Text = speech.EmailToSpeech(message.Text)
	return message, nil
}

func parsePart(header textproto.MIMEHeader, body io.Reader) (string, []Attachment) {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		mediaType = "text/plain"
	}
	reader := decodeBody(header, body)
	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		mr := multipart.NewReader(reader, params["boundary"])
		var plain, html string
		var attachments []Attachment
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			text, parts := parsePart(part.Header, part)
			attachments = append(attachments, parts...)
			typ, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
			if strings.HasPrefix(typ, "text/plain") && plain == "" {
				plain = text
			}
			if strings.HasPrefix(typ, "text/html") && html == "" {
				html = text
			}
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
		name := decodeHeader(header.Get("Content-Disposition"))
		if name == "" {
			name = decodeHeader(header.Get("Content-Type"))
		}
		_, params, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
		if params["filename"] != "" {
			name = decodeHeader(params["filename"])
		}
		if filepath.Base(name) == "." {
			name = "attachment"
		}
		return "", []Attachment{{Name: name, ContentType: mediaType, Playable: strings.HasPrefix(mediaType, "audio/") || strings.HasPrefix(mediaType, "video/")}}
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
