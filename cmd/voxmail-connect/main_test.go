package main

import (
	"bytes"
	"io"
	"net"
	"testing"
)

func TestProxyRelaysBothDirectionsAndClosesOnPeerShutdown(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	input, inputWriter := io.Pipe()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- proxy(local, input, &output) }()

	if _, err := inputWriter.Write([]byte("client payload")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("client payload"))
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "client payload" {
		t.Fatalf("peer received %q", got)
	}
	if _, err := peer.Write([]byte("server payload")); err != nil {
		t.Fatal(err)
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if output.String() != "server payload" {
		t.Fatalf("client received %q", output.String())
	}
}
