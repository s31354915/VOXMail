package mailsync

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

func TestSyncChangeAndErrorBitsRemainAnError(t *testing.T) {
	for _, exitCode := range []int{33, 65, 97} {
		t.Run(strconv.Itoa(exitCode), func(t *testing.T) {
			r := newRunner(fakeMbsync(t, exitCode, ""))
			result, err := r.Sync(context.Background(), "config", "home")
			if err == nil {
				t.Fatalf("exit %d should surface an error", exitCode)
			}
			if !result.Changed {
				t.Fatalf("exit %d should retain its change bits", exitCode)
			}
		})
	}
}

func TestSyncRequiresChannel(t *testing.T) {
	r := newRunner(fakeMbsync(t, 0, ""))
	if _, err := r.Sync(context.Background(), "config", ""); err == nil {
		t.Fatal("empty channel accepted")
	}
}

func TestSyncChannelsPassesEveryChannel(t *testing.T) {
	r := newRunner(fakeMbsync(t, 32, ""))
	result, err := r.SyncChannels(context.Background(), "config", "one", "two")
	if err != nil {
		t.Fatalf("SyncChannels: %v", err)
	}
	if !result.Changed || result.Account != "one,two" {
		t.Fatalf("result=%+v", result)
	}
}

func TestValidateUsesMbsyncDryRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is unix-only")
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	shim := filepath.Join(dir, "mbsync")
	content := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsPath + "'\nexit 0\n"
	if err := os.WriteFile(shim, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	if err := (Runner{Binary: shim}).Validate(context.Background(), "voxmail.conf", "one", "two"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "--dry-run\n--config\nvoxmail.conf\n--ext-exit\none\ntwo\n"
	if string(data) != want {
		t.Fatalf("args = %q, want %q", data, want)
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

func TestSyncSignalExitIsFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is unix-only")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "mbsync")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nkill -TERM $$\n"), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := (Runner{Binary: shim}).Sync(context.Background(), "config", "home")
	if err == nil {
		t.Fatal("signal termination was reported as success")
	}
	if result.Changed {
		t.Fatal("signal termination was reported as a change")
	}
}

func TestSyncBoundsSubprocessOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is unix-only")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "mbsync")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%02097152d' 0\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := (Runner{Binary: shim}).Sync(context.Background(), "config", "home")
	if err == nil {
		t.Fatal("expected non-zero mbsync status")
	}
	if len(result.Output) > maxMbsyncOutput+64 {
		t.Fatalf("captured output=%d bytes, cap=%d", len(result.Output), maxMbsyncOutput)
	}
	if !strings.Contains(string(result.Output), "output truncated") {
		t.Fatalf("truncation marker missing; captured %d bytes", len(result.Output))
	}
}

func TestSyncDefaultBinaryAndTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is unix-only")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "mbsync")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	started := time.Now()
	_, err := (Runner{}).Sync(context.Background(), "config", "home")
	// The default binary name must be resolved from PATH and a non-zero exit
	// must be reported without depending on whether the host has real mbsync.
	if err == nil {
		t.Fatal("Sync with defaults unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("default timeout applied lazily and took too long (%v)", elapsed)
	}
}
