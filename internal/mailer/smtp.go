package mailer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/netguard"
)

type Config struct {
	Host string
	// Address is an optional already-validated IP:port endpoint used by
	// onboarding checks to avoid a DNS-rebinding gap. Host remains TLS SNI.
	Address            string
	Port               int
	Security           string
	Username, Password string
	From               string
	// AllowPlaintext25 must be explicitly enabled by a trusted caller. The
	// normal web/account path never sets it, so port 25 cannot silently turn
	// into unauthenticated or plaintext submission.
	AllowPlaintext25 bool
	// AllowPrivate is reserved for local integration tests or an explicitly
	// isolated deployment. It is false for normal account operations.
	AllowPrivate bool
}

const (
	// The dial budget is deliberately shorter than the whole SMTP operation so
	// a resolver or unreachable endpoint cannot consume the entire exchange
	// budget before protocol work starts.
	smtpConnectionTimeout = 15 * time.Second
	// net/smtp has no context-aware command methods. We apply this deadline to
	// the underlying connection before every protocol operation.
	smtpCommandTimeout = 30 * time.Second
	// This is the hard upper bound for one check or message submission.
	smtpOperationTimeout = 60 * time.Second
)

type SendStatus string

const (
	SendRejected  SendStatus = "rejected"
	SendAccepted  SendStatus = "accepted"
	SendUncertain SendStatus = "uncertain"
)

type SendResult struct {
	Status       SendStatus
	Err          error
	CleanupError error
}

// Check authenticates to an SMTP submission service without sending a
// message. Port 465 uses implicit TLS; 587 uses STARTTLS. Port 25 is rejected
// for this authenticated-submission check.
func Check(ctx context.Context, c Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	operationCtx, cancel := context.WithTimeout(ctx, smtpOperationTimeout)
	defer cancel()
	if c.Host == "" || c.Port < 1 || c.Port > 65535 || c.Username == "" {
		return fmt.Errorf("incomplete SMTP settings")
	}
	security, err := smtpSecurity(c.Security, c.Port, c.AllowPlaintext25)
	if err != nil {
		return err
	}
	if c.Port == 25 && !c.AllowPlaintext25 {
		return fmt.Errorf("SMTP port 25 is disabled for authenticated submission")
	}
	dialCtx, dialCancel := context.WithTimeout(operationCtx, smtpConnectionTimeout)
	defer dialCancel()
	dialer := &net.Dialer{Timeout: smtpConnectionTimeout}
	address := c.Address
	if address == "" {
		address = net.JoinHostPort(c.Host, fmt.Sprint(c.Port))
	}
	conn, err := dialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return fmt.Errorf("SMTP connection failed: %w", err)
	}
	if err := setSMTPDeadline(conn, operationCtx); err != nil {
		_ = conn.Close()
		return err
	}
	if security == "implicit_tls" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12})
		commandCtx, commandCancel := context.WithTimeout(operationCtx, smtpCommandTimeout)
		err := tlsConn.HandshakeContext(commandCtx)
		commandCancel()
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("SMTP TLS handshake failed: %w", err)
		}
		conn = tlsConn
	}
	if err := setSMTPDeadline(conn, operationCtx); err != nil {
		_ = conn.Close()
		return err
	}
	stopClose := context.AfterFunc(operationCtx, func() { _ = conn.Close() })
	defer stopClose()
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("SMTP protocol failed: %w", err)
	}
	defer client.Close()
	if security == "starttls" {
		if err := setSMTPDeadline(conn, operationCtx); err != nil {
			return err
		}
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("SMTP server does not advertise STARTTLS")
		}
		if err := setSMTPDeadline(conn, operationCtx); err != nil {
			return err
		}
		if err := client.StartTLS(&tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("SMTP STARTTLS failed: %w", err)
		}
	}
	if err := setSMTPDeadline(conn, operationCtx); err != nil {
		return err
	}
	if err := client.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
		return fmt.Errorf("SMTP authentication failed: %w", err)
	}
	if err := setSMTPDeadline(conn, operationCtx); err != nil {
		return err
	}
	return client.Quit()
}

