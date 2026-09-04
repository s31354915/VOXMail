package mailer

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

type Config struct {
	Host               string
	Port               int
	Username, Password string
	From               string
}

func Send(c Config, to []string, raw []byte) error {
	if c.Host == "" || c.Port < 1 || len(to) == 0 || len(raw) == 0 {
		return fmt.Errorf("incomplete SMTP request")
	}
	address := net.JoinHostPort(c.Host, fmt.Sprint(c.Port))
	auth := smtp.PlainAuth("", c.Username, c.Password, c.Host)
	if c.Port == 465 {
		conn, err := tls.Dial("tcp", address, &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12})
		if err != nil {
			return err
		}
		client, err := smtp.NewClient(conn, c.Host)
		if err != nil {
			conn.Close()
			return err
		}
		defer client.Close()
		if err := client.Auth(auth); err != nil {
			return err
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
			writer.Close()
			return err
		}
		if err = writer.Close(); err != nil {
			return err
		}
		return client.Quit()
	}
	return smtp.SendMail(address, auth, c.From, to, raw)
}

func BuildMessage(from, sender string, to, cc, bcc []string, subject, body string) []byte {
	clean := func(value string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(value) }
	from, sender, subject = clean(from), clean(sender), clean(subject)
	for i := range to {
		to[i] = clean(to[i])
	}
	for i := range cc {
		cc[i] = clean(cc[i])
	}
	for i := range bcc {
		bcc[i] = clean(bcc[i])
	}
	lines := []string{"From: " + sender + " <" + from + ">", "To: " + strings.Join(to, ", ")}
	if len(cc) > 0 {
		lines = append(lines, "Cc: "+strings.Join(cc, ", "))
	}
	lines = append(lines, "Subject: "+subject, "MIME-Version: 1.0", "Content-Type: text/plain; charset=UTF-8", "", body)
	return []byte(strings.Join(lines, "\r\n"))
}
