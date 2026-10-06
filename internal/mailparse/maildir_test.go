package mailparse

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestScanMetadataFilterCanSkipOpeningAFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Inbox", "new")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "malformed")
	if err := os.WriteFile(path, []byte("not a message"), 0600); err != nil {
		t.Fatal(err)
	}
	filtered := false
	visited := false
	err := ScanMetadataEachContextWithFilter(context.Background(), filepath.Dir(filepath.Dir(root)), func(gotPath string, info os.FileInfo) (bool, error) {
		filtered = gotPath == path && info.Size() > 0
		return false, nil
	}, func(MaildirMessage) error {
		visited = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !filtered {
		t.Fatal("file filter did not observe the Maildir file")
	}
	if visited {
		t.Fatal("filtered file was parsed and visited")
	}
}