func Send(c Config, to []string, raw []byte) error {
	result := SendWithOutcome(c, to, raw)
	if result.Status == SendAccepted {
		return nil
	}
	return result.Err
}

// SendWithOutcome separates SMTP message acceptance from post-DATA cleanup.
// Once the DATA terminator receives a success response, the server has
// accepted the message; a later QUIT failure must not cause a caller to retry.
func SendWithOutcome(c Config, to []string, raw []byte) SendResult {
	return SendWithOutcomeContext(context.Background(), c, to, raw)
}

// SendWithOutcomeContext is the cancellable form of SendWithOutcome. The
// context closes the underlying connection so a caller does not have to wait
// for a protocol read deadline when abandoning a submission.
func SendWithOutcomeContext(ctx context.Context, c Config, to []string, raw []byte) SendResult {
	if c.Host == "" || c.Port < 1 || len(to) == 0 || len(raw) == 0 || c.From == "" {
		return SendResult{Status: SendRejected, Err: fmt.Errorf("incomplete SMTP request")}
	}
	if c.Port == 25 && !c.AllowPlaintext25 {
		return SendResult{Status: SendRejected, Err: fmt.Errorf("SMTP port 25 requires explicit plaintext opt-in")}
	}
	security, err := smtpSecurity(c.Security, c.Port, c.AllowPlaintext25)
	if err != nil {
		return SendResult{Status: SendRejected, Err: err}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	operationCtx, cancel := context.WithTimeout(ctx, smtpOperationTimeout)
	defer cancel()
	dialCtx, dialCancel := context.WithTimeout(operationCtx, smtpConnectionTimeout)
	defer dialCancel()
	conn, err := netguard.DialContext(dialCtx, c.Host, c.Port, c.Address, c.AllowPrivate)
	if err != nil {
		return SendResult{Status: SendRejected, Err: err}
	}
	if err := setSMTPDeadline(conn, operationCtx); err != nil {
		_ = conn.Close()
		return SendResult{Status: SendRejected, Err: err}
	}
	stopClose := context.AfterFunc(operationCtx, func() { _ = conn.Close() })
	defer stopClose()
	if security == "implicit_tls" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12})
		commandCtx, commandCancel := context.WithTimeout(operationCtx, smtpCommandTimeout)
		err := tlsConn.HandshakeContext(commandCtx)
		commandCancel()
		if err != nil {
			_ = conn.Close()
			return SendResult{Status: SendRejected, Err: err}
		}
		conn = tlsConn
	}
	if err := setSMTPDeadline(conn, operationCtx); err != nil {
		return SendResult{Status: SendRejected, Err: err}
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		_ = conn.Close()
		return SendResult{Status: SendRejected, Err: err}
	}
	defer client.Close()
	if security == "starttls" {
		if err := setSMTPDeadline(conn, operationCtx); err != nil {
			return SendResult{Status: SendRejected, Err: err}
		}
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return SendResult{Status: SendRejected, Err: fmt.Errorf("SMTP server does not advertise STARTTLS")}
		}
		if err := setSMTPDeadline(conn, operationCtx); err != nil {
			return SendResult{Status: SendRejected, Err: err}
		}
		if err := client.StartTLS(&tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return SendResult{Status: SendRejected, Err: err}
		}
	}
	if c.Username != "" {
		if err := setSMTPDeadline(conn, operationCtx); err != nil {
			return SendResult{Status: SendRejected, Err: err}
		}
		if err := client.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return SendResult{Status: SendRejected, Err: err}
		}
	}
	if err := setSMTPDeadline(conn, operationCtx); err != nil {
		return SendResult{Status: SendRejected, Err: err}
	}
	if err := client.Mail(c.From); err != nil {
		return SendResult{Status: SendRejected, Err: err}
	}
	for _, recipient := range to {
		if err := setSMTPDeadline(conn, operationCtx); err != nil {
			return SendResult{Status: SendRejected, Err: err}
		}
		if err := client.Rcpt(recipient); err != nil {
			return SendResult{Status: SendRejected, Err: err}
		}
	}
	if err := setSMTPDeadline(conn, operationCtx); err != nil {
		return SendResult{Status: SendRejected, Err: err}
	}
	writer, err := client.Data()
	if err != nil {
		return SendResult{Status: SendRejected, Err: err}
	}
	if err := setSMTPDeadline(conn, operationCtx); err != nil {
		_ = writer.Close()
		return SendResult{Status: SendRejected, Err: err}
	}
	if _, err = writer.Write(raw); err != nil {
		_ = writer.Close()
		return SendResult{Status: SendUncertain, Err: err}
	}
	if err = writer.Close(); err != nil {
		return SendResult{Status: SendUncertain, Err: err}
	}
	if err := setSMTPDeadline(conn, operationCtx); err != nil {
		return SendResult{Status: SendAccepted, CleanupError: err}
	}
	if err := client.Quit(); err != nil {
		return SendResult{Status: SendAccepted, CleanupError: err}
	}
	return SendResult{Status: SendAccepted}
}

