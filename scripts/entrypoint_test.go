//go:build linux

package scripts

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Run the actual entrypoint with only its absolute installation paths mapped
// into a disposable fixture. The child deliberately holds shutdown open until
// the test releases it; this reproduces the interrupted-shell-wait defect.
func TestEntrypointWaitsForApplicationShutdown(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			root := t.TempDir()
			data := filepath.Join(root, "data")
			share := filepath.Join(root, "share")
			if err := os.MkdirAll(share, 0700); err != nil {
				t.Fatal(err)
			}
			for _, asset := range []string{"welcome.wav", "main-menu.wav", "static-prompts.json"} {
				if err := os.WriteFile(filepath.Join(share, asset), []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(data, "run"), 0700); err != nil {
				t.Fatal(err)
			}
			// The old PID is intentionally this still-live test process. Startup
			// must remove the obsolete file without using it as process identity.
			pidPath := filepath.Join(data, "run", "voxmail.pid")
			if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := filepath.Join(root, "child")
			quoted := "'" + strings.ReplaceAll(exe, "'", "'\"'\"'") + "'"
			if err := os.WriteFile(child, []byte("#!/bin/sh\nexec "+quoted+" -test.run=^TestEntrypointShutdownChild$\n"), 0700); err != nil {
				t.Fatal(err)
			}
			source, err := os.ReadFile("entrypoint.sh")
			if err != nil {
				t.Fatal(err)
			}
			mapped := strings.NewReplacer("/usr/local/bin/voxmail", child, "/usr/local/share/voxmail", share, "/data", data).Replace(string(source))
			entrypoint := filepath.Join(root, "entrypoint.sh")
			if err := os.WriteFile(entrypoint, []byte(mapped), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", entrypoint)
			cmd.Env = append(os.Environ(), "VOXMAIL_ENCRYPTION_KEY=entrypoint-test-key", "VOXMAIL_ENTRYPOINT_TEST_ROOT="+root)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			ready := waitEntrypointFile(t, filepath.Join(root, "ready"))
			if string(ready) != strconv.Itoa(cmd.Process.Pid) {
				t.Errorf("application PID=%s, entrypoint PID=%d; shell did not exec", ready, cmd.Process.Pid)
			}
			if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
				t.Errorf("obsolete PID file remains: %v", err)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			waitEntrypointFile(t, filepath.Join(root, "stopping"))
			select {
			case err := <-done:
				t.Fatalf("entrypoint exited before application shutdown completed: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 23 {
					t.Fatalf("application exit status was not preserved: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("entrypoint did not exit after application shutdown")
			}
			waitEntrypointFile(t, filepath.Join(root, "stopped"))
		})
	}
}

func TestEntrypointShutdownChild(t *testing.T) {
	root := os.Getenv("VOXMAIL_ENTRYPOINT_TEST_ROOT")
	if root == "" {
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	// Publish readiness only after the PID payload is complete. WriteFile
	// creates the pathname before writing, so existence alone was a race.
	if err := os.WriteFile(filepath.Join(root, "ready.tmp"), []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
		os.Exit(90)
	}
	if err := os.Rename(filepath.Join(root, "ready.tmp"), filepath.Join(root, "ready")); err != nil {
		os.Exit(90)
	}
	<-signals
	if err := os.WriteFile(filepath.Join(root, "stopping"), nil, 0600); err != nil {
		os.Exit(91)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "release")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(root, "stopped"), nil, 0600); err != nil {
		os.Exit(92)
	}
	os.Exit(23)
}

func waitEntrypointFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", filepath.Base(path))
	return nil
}
