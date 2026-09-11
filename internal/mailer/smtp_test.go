package mailer

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"strings"
	"testing"
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
