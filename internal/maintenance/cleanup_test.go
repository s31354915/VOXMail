package maintenance

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupEphemeralRemovesOnlyKnownOldArtifacts(t *testing.T) {
	root := t.TempDir()
	recordings := filepath.Join(root, "recordings")
	if err := os.MkdirAll(recordings, 0700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(recordings, ".recording-old.pcm")
	newFile := filepath.Join(recordings, ".recording-new.pcm")

	for _, path := range []string{old, newFile, filepath.Join(recordings, "user-upload.wav")} {
		if err := os.WriteFile(path, []byte("audio"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if err := os.Chtimes(old, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	report, err := CleanupEphemeral(root, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if report.RemovedFiles != 1 {
		t.Fatalf("removed files=%d, want 1", report.RemovedFiles)
	}
	for _, path := range []string{old} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("old artifact still exists: %s", path)
		}
	}
	for _, path := range []string{newFile, filepath.Join(recordings, "user-upload.wav")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unrelated/new file removed: %s: %v", path, err)
		}
	}
}

func TestCleanupEphemeralDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	recordings := filepath.Join(root, "recordings")
	if err := os.MkdirAll(recordings, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(recordings, ".recording-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := CleanupEphemeral(root, time.Now().Add(48*time.Hour), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("symlink target was affected: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink should be left for operator review: %v", err)
	}
}
