package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func TestProxyRelaysBothDirectionsAndClosesOnPeerShutdown(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	input, inputWriter := io.Pipe()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- proxy(local, input, bufferCloser{&output}) }()

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

// bytes.Buffer is only inspected after proxy joins both pumps.
type bufferCloser struct{ *bytes.Buffer }

func (bufferCloser) Close() error { return nil }

func awaitProxy(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not terminate")
		return nil
	}
}

func TestProxyPeerCloseUnblocksOpenStdin(t *testing.T) {
	local, peer := net.Pipe()
	input, writer := io.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	go func() { done <- proxy(local, input, bufferCloser{new(bytes.Buffer)}) }()
	peer.Close()
	if err := awaitProxy(t, done); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("late")); err == nil {
		t.Fatal("stdin was not closed")
	}
}

func TestProxyStdinCloseUnblocksSilentPeer(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	input, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- proxy(local, input, bufferCloser{new(bytes.Buffer)}) }()
	writer.Close()
	if err := awaitProxy(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestProxyIdleTimeoutUnblocksBothPumps(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	input, writer := io.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	go func() { done <- proxyWithTimeout(local, input, bufferCloser{new(bytes.Buffer)}, 50*time.Millisecond) }()
	if err := awaitProxy(t, done); !errors.Is(err, errIdleTimeout) {
		t.Fatalf("error = %v", err)
	}
}

func TestProxyIdleTimeoutUnblocksStalledOutput(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	input, writer := io.Pipe()
	defer writer.Close()
	reader, output := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	go func() { done <- proxyWithTimeout(local, input, output, 100*time.Millisecond) }()
	if _, err := peer.Write([]byte("unconsumed server response")); err != nil {
		t.Fatal(err)
	}
	if err := awaitProxy(t, done); !errors.Is(err, errIdleTimeout) {
		t.Fatalf("error = %v", err)
	}
}

func TestProxyTrafficInEitherDirectionExtendsIdleDeadline(t *testing.T) {
	for _, outbound := range []bool{false, true} {
		t.Run(fmtDirection(outbound), func(t *testing.T) {
			local, peer := net.Pipe()
			defer peer.Close()
			input, writer := io.Pipe()
			defer writer.Close()
			done := make(chan error, 1)
			go func() { done <- proxyWithTimeout(local, input, bufferCloser{new(bytes.Buffer)}, 300*time.Millisecond) }()
			if outbound {
				go io.Copy(io.Discard, peer)
			}
			// Keep transferring longer than the original idle deadline, then stop.
			for i := 0; i < 10; i++ {
				var err error
				if outbound {
					_, err = writer.Write([]byte("x"))
				} else {
					_, err = peer.Write([]byte("x"))
				}
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(50 * time.Millisecond)
			}
			select {
			case err := <-done:
				t.Fatalf("active connection stopped: %v", err)
			default:
			}
			if err := awaitProxy(t, done); !errors.Is(err, errIdleTimeout) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
func fmtDirection(outbound bool) string {
	if outbound {
		return "client-to-server"
	}
	return "server-to-client"
}

// A pump that ignores Close reproduces the reported deadlock shape: the join
// loop used to wait forever on the stdin pump after the idle watchdog tore
// everything down, so the process never exited and mbsync never saw EOF.
type stuckReadCloser struct{}

func (stuckReadCloser) Read(p []byte) (int, error) {
	<-make(chan struct{})
	return 0, io.EOF
}

func (stuckReadCloser) Close() error { return nil }

func TestProxyForceTerminatesWhenStdinPumpCannotBeUnblocked(t *testing.T) {
	original := pumpGrace
	pumpGrace = 150 * time.Millisecond
	t.Cleanup(func() { pumpGrace = original })
	local, peer := net.Pipe()
	defer peer.Close()
	done := make(chan error, 1)
	go func() {
		done <- proxyWithTimeout(local, stuckReadCloser{}, bufferCloser{new(bytes.Buffer)}, 50*time.Millisecond)
	}()
	if err := awaitProxy(t, done); !errors.Is(err, errForceTerminate) {
		t.Fatalf("error = %v, want errForceTerminate", err)
	}
}

func TestProxyForceTerminateIsNeverReturnedWhenPumpsFinish(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	input, writer := io.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	go func() { done <- proxyWithTimeout(local, input, bufferCloser{new(bytes.Buffer)}, 50*time.Millisecond) }()
	if err := awaitProxy(t, done); !errors.Is(err, errIdleTimeout) {
		t.Fatalf("error = %v, want errIdleTimeout", err)
	}
}

func TestProxyTCPHalfCloseUnblocksStdin(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	input, writer := io.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	go func() { done <- proxy(client, input, bufferCloser{new(bytes.Buffer)}) }()
	if err := peer.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := awaitProxy(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestProxyPeerCloseUnblocksOSPipeInput(t *testing.T) {
	local, peer := net.Pipe()
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	outputReader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outputReader.Close()
	done := make(chan error, 1)
	go func() { done <- proxy(local, input, output) }()
	peer.Close()
	if err := awaitProxy(t, done); err != nil {
		t.Fatal(err)
	}
}
