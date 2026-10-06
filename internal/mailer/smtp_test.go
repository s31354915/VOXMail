package mailer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestBuildMessageDoesNotMutateInputs(t *testing.T) {
	to := []string{"a@example.com"}
	cc := []string{"c@example.com"}
	bcc := []string{"hidden@example.com"}
	_ = BuildMessage("from@example.com", "Sender", to, cc, bcc, "subject", "body")
	if to[0] != "a@example.com" || cc[0] != "c@example.com" || bcc[0] != "hidden@example.com" {
		t.Fatal("BuildMessage mutated its input slices")
	}
}

func TestBuildMessageBlocksHeaderInjection(t *testing.T) {
	raw := string(BuildMessage("from@example.com", "Sender", []string{"a@ex\r\nBcc: leak@x.com"}, nil, nil, "Sub\r\nCc: leak@x.com", "body"))
	if strings.Contains(raw, "\r\nBcc:") || strings.Contains(raw, "\r\nCc:") {
		t.Fatal("header injection reached the wire")
	}
	if !strings.Contains(raw, "To:") || !strings.Contains(raw, "Subject:") {
		t.Fatal("message missing standard headers")
	}
}

func TestBuildMessageWithAttachmentsUsesMIMEAndHidesBcc(t *testing.T) {
	raw := string(BuildMessageWithAttachments(
		"from@example.com", "Sender", []string{"to@example.com"}, []string{"cc@example.com"}, []string{"hidden@example.com"},
		"Résumé", "hello", []Attachment{{Filename: "clip.wav", ContentType: "audio/wav", Data: []byte("audio")}},
	))
	if strings.Contains(raw, "hidden@example.com") {
		t.Fatal("Bcc address was exposed in MIME headers")
	}
	for _, want := range []string{"multipart/mixed", "clip.wav", "Content-Transfer-Encoding: base64", "YXVkaW8="} {
		if !strings.Contains(raw, want) {
			t.Fatalf("MIME message missing %q: %s", want, raw)
		}
	}
}

func TestBuildMessageRoundTripsUnicodeLongTextAndBinaryAttachment(t *testing.T) {
	body := "Résumé\n" + strings.Repeat("A long line with Unicode café and URL https://example.com/path?x=1&y=2.\n", 20)
	attachment := []byte{0x00, 0x01, 0x7f, 0x80, 0xfe, 0xff}
	raw, err := BuildMessageWithAttachmentsE("from@example.com", "Séndér", []string{"to@example.com"}, []string{"cc@example.com"}, []string{"hidden@example.com"}, "日本語 subject", body, []Attachment{{Filename: "binary.bin", ContentType: "application/octet-stream", Data: attachment}})
	if err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject")); err != nil || got != "日本語 subject" {
		t.Fatalf("subject=%q err=%v", got, err)
	}
	if message.Header.Get("Message-ID") == "" || message.Header.Get("Bcc") != "" || !strings.Contains(message.Header.Get("To"), "to@example.com") || !strings.Contains(message.Header.Get("Cc"), "cc@example.com") {
		t.Fatalf("headers=%v", message.Header)
	}
	typ, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || typ != "multipart/mixed" {
		t.Fatalf("content type=%q params=%v err=%v", typ, params, err)
	}
	reader := multipart.NewReader(message.Body, params["boundary"])
	textPart, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	textData, err := io.ReadAll(textPart)
	if err != nil {
		t.Fatal(err)
	}
	decodedText, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(textData)))
	if err != nil || string(decodedText) != normalizeBody(body) {
		t.Fatalf("decoded body mismatch err=%v", err)
	}
	attachmentPart, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	decodedAttachment, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, attachmentPart))
	if err != nil || !bytes.Equal(decodedAttachment, attachment) {
		t.Fatalf("decoded attachment=%v err=%v", decodedAttachment, err)
	}
}

func TestBuildMessageWithAttachmentsIDUsesStableValidIdentifier(t *testing.T) {
	wantID := "<attempt-123@example.com>"
	raw, err := BuildMessageWithAttachmentsID("from@example.com", "Sender", []string{"to@example.com"}, nil, nil, "subject", "body", nil, wantID)
	if err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := message.Header.Get("Message-ID"); got != wantID {
		t.Fatalf("Message-ID=%q, want %q", got, wantID)
	}
	if _, err := BuildMessageWithAttachmentsID("from@example.com", "Sender", nil, nil, nil, "subject", "body", nil, "<bad\r\nX: injected>"); err == nil {
		t.Fatal("header-injection Message-ID was accepted")
	}
}

func TestNewMessageIDUsesSenderDomain(t *testing.T) {
	id, err := NewMessageID("Sender <person@example.net>")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(id, "@example.net>") || !strings.HasPrefix(id, "<") {
		t.Fatalf("generated Message-ID=%q", id)
	}
}

func TestSendToLocalServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	var received []byte
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		fmt.Fprintf(conn, "220 local ESMTP\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "HELO"):
				fmt.Fprintf(conn, "250 OK\r\n")
			case strings.HasPrefix(line, "EHLO"):
				fmt.Fprintf(conn, "250-example\r\n250-AUTH PLAIN\r\n250 OK\r\n")
			case strings.HasPrefix(line, "AUTH"):
				fmt.Fprintf(conn, "235 ok\r\n")
			case strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
				fmt.Fprintf(conn, "250 OK\r\n")
			case line == "DATA":
				fmt.Fprintf(conn, "354 go ahead\r\n")
				var buf bytes.Buffer
				for {
					l, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimRight(l, "\r\n") == "." {
						break
					}
					buf.WriteString(l)
				}
				received = buf.Bytes()
				fmt.Fprintf(conn, "250 queued\r\n")
			case line == "QUIT":
				fmt.Fprintf(conn, "221 bye\r\n")
				return
			}
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	err = Send(Config{Host: "127.0.0.1", Port: port, From: "a@example.com", Security: "plaintext", AllowPlaintext25: true, AllowPrivate: true}, []string{"b@example.com"}, []byte("Subject: hi\r\n\r\nbody"))
	<-serverDone
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !bytes.Contains(received, []byte("Subject: hi")) || !bytes.Contains(received, []byte("body")) {
		t.Fatalf("server received unexpected message: %q", received)
	}
}

func TestSendRejectsIncompleteConfig(t *testing.T) {
	if err := Send(Config{}, nil, nil); err == nil {
		t.Fatal("Send accepted an empty configuration")
	}
}

func TestSendOutcomeTreatsQUITFailureAsAccepted(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 local ESMTP\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				fmt.Fprint(conn, "250 local\r\n")
			case strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
				fmt.Fprint(conn, "250 OK\r\n")
			case line == "DATA":
				fmt.Fprint(conn, "354 go\r\n")
				for {
					bodyLine, readErr := reader.ReadString('\n')
					if readErr != nil {
						return
					}
					if strings.TrimRight(bodyLine, "\r\n") == "." {
						break
					}
				}
				fmt.Fprint(conn, "250 accepted\r\n")
			case line == "QUIT":
				return
			}
		}
	}()
	result := SendWithOutcome(Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, From: "a@example.com", Security: "plaintext", AllowPlaintext25: true, AllowPrivate: true}, []string{"b@example.com"}, []byte("Subject: hi\r\n\r\nbody"))
	<-done
	if result.Status != SendAccepted || result.CleanupError == nil {
		t.Fatalf("outcome=%+v, want accepted with cleanup error", result)
	}
}

func TestSendOutcomeReportsUncertainDATACompletion(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 local ESMTP\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				fmt.Fprint(conn, "250 local\r\n")
			case strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
				fmt.Fprint(conn, "250 OK\r\n")
			case line == "DATA":
				fmt.Fprint(conn, "354 go\r\n")
				for {
					bodyLine, readErr := reader.ReadString('\n')
					if readErr != nil {
						return
					}
					if strings.TrimRight(bodyLine, "\r\n") == "." {
						return
					}
				}
			}
		}
	}()
	result := SendWithOutcome(Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, From: "a@example.com", Security: "plaintext", AllowPlaintext25: true, AllowPrivate: true}, []string{"b@example.com"}, []byte("Subject: hi\r\n\r\nbody"))
	<-done
	if result.Status != SendUncertain || result.Err == nil {
		t.Fatalf("outcome=%+v, want uncertain DATA result", result)
	}
}

func TestSendOutcomeReportsPreDATADisconnectAsRejected(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		reader := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 local ESMTP\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				_ = conn.Close()
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				fmt.Fprint(conn, "250 local\r\n")
			case strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
				fmt.Fprint(conn, "250 OK\r\n")
			case line == "DATA":
				// The transaction has not entered DATA yet. A disconnect here
				// is a definite rejection, not an ambiguous acceptance.
				_ = conn.Close()
				return
			}
		}
	}()
	result := SendWithOutcome(Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, From: "a@example.com", Security: "plaintext", AllowPlaintext25: true, AllowPrivate: true}, []string{"b@example.com"}, []byte("Subject: hi\r\n\r\nbody"))
	<-done
	if result.Status != SendRejected || result.Err == nil {
		t.Fatalf("outcome=%+v, want rejected pre-DATA disconnect", result)
	}
}

func TestCheckNeverGreetingHonorsParentDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- Check(ctx, Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Username: "user", Password: "password", Security: "plaintext", AllowPlaintext25: true})
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Check accepted a server that never sent an SMTP greeting")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Check exceeded its parent cancellation deadline")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("never-greeting server connection was not closed")
	}
}

func TestSendWithOutcomeContextCancelsNeverGreetingServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := SendWithOutcomeContext(ctx, Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, From: "a@example.com", Security: "plaintext", AllowPlaintext25: true, AllowPrivate: true}, []string{"b@example.com"}, []byte("Subject: hi\r\n\r\nbody"))
	if result.Status != SendRejected || result.Err == nil {
		t.Fatalf("outcome=%+v, want rejected cancelled greeting", result)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("never-greeting submission connection was not closed")
	}
}

func TestCheckStalledTLSHonorsParentDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- Check(ctx, Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Username: "user", Password: "password", Security: "implicit_tls"})
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Check accepted a TLS peer that never completed its handshake")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled TLS check exceeded its parent cancellation deadline")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stalled TLS connection was not closed")
	}
}

func TestCheckStalledAuthHonorsParentDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		fmt.Fprint(conn, "220 local ESMTP\r\n")
		line, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(strings.TrimRight(line, "\r\n"), "EHLO") {
			return
		}
		fmt.Fprint(conn, "250-local\r\n250 AUTH PLAIN\r\n")
		_, _ = io.Copy(io.Discard, conn)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- Check(ctx, Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Username: "user", Password: "password", Security: "plaintext", AllowPlaintext25: true})
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Check authenticated against a server that never completed AUTH")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled AUTH check exceeded its parent cancellation deadline")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stalled AUTH connection was not closed")
	}
}
