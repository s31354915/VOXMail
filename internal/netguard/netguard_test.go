package netguard

import (
	"context"
	"net"
	"testing"
)

func TestDialRejectsLoopbackByDefault(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if conn, err := DialContext(context.Background(), "127.0.0.1", port, "", false); err == nil {
		conn.Close()
		t.Fatal("loopback dial was allowed")
	}
}

func TestDialAllowsExplicitPrivateEndpointForTests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			close(accepted)
			_ = conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	conn, err := DialContext(context.Background(), "127.0.0.1", port, "", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	<-accepted
}