func setSMTPDeadline(conn net.Conn, ctx context.Context) error {
	if conn == nil {
		return fmt.Errorf("SMTP connection is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(smtpCommandTimeout)
	if operationDeadline, ok := ctx.Deadline(); ok && operationDeadline.Before(deadline) {
		deadline = operationDeadline
	}
	return conn.SetDeadline(deadline)
}

func smtpSecurity(value string, port int, allowPlaintext bool) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		if port == 465 {
			return "implicit_tls", nil
		}
		return "starttls", nil
	}
	if value == "plaintext" {
		if !allowPlaintext {
			return "", fmt.Errorf("plaintext SMTP transport requires explicit opt-in")
		}
		return value, nil
	}
	if value != "implicit_tls" && value != "starttls" {
		return "", fmt.Errorf("unsupported SMTP security mode")
	}
	return value, nil
}

type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

func BuildMessage(from, sender string, to, cc, bcc []string, subject, body string) []byte {
	raw, _ := BuildMessageWithAttachmentsE(from, sender, to, cc, bcc, subject, body, nil)
	return raw
}

// BuildMessageWithAttachments creates a standards-compliant UTF-8 MIME
// message. Bcc recipients are deliberately used only by the SMTP envelope;
// they are never emitted as a header. This keeps the old BuildMessage API
// useful while giving phone composition a safe attachment path.
func BuildMessageWithAttachments(from, sender string, to, cc, bcc []string, subject, body string, attachments []Attachment) []byte {
	raw, _ := BuildMessageWithAttachmentsE(from, sender, to, cc, bcc, subject, body, attachments)
	return raw
}

// BuildMessageWithAttachmentsID builds a message using the supplied stable
// Message-ID. Submission journals use this form so a later reconciliation can
// identify exactly which message the SMTP server may have accepted.
func BuildMessageWithAttachmentsID(from, sender string, to, cc, bcc []string, subject, body string, attachments []Attachment, messageID string) ([]byte, error) {
	return buildMessageWithAttachments(from, sender, to, cc, bcc, subject, body, attachments, messageID)
}

// BuildMessageWithAttachmentsE is the error-returning MIME builder used by
// send paths. The compatibility wrapper above cannot expose entropy or writer
// failures without breaking its historical signature.
func BuildMessageWithAttachmentsE(from, sender string, to, cc, bcc []string, subject, body string, attachments []Attachment) ([]byte, error) {
	return buildMessageWithAttachments(from, sender, to, cc, bcc, subject, body, attachments, "")
}

