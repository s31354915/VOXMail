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

// pumpGrace bounds the join loop after teardown begins. Closing a descriptor
// does not reliably interrupt a blocked read on every pipe/socketpair shape
// (observed as mbsync's stdin staying blocked while the socket side was
// already gone), so the watchdog must never depend on the pumps exiting.
// When the grace expires the caller force-terminates the process; exit closes
// every descriptor and mbsync's poll on the socketpair finally sees EOF.
var pumpGrace = 2 * time.Second

var errIdleTimeout = errors.New("IMAP tunnel inactivity timeout")
var errForceTerminate = errors.New("IMAP tunnel teardown: pumps could not be interrupted, forcing exit")

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
	// Keepalive probes detect a silently dropped middlebox path while the
	// process itself looks healthy; without them the socket only fails on
	// the next write, which mbsync may not perform for a long time.
	if tc, ok := conn.(*net.TCPConn); ok {
		if err := tc.SetKeepAliveConfig(net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 30 * time.Second}); err != nil {
			fmt.Fprintf(os.Stderr, "keepalive: %v\n", err)
		}
	}
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
				// A past deadline unblocks a blocked socket read even when
				// Close alone cannot.
				_ = tracked.SetReadDeadline(time.Now())
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
	graceTimer := time.NewTimer(pumpGrace)
	defer graceTimer.Stop()
	for completed < 2 {
		select {
		case err := <-results:
			completed++
			if first == nil || isClosedNetworkError(first) {
				first = err
			}
		case <-graceTimer.C:
			// One pump survived teardown and Close could not interrupt it.
			// Waiting longer reproduces the indefinite deadlock; returning
			// the sentinel lets main exit non-zero and release everything.
			return errForceTerminate
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
