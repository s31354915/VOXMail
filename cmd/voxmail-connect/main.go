// voxmail-connect is the fixed-address tunnel used by mbsync. The address is
// deliberately numeric: mbsync keeps the provider hostname for TLS identity,
// while this process prevents a second DNS lookup from changing the socket
// destination after policy validation.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/voxmail/voxmail/internal/netguard"
)

const connectTimeout = 30 * time.Second

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: voxmail-connect <validated-ip:port>")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	conn, err := netguard.DialAddressContext(ctx, os.Args[1], false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	if err := proxy(conn, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "proxy: %v\n", err)
		os.Exit(1)
	}
}

func proxy(conn net.Conn, input io.Reader, output io.Writer) error {
	results := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, input)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		results <- err
	}()
	go func() {
		_, err := io.Copy(output, conn)
		results <- err
	}()
	first := <-results
	_ = conn.Close()
	second := <-results
	if first != nil && !isClosedNetworkError(first) {
		return first
	}
	if second != nil && !isClosedNetworkError(second) {
		return second
	}
	return nil
}

func isClosedNetworkError(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}