func buildMessageWithAttachments(from, sender string, to, cc, bcc []string, subject, body string, attachments []Attachment, suppliedMessageID string) ([]byte, error) {
	clean := func(value string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(value) }
	from, sender, subject = clean(from), clean(sender), clean(subject)
	cleanList := func(input []string) []string {
		out := make([]string, len(input))
		for i, recipient := range input {
			out[i] = clean(recipient)
		}
		return out
	}
	to, cc, bcc = cleanList(to), cleanList(cc), cleanList(bcc)
	fromHeader := from
	if sender != "" {
		fromHeader = (&mail.Address{Name: sender, Address: from}).String()
	}
	lines := []string{"From: " + fromHeader, "To: " + strings.Join(to, ", ")}
	if len(cc) > 0 {
		lines = append(lines, "Cc: "+strings.Join(cc, ", "))
	}
	encodedSubject := mime.QEncoding.Encode("UTF-8", subject)
	messageID := strings.TrimSpace(suppliedMessageID)
	if messageID == "" {
		var err error
		messageID, err = NewMessageID(from)
		if err != nil {
			return nil, fmt.Errorf("create Message-ID: %w", err)
		}
	} else if !validMessageID(messageID) {
		return nil, fmt.Errorf("invalid Message-ID")
	}
	lines = append(lines,
		"Subject: "+encodedSubject,
		"Date: "+time.Now().UTC().Format(time.RFC1123Z),
		"Message-ID: "+messageID,
		"MIME-Version: 1.0")
	if len(attachments) == 0 {
		var out bytes.Buffer
		out.WriteString(strings.Join(lines, "\r\n"))
		out.WriteString("\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		writer := quotedprintable.NewWriter(&out)
		if _, err := writer.Write([]byte(normalizeBody(body))); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	}
	var out bytes.Buffer
	out.WriteString(strings.Join(lines, "\r\n"))
	out.WriteString("\r\nContent-Type: multipart/mixed; boundary=")
	writer := multipart.NewWriter(&out)
	out.WriteString(writer.Boundary())
	out.WriteString("\r\n\r\n")
	textHeader := make(textproto.MIMEHeader)
	textHeader.Set("Content-Type", "text/plain; charset=UTF-8")
	textHeader.Set("Content-Transfer-Encoding", "quoted-printable")
	part, err := writer.CreatePart(textHeader)
	if err != nil {
		return nil, err
	}
	textWriter := quotedprintable.NewWriter(part)
	if _, err := textWriter.Write([]byte(normalizeBody(body))); err != nil {
		return nil, err
	}
	if err := textWriter.Close(); err != nil {
		return nil, err
	}
	for _, attachment := range attachments {
		name := clean(attachment.Filename)
		if name == "" {
			name = "attachment"
		}
		contentType := clean(attachment.ContentType)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Type", contentType)
		header.Set("Content-Transfer-Encoding", "base64")
		header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		part, err := writer.CreatePart(header)
		if err != nil {
			return nil, err
		}
		encoded := make([]byte, base64.StdEncoding.EncodedLen(len(attachment.Data)))
		base64.StdEncoding.Encode(encoded, attachment.Data)
		for len(encoded) > 0 {
			n := 76
			if n > len(encoded) {
				n = len(encoded)
			}
			if _, err := part.Write(encoded[:n]); err != nil {
				return nil, err
			}
			if _, err := part.Write([]byte("\r\n")); err != nil {
				return nil, err
			}
			encoded = encoded[n:]
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func normalizeBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	return strings.ReplaceAll(body, "\n", "\r\n")
}

// NewMessageID creates a globally unique RFC 5322-style identifier using the
// sender's domain when available.
func NewMessageID(from string) (string, error) {
	domain := "voxmail.local"
	if address, err := mail.ParseAddress(from); err == nil {
		if at := strings.LastIndexByte(address.Address, '@'); at > 0 && at+1 < len(address.Address) {
			domain = address.Address[at+1:]
		}
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "<" + hex.EncodeToString(b) + "@" + domain + ">", nil
}

func validMessageID(value string) bool {
	if len(value) < 5 || len(value) > 512 || strings.ContainsAny(value, "\r\n\t ") || value[0] != '<' || value[len(value)-1] != '>' {
		return false
	}
	inner := value[1 : len(value)-1]
	if strings.Count(inner, "@") != 1 {
		return false
	}
	for _, r := range inner {
		if r < 0x21 || r > 0x7e || r == '<' || r == '>' {
			return false
		}
	}
	parts := strings.SplitN(inner, "@", 2)
	return parts[0] != "" && parts[1] != ""
}
