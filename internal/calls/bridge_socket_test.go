package calls

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitForBridgeSocket(t *testing.T) {
	dir := t.TempDir()
	svc := &Service{Socket: filepath.Join(dir, "baresip.sock")}

	// A missing socket waits, but cancellation must end the wait early.
	started := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(600*time.Millisecond, cancel)
	svc.waitForBridgeSocket(ctx)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("missing socket wait ignored cancellation: %v", elapsed)
	}

	// An existing path returns immediately.
	if err := os.WriteFile(svc.Socket, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	svc.waitForBridgeSocket(context.Background())
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("existing socket did not return immediately: %v", elapsed)
	}

	// An empty path returns immediately.
	empty := &Service{}
	started = time.Now()
	empty.waitForBridgeSocket(context.Background())
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("empty socket path did not return immediately: %v", elapsed)
	}
}
