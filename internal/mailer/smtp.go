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

const smtpOperationTimeout = 60 * time.Second

// Check authenticates to an SMTP submission service without sending a
// message. Port 465 uses implicit TLS; 587 uses STARTTLS. Port 25 is rejected
// for this authenticated-submission check.
func Check(ctx context.Context, c Config) error {
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
	dialer := &net.Dialer{}
	address := c.Address
	if address == "" {
		address = net.JoinHostPort(c.Host, fmt.Sprint(c.Port))
	}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("SMTP connection failed: %w", err)
	}
	if security == "implicit_tls" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return fmt.Errorf("SMTP TLS handshake failed: %w", err)
		}
		conn = tlsConn
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("SMTP protocol failed: %w", err)
	}
	defer client.Close()
	if security == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("SMTP server does not advertise STARTTLS")
		}
		if err := client.StartTLS(&tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("SMTP STARTTLS failed: %w", err)
		}
	}
	if err := client.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
		return fmt.Errorf("SMTP authentication failed: %w", err)
	}
	return client.Quit()
}

func Send(c Config, to []string, raw []byte) error {
	if c.Host == "" || c.Port < 1 || len(to) == 0 || len(raw) == 0 || c.From == "" {
		return fmt.Errorf("incomplete SMTP request")
	}
	if c.Port == 25 && !c.AllowPlaintext25 {
		return fmt.Errorf("SMTP port 25 requires explicit plaintext opt-in")
	}
	security, err := smtpSecurity(c.Security, c.Port, c.AllowPlaintext25)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), smtpOperationTimeout)
	defer cancel()
	conn, err := netguard.DialContext(ctx, c.Host, c.Port, c.Address, c.AllowPrivate)
	if err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	if security == "implicit_tls" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return err
		}
		conn = tlsConn
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer client.Close()
	if security == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("SMTP server does not advertise STARTTLS")
		}
		if err := client.StartTLS(&tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if c.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return err
		}
	}
	if err := client.Mail(c.From); err != nil {
		return err
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = writer.Write(raw); err != nil {
		_ = writer.Close()
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	return client.Quit()
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
	return BuildMessageWithAttachments(from, sender, to, cc, bcc, subject, body, nil)
}

// BuildMessageWithAttachments creates a standards-compliant UTF-8 MIME
// message. Bcc recipients are deliberately used only by the SMTP envelope;
// they are never emitted as a header. This keeps the old BuildMessage API
// useful while giving phone composition a safe attachment path.
func BuildMessageWithAttachments(from, sender string, to, cc, bcc []string, subject, body string, attachments []Attachment) []byte {
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
	messageID, _ := newMessageID(from)
	lines = append(lines,
		"Subject: "+encodedSubject,
		"Date: "+time.Now().UTC().Format(time.RFC1123Z),
		"Message-ID: "+messageID,
		"MIME-Version: 1.0")
	if len(attachments) == 0 {
		lines = append(lines, "Content-Type: text/plain; charset=UTF-8", "", normalizeBody(body))
		return []byte(strings.Join(lines, "\r\n"))
	}
	var out bytes.Buffer
	out.WriteString(strings.Join(lines, "\r\n"))
	out.WriteString("\r\nContent-Type: multipart/mixed; boundary=")
	writer := multipart.NewWriter(&out)
	out.WriteString(writer.Boundary())
	out.WriteString("\r\n\r\n")
	textHeader := make(textproto.MIMEHeader)
	textHeader.Set("Content-Type", "text/plain; charset=UTF-8")
	part, err := writer.CreatePart(textHeader)
	if err != nil {
		return nil
	}
	_, _ = part.Write([]byte(normalizeBody(body)))
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
			return nil
		}
		encoded := make([]byte, base64.StdEncoding.EncodedLen(len(attachment.Data)))
		base64.StdEncoding.Encode(encoded, attachment.Data)
		for len(encoded) > 0 {
			n := 57
			if n > len(encoded) {
				n = len(encoded)
			}
			_, _ = part.Write(encoded[:n])
			_, _ = part.Write([]byte("\r\n"))
			encoded = encoded[n:]
		}
	}
	_ = writer.Close()
	return out.Bytes()
}

func normalizeBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	return strings.ReplaceAll(body, "\n", "\r\n")
}

func newMessageID(from string) (string, error) {
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
