package mailsync

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// fakeMbsync returns a path to a shim that exits with exitCode, optionally
// sleeping first.
func fakeMbsync(t *testing.T, exitCode int, sleep string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is unix-only")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "mbsync")
	content := "#!/bin/sh\n[ -n \"" + sleep + "\" ] && sleep " + sleep + "\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(shim, []byte(content), 0700); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	return shim
}

func newRunner(binary string) Runner {
	return Runner{Binary: binary, Timeout: 10 * time.Second}
}

func TestSyncCleanExitIsNotChanged(t *testing.T) {
	r := newRunner(fakeMbsync(t, 0, ""))
	result, err := r.Sync(context.Background(), "config", "work")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Changed {
		t.Fatal("clean exit reported a change")
	}
	if result.Account != "work" {
		t.Fatalf("Account = %q", result.Account)
	}
}

func TestSyncNearSideChange(t *testing.T) {
	r := newRunner(fakeMbsync(t, 32, ""))
	result, err := r.Sync(context.Background(), "config", "home")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !result.Changed {
		t.Fatal("near-side change (32) not reported")
	}
}

func TestSyncFarSideChange(t *testing.T) {
	r := newRunner(fakeMbsync(t, 64, ""))
	result, err := r.Sync(context.Background(), "config", "home")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !result.Changed {
		t.Fatal("far-side change (64) not reported")
	}
}

func TestSyncErrorExitNotChanged(t *testing.T) {
	r := newRunner(fakeMbsync(t, 3, ""))
	result, err := r.Sync(context.Background(), "config", "home")
	if err == nil {
		t.Fatal("exit 3 should surface as an error")
	}
	if result.Changed {
		t.Fatal("error exit was incorrectly reported as a change")
	}
}

func TestSyncRequiresChannel(t *testing.T) {
	r := newRunner(fakeMbsync(t, 0, ""))
	if _, err := r.Sync(context.Background(), "config", ""); err == nil {
		t.Fatal("empty channel accepted")
	}
}

func TestSyncTimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is unix-only")
	}
	r := Runner{Binary: fakeMbsync(t, 0, "2"), Timeout: 100 * time.Millisecond}
	started := time.Now()
	_, err := r.Sync(context.Background(), "config", "home")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("timeout was not honored (took %v)", elapsed)
	}
}

func TestSyncDefaultBinaryAndTimeout(t *testing.T) {
	started := time.Now()
	_, err := (Runner{}).Sync(context.Background(), filepath.Join(t.TempDir(), "missing-mbsync-config"), "home")
	// With the default binary, the wrong config path must produce an error
	// that mentions mbsync, fast.
	if err == nil {
		t.Fatal("Sync with defaults unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("default timeout applied lazily and took too long (%v)", elapsed)
	}
}
