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
	"sync"
	"time"

	"github.com/voxmail/voxmail/internal/netguard"
)

const connectTimeout = 30 * time.Second

// Bound silence, not the total duration of a large initial download.
const idleTimeout = 5 * time.Minute

var errIdleTimeout = errors.New("IMAP tunnel inactivity timeout")

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

// Both local streams must unblock pending I/O when closed (as the process's
// stdin/stdout pipes do). Closing only the socket cannot stop a stdin reader.
func proxy(conn net.Conn, input io.ReadCloser, output io.WriteCloser) error {
	return proxyWithTimeout(conn, input, output, idleTimeout)
}

func proxyWithTimeout(conn net.Conn, input io.ReadCloser, output io.WriteCloser, idle time.Duration) error {
	tracked := &progressConn{Conn: conn, last: time.Now()}
	results := make(chan error, 2)
	go func() { _, err := io.Copy(tracked, input); results <- err }()
	go func() { _, err := io.Copy(output, tracked); results <- err }()
	timer := time.NewTimer(idle)
	defer timer.Stop()
	var first error
	completed := 0
wait:
	for {
		select {
		case first = <-results:
			completed++
			break wait
		case <-timer.C:
			remaining := tracked.remaining(idle)
			if remaining <= 0 {
				first = errIdleTimeout
				break wait
			}
			timer.Reset(remaining)
		}
	}
	// Close all three endpoints before joining either pump. This also releases
	// a writer blocked on a client which has stopped consuming server data.
	_ = conn.Close()
	_ = input.Close()
	_ = output.Close()
	for completed < 2 {
		err := <-results
		completed++
		if first == nil || isClosedNetworkError(first) {
			first = err
		}
	}
	if isClosedNetworkError(first) {
		return nil
	}
	return first
}

type progressConn struct {
	net.Conn
	mu   sync.Mutex
	last time.Time
}

func (c *progressConn) progress(n int) {
	if n > 0 {
		c.mu.Lock()
		c.last = time.Now()
		c.mu.Unlock()
	}
}
func (c *progressConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.progress(n)
	return n, err
}
func (c *progressConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.progress(n)
	return n, err
}
func (c *progressConn) remaining(idle time.Duration) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return idle - time.Since(c.last)
}

func isClosedNetworkError(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed)
}
